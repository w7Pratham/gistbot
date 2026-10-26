// Package bot implements Zero Two's owner-only Telegram command surface and
// delivers finished jobs back to the owner.
//
// The poller already drops updates that do not come from the owner, but Handle
// repeats that gate so the bot is safe to call from any caller. Every reply is
// HTML-escaped and bounded with telegram.EscapeAndTruncate; no user-supplied
// text is ever interpolated into markup unescaped.
package bot

import (
	"context"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"gistbot/internal/config"
	"gistbot/internal/jobs"
	"gistbot/internal/repo"
	"gistbot/internal/state"
	"gistbot/internal/telegram"
)

const (
	// maxMessageUTF16 mirrors Telegram's message limit in UTF-16 code units.
	maxMessageUTF16 = 4096
	// preOverhead is the cost of the surrounding <pre> and </pre> tags.
	preOverhead = len("<pre>") + len("</pre>")
	// docSendDelay paces consecutive fallback document sends. It is injected
	// so tests never wait.
	docSendDelay = time.Second
	// maxDocBytes caps each uploaded gist document at 1 MiB.
	maxDocBytes = 1 << 20
	// logTailBudget bounds the escaped log tail inside its <pre> block.
	logTailBudget = 2800
	// maxTailLines is how many log lines a failure notification includes.
	maxTailLines = 10
	// maxLogAttachmentBytes caps the full .log attachment at 5 MiB.
	maxLogAttachmentBytes = 5 << 20
	// startEditTimeout bounds the start-card edit because the manager calls
	// NotifyStarted synchronously on its worker with the root context.
	startEditTimeout = 5 * time.Second
	// cardFieldBudget bounds one escaped variable field in a card so several
	// fields can never push the message over Telegram's limit.
	cardFieldBudget = 512
)

// ModelLister returns the model references installed in OpenCode. Task 14
// consumes it for /model; Task 12 only stores it.
type ModelLister func(ctx context.Context) ([]string, error)

// Sender is the Telegram surface the bot writes through. *telegram.Client
// implements it.
type Sender interface {
	SendMessage(ctx context.Context, chatID int64, html string) error
	SendMessageID(ctx context.Context, chatID int64, html string) (int64, error)
	EditMessageText(ctx context.Context, chatID, messageID int64, html string) error
	SendDocument(ctx context.Context, chatID int64, filename string, data []byte) error
	SendMediaGroup(ctx context.Context, chatID int64, docs []telegram.Document) error
}

// Jobs is the job-manager surface the bot consumes. *jobs.Manager implements
// it.
type Jobs interface {
	Enqueue(jobs.Request) (position int, err error)
	Status() jobs.Status
	Cancel(ctx context.Context) bool
}

// Bot routes owner messages to commands and delivers finished jobs. It is safe
// for concurrent use after construction.
type Bot struct {
	cfg        config.Config
	state      *state.Store
	jobs       Jobs
	sender     Sender
	ownerID    int64
	listModels ModelLister
	log        *slog.Logger

	now   func() time.Time
	sleep func(context.Context, time.Duration) error
	seq   atomic.Uint64

	cardsMu sync.Mutex
	cards   map[string]int64
}

// New returns an owner-only bot. A nil logger falls back to slog.Default. The
// owner ID from the configuration is used when the explicit ownerID is not
// positive.
func New(cfg config.Config, st *state.Store, mgr Jobs, sender Sender,
	ownerID int64, listModels ModelLister, log *slog.Logger) *Bot {
	if log == nil {
		log = slog.Default()
	}
	if ownerID <= 0 {
		ownerID = cfg.OwnerUserID
	}
	return &Bot{
		cfg:        cfg,
		state:      st,
		jobs:       mgr,
		sender:     sender,
		ownerID:    ownerID,
		listModels: listModels,
		log:        log,
		now:        time.Now,
		sleep:      sleepContext,
		cards:      make(map[string]int64),
	}
}

