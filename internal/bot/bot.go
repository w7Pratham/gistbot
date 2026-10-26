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
	// docSendDelay paces consecutive Telegram sends. It is injected so tests
	// never wait.
	docSendDelay = time.Second
	// maxDocBytes caps each uploaded gist document at 1 MiB.
	maxDocBytes = 1 << 20
	// failureSummaryBudget leaves room for the log tail in the same message.
	failureSummaryBudget = 1200
	// logTailBudget bounds the escaped log tail inside its <pre> block.
	logTailBudget = 2800
	// maxTailLines is how many log lines a failure notification includes.
	maxTailLines = 20
)

// ModelLister returns the model references installed in OpenCode. Task 14
// consumes it for /model; Task 12 only stores it.
type ModelLister func(ctx context.Context) ([]string, error)

// Sender is the Telegram surface the bot writes through. *telegram.Client
// implements it.
type Sender interface {
	SendMessage(ctx context.Context, chatID int64, html string) error
	SendDocument(ctx context.Context, chatID int64, filename string, data []byte) error
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

// Notify delivers one finished job. Successful jobs send a summary and then
// each available document; every other terminal status sends a short summary
// plus the tail of the job log.
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
		b.send(ctx, chatID, successHTML(res))
		b.sendDocuments(ctx, chatID, res.Docs)
		return
	}
	b.send(ctx, chatID, failureHTML(res, b.logTail(res)))
}

// sendDocuments uploads the present documents in canonical order, pausing
// docSendDelay before each upload.
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