// Handle re-checks the owner/private gate and dispatches one Telegram update.
func (b *Bot) Handle(ctx context.Context, u telegram.Update) {
	message := u.Message
	if message == nil || message.From == nil || message.From.IsBot {
		return
	}
	if message.Chat.Type != "private" || message.From.ID != b.ownerID {
		return
	}
	if ctx == nil {
		ctx = context.Background()
	}

	command, args, ok := parseCommand(message.Text)
	if !ok {
		return
	}
	chatID := message.Chat.ID

	switch command {
	case "start", "help":
		b.handleHelp(ctx, chatID)
	case "gist":
		b.handleGist(ctx, chatID, args)
	case "status":
		b.handleStatus(ctx, chatID)
	case "cancel":
		b.handleCancel(ctx, chatID)
	case "list":
		b.handleList(ctx, chatID)
	case "show":
		b.handleShow(ctx, chatID, args)
	case "log":
		b.handleLog(ctx, chatID, args)
	case "model":
		b.handleModel(ctx, chatID, args)
	case "runner":
		b.handleRunner(ctx, chatID, args)
	default:
		b.reply(ctx, chatID, "Unknown command. Use /help.")
	}
}

// NotifyStarted edits a queued job card to Running when the manager begins the
// job. The manager calls this synchronously on its worker with the root
// context, so the edit gets a short independent deadline. A missing card is
// normal: a position-0 initial card already says Running, and a final fallback
// covers a restart.
func (b *Bot) NotifyStarted(ctx context.Context, req jobs.Request) {
	if b.sender == nil || req.ID == "" || req.ChatID <= 0 {
		return
	}
	messageID, ok := b.cardID(req.ID)
	if !ok {
		return
	}
	if ctx == nil {
		ctx = context.Background()
	}
	editCtx, cancel := context.WithTimeout(ctx, startEditTimeout)
	defer cancel()

	if err := b.sender.EditMessageText(editCtx, req.ChatID, messageID, runningCardHTML(req, b.clock())); err != nil {
		b.logWarn("edit started job card", "job", req.ID, "error", err)
	}
}

// Notify delivers one finished job by editing its card in place. When the card
// is missing or no longer editable, it sends a new final card. Successful jobs
// then send one album (or one document); every other terminal status sends the
// job log attachment.
func (b *Bot) Notify(ctx context.Context, res jobs.Result) {
	if b.sender == nil {
		return
	}
	chatID := res.Request.ChatID
	if chatID <= 0 {
		return
	}
	if ctx == nil {
		ctx = context.Background()
	}

	if res.Status == state.StatusSucceeded {
		b.finishCard(ctx, res, successCardHTML(res))
		b.sendDocAlbum(ctx, chatID, res.Docs)
		return
	}
	b.finishCard(ctx, res, failureCardHTML(res, b.logTail(res)))
	b.sendLogFile(ctx, chatID, res)
}

// finishCard edits the stored card, falling back to a new message when the card
// is missing or the edit fails. The map entry is always removed afterwards.
func (b *Bot) finishCard(ctx context.Context, res jobs.Result, html string) {
	jobID := res.Request.ID
	if messageID, ok := b.cardID(jobID); ok {
		err := b.sender.EditMessageText(ctx, res.Request.ChatID, messageID, html)
		if err == nil {
			b.forgetCard(jobID)
			return
		}
		b.logWarn("edit finished job card", "job", jobID, "error", err)
	}

	if _, err := b.sender.SendMessageID(ctx, res.Request.ChatID, html); err != nil {
		b.logWarn("send finished job card", "job", jobID, "error", err)
	}
	b.forgetCard(jobID)
}

// sendDocAlbum uploads the present documents in canonical order: one document
// is sent alone, two or three become one album, and a rejected album falls back
// to paced separate documents.
func (b *Bot) sendDocAlbum(ctx context.Context, chatID int64, docs repo.Docs) {
	names := presentDocNames(docs.Files)
	if len(names) == 0 {
		return
	}

	attachments := make([]telegram.Document, 0, len(names))
	for _, name := range names {
		data, err := readDoc(docs.Files[name])
		if err != nil {
			b.logWarn("read gist document", "doc", name, "error", err)
			continue
		}
		attachments = append(attachments, telegram.Document{Filename: name + ".md", Data: data})
	}
	if len(attachments) == 0 {
		return
	}

	if len(attachments) == 1 {
		if err := b.sender.SendDocument(ctx, chatID, attachments[0].Filename, attachments[0].Data); err != nil {
			b.logWarn("send gist document", "doc", attachments[0].Filename, "error", err)
		}
		return
	}

	if len(attachments) <= 3 {
		err := b.sender.SendMediaGroup(ctx, chatID, attachments)
		if err == nil {
			return
		}
		b.logWarn("send gist album", "error", err)
	}

	for _, doc := range attachments {
		if err := b.wait(ctx, docSendDelay); err != nil {
			b.logWarn("wait before sending document", "error", err)
			return
		}
		if err := b.sender.SendDocument(ctx, chatID, doc.Filename, doc.Data); err != nil {
			b.logWarn("send gist document", "doc", doc.Filename, "error", err)
		}
	}
}

// sendDocuments uploads documents separately with the historical one-second
// pacing. The browse view still uses this; the job notification uses
// sendDocAlbum.
func (b *Bot) sendDocuments(ctx context.Context, chatID int64, docs repo.Docs) {
	for _, name := range presentDocNames(docs.Files) {
		if err := b.wait(ctx, docSendDelay); err != nil {
			b.logWarn("wait before sending document", "error", err)
			return
		}
		data, err := readDoc(docs.Files[name])
		if err != nil {
			b.logWarn("read gist document", "doc", name, "error", err)
			continue
		}
		if err := b.sender.SendDocument(ctx, chatID, name+".md", data); err != nil {
			b.logWarn("send gist document", "doc", name, "error", err)
		}
	}
}

// sendLogFile attaches the job log, capped to its most recent 5 MiB.
func (b *Bot) sendLogFile(ctx context.Context, chatID int64, res jobs.Result) {
	if res.LogPath == "" {
		return
	}
	data, truncated, err := readLogAttachment(res.LogPath)
	if err != nil {
		b.logWarn("read job log attachment", "job", res.Request.ID, "error", err)
		return
	}
	if truncated {
		data = append([]byte(logTruncatedMarker), data...)
	}
	filename := res.Request.ID + ".log"
	if res.Request.ID == "" {
		filename = "job.log"
	}
	if err := b.sender.SendDocument(ctx, chatID, filename, data); err != nil {
		b.logWarn("send job log attachment", "job", res.Request.ID, "error", err)
	}
}

// storeCard remembers the message ID of a job's card.
func (b *Bot) storeCard(jobID string, messageID int64) {
	if jobID == "" || messageID <= 0 {
		return
	}
	b.cardsMu.Lock()
	defer b.cardsMu.Unlock()
	if b.cards == nil {
		b.cards = make(map[string]int64)
	}
	b.cards[jobID] = messageID
}

// cardID returns the stored message ID for a job.
func (b *Bot) cardID(jobID string) (int64, bool) {
	b.cardsMu.Lock()
	defer b.cardsMu.Unlock()
	messageID, ok := b.cards[jobID]
	return messageID, ok
}

// forgetCard drops the card mapping after a terminal card is handled.
func (b *Bot) forgetCard(jobID string) {
	b.cardsMu.Lock()
	defer b.cardsMu.Unlock()
	delete(b.cards, jobID)
}

// logTail reads the last maxTailLines lines of the job log. A missing or
// unreadable log yields an empty tail so the summary is still delivered.
func (b *Bot) logTail(res jobs.Result) string {
	if res.LogPath == "" {
		return ""
	}
	tail, err := tailFile(res.LogPath, maxTailLines)
	if err != nil {
		b.logWarn("read job log", "job", res.Request.ID, "error", err)
		return ""
	}
	return tail
}

// send delivers one already-escaped HTML message. Send failures are logged and
// never surfaced to the chat.
func (b *Bot) send(ctx context.Context, chatID int64, html string) {
	if b.sender == nil || html == "" {
		return
	}
	if err := b.sender.SendMessage(ctx, chatID, html); err != nil {
		b.logWarn("send telegram message", "error", err)
	}
}

// reply sends plain text after escaping and truncating it.
func (b *Bot) reply(ctx context.Context, chatID int64, text string) {
	b.send(ctx, chatID, telegram.EscapeAndTruncate(text, maxMessageUTF16))
}

// replyPre sends plain text inside a <pre> block. Only the content is escaped;
// the tags are constants.
func (b *Bot) replyPre(ctx context.Context, chatID int64, text string) {
	b.send(ctx, chatID, preBlock(telegram.EscapeAndTruncate(text, maxMessageUTF16-preOverhead)))
}

// wait uses the injected sleep when present.
func (b *Bot) wait(ctx context.Context, d time.Duration) error {
	if b.sleep == nil {
		return sleepContext(ctx, d)
	}
	return b.sleep(ctx, d)
}

// clock uses the injected clock when present.
func (b *Bot) clock() time.Time {
	if b.now == nil {
		return time.Now()
	}
	return b.now()
}

func (b *Bot) logWarn(msg string, args ...any) {
	if b.log != nil {
		b.log.Warn(msg, args...)
	}
}

// sleepContext waits for d or until ctx is cancelled.
func sleepContext(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return nil
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}
