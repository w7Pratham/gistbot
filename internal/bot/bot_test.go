package bot

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"gistbot/internal/config"
	"gistbot/internal/jobs"
	"gistbot/internal/repo"
	"gistbot/internal/state"
	"gistbot/internal/telegram"
)

const (
	testOwnerID int64 = 7
	testChatID  int64 = 7
	testJobTime       = 90 * time.Second
)

var fixedNow = time.Date(2026, time.January, 2, 3, 4, 5, 0, time.UTC)

// messageSend, editSend, mediaGroupSend, and documentSend record one fake
// Telegram call each.
type messageSend struct {
	chatID int64
	html   string
	id     int64
}

type editSend struct {
	chatID    int64
	messageID int64
	html      string
}

type mediaGroupSend struct {
	chatID int64
	docs   []telegram.Document
}

type documentSend struct {
	chatID   int64
	filename string
	data     []byte
}

// fakeSender records outgoing Telegram traffic by method so card edits, albums,
// and single documents can be asserted independently and in order.
type fakeSender struct {
	mu        sync.Mutex
	messages  []messageSend
	edits     []editSend
	groups    []mediaGroupSend
	documents []documentSend
	events    []string

	messageErr    error
	editErr       error
	mediaGroupErr error
	documentErr   error

	nextMessageID   int64
	editHasDeadline []bool
}

func (s *fakeSender) SendMessage(ctx context.Context, chatID int64, html string) error {
	_, err := s.SendMessageID(ctx, chatID, html)
	return err
}

func (s *fakeSender) SendMessageID(_ context.Context, chatID int64, html string) (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.nextMessageID++
	id := s.nextMessageID
	s.messages = append(s.messages, messageSend{chatID: chatID, html: html, id: id})
	s.events = append(s.events, "sendMessageID")
	return id, s.messageErr
}

func (s *fakeSender) EditMessageText(ctx context.Context, chatID, messageID int64, html string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, hasDeadline := ctx.Deadline()
	s.editHasDeadline = append(s.editHasDeadline, hasDeadline)
	s.edits = append(s.edits, editSend{chatID: chatID, messageID: messageID, html: html})
	s.events = append(s.events, "editMessageText")
	return s.editErr
}

func (s *fakeSender) SendMediaGroup(_ context.Context, chatID int64, docs []telegram.Document) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	copied := make([]telegram.Document, len(docs))
	for i, doc := range docs {
		copied[i] = telegram.Document{
			Filename: doc.Filename,
			Data:     append([]byte(nil), doc.Data...),
			Caption:  doc.Caption,
		}
	}
	s.groups = append(s.groups, mediaGroupSend{chatID: chatID, docs: copied})
	s.events = append(s.events, "sendMediaGroup")
	return s.mediaGroupErr
}

func (s *fakeSender) SendDocument(_ context.Context, chatID int64, filename string, data []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	kept := make([]byte, len(data))
	copy(kept, data)
	s.documents = append(s.documents, documentSend{chatID: chatID, filename: filename, data: kept})
	s.events = append(s.events, "sendDocument:"+filename)
	return s.documentErr
}

func (s *fakeSender) messageTexts() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	texts := make([]string, len(s.messages))
	for i := range s.messages {
		texts[i] = s.messages[i].html
	}
	return texts
}

func (s *fakeSender) sentMessages() []messageSend {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]messageSend(nil), s.messages...)
}

func (s *fakeSender) sentEdits() []editSend {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]editSend(nil), s.edits...)
}

func (s *fakeSender) sentGroups() []mediaGroupSend {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]mediaGroupSend(nil), s.groups...)
}

func (s *fakeSender) sentDocuments() []documentSend {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]documentSend(nil), s.documents...)
}

func (s *fakeSender) eventLog() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.events...)
}

func (s *fakeSender) editsHadDeadline() []bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]bool(nil), s.editHasDeadline...)
}

// fakeJobs records enqueue, status, and cancel calls.
type fakeJobs struct {
	mu           sync.Mutex
	requests     []jobs.Request
	position     int
	enqueueErr   error
	status       jobs.Status
	cancelResult bool
	cancelCalls  int
}

func (j *fakeJobs) Enqueue(req jobs.Request) (int, error) {
	j.mu.Lock()
	defer j.mu.Unlock()
	j.requests = append(j.requests, req)
	return j.position, j.enqueueErr
}

func (j *fakeJobs) Status() jobs.Status {
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.status
}

func (j *fakeJobs) Cancel(context.Context) bool {
	j.mu.Lock()
	defer j.mu.Unlock()
	j.cancelCalls++
	return j.cancelResult
}

func (j *fakeJobs) requestList() []jobs.Request {
	j.mu.Lock()
	defer j.mu.Unlock()
	return append([]jobs.Request(nil), j.requests...)
}

func (j *fakeJobs) cancelCount() int {
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.cancelCalls
}

// botFixture wires a Bot to fakes backed by a real, temporary state store.
type botFixture struct {
	t      *testing.T
	cfg    config.Config
	store  *state.Store
	jobs   *fakeJobs
	sender *fakeSender
	bot    *Bot
	sleeps []time.Duration
}

func newBotFixture(t *testing.T) *botFixture {
	t.Helper()

	root := t.TempDir()
	cfg := config.Config{
		OwnerUserID: testOwnerID,
		WorkDir:     filepath.Join(root, "work"),
		LogDir:      filepath.Join(root, "logs"),
		StateFile:   filepath.Join(root, "state.json"),
		Models: map[string]config.ModelRef{
			"free":  {ProviderID: "test", ModelID: "free"},
			"flash": {ProviderID: "test", ModelID: "flash"},
			"pro":   {ProviderID: "test", ModelID: "pro", Variant: "high"},
		},
		DefaultModel:  "flash",
		DefaultRunner: "cli",
		MaxQueue:      3,
		JobTimeout:    time.Minute,
	}
	if err := os.MkdirAll(cfg.WorkDir, 0o700); err != nil {
		t.Fatalf("create work directory: %v", err)
	}

	store, err := state.Open(cfg.StateFile, state.Data{Runner: "cli", Model: "flash"})
	if err != nil {
		t.Fatalf("open state: %v", err)
	}

	f := &botFixture{
		t:      t,
		cfg:    cfg,
		store:  store,
		jobs:   &fakeJobs{},
		sender: &fakeSender{},
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	f.bot = New(cfg, store, f.jobs, f.sender, testOwnerID, func(context.Context) ([]string, error) {
		return []string{"test/flash", "test/pro"}, nil
	}, logger)
	f.bot.now = func() time.Time { return fixedNow }
	f.bot.sleep = func(_ context.Context, d time.Duration) error {
		f.sleeps = append(f.sleeps, d)
		return nil
	}
	return f
}

func (f *botFixture) handle(update telegram.Update) {
	f.t.Helper()
	f.bot.Handle(context.Background(), update)
}

func (f *botFixture) setRunner(t *testing.T, runner string) {
	t.Helper()
	if err := f.store.SetRunner(runner); err != nil {
		t.Fatalf("SetRunner(%q): %v", runner, err)
	}
}

func (f *botFixture) setModel(t *testing.T, alias string) {
	t.Helper()
	if err := f.store.SetModel(alias); err != nil {
		t.Fatalf("SetModel(%q): %v", alias, err)
	}
}

// enqueueGist runs /gist and returns the request the fake manager accepted.
func (f *botFixture) enqueueGist(url string) jobs.Request {
	f.t.Helper()
	f.handle(privateUpdate("/gist " + url))
	requests := f.jobs.requestList()
	if len(requests) == 0 {
		f.t.Fatal("no request was enqueued")
	}
	return requests[len(requests)-1]
}

// plainCard removes the small markup used by cards so the approved text shape
// can be asserted directly.
func plainCard(html string) string {
	return strings.NewReplacer(
		"<b>", "",
		"</b>", "",
		"<code>", "",
		"</code>", "",
		"<pre>", "",
		"</pre>", "",
	).Replace(html)
}

func privateUpdate(text string) telegram.Update {
	return updateFrom(testOwnerID, testChatID, "private", text)
}

func updateFrom(fromID, chatID int64, chatType, text string) telegram.Update {
	return telegram.Update{
		UpdateID: 1,
		Message: &telegram.Message{
			MessageID: 1,
			From:      &telegram.User{ID: fromID, FirstName: "Owner"},
			Chat:      telegram.Chat{ID: chatID, Type: chatType},
			Text:      text,
		},
	}
}

func requireContains(t *testing.T, text, want string) {
	t.Helper()
	if !strings.Contains(text, want) {
		t.Fatalf("text %q does not contain %q", text, want)
	}
}

func requireNotContains(t *testing.T, text, unwanted string) {
	t.Helper()
	if strings.Contains(text, unwanted) {
		t.Fatalf("text %q unexpectedly contains %q", text, unwanted)
	}
}

// writeGistDocs writes docs/gist/<name>.md under a fresh repository and
// returns the discovered documents.
func writeGistDocs(t *testing.T, bodies map[string]string) repo.Docs {
	t.Helper()
	root := t.TempDir()
	for name, body := range bodies {
		path := filepath.Join(root, "docs", "gist", name+".md")
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatalf("create docs directory: %v", err)
		}
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatalf("write %s: %v", path, err)
		}
	}
	docs, err := repo.DiscoverDocs(root)
	if err != nil {
		t.Fatalf("DiscoverDocs: %v", err)
	}
	return docs
}

func testResult(docs repo.Docs) jobs.Result {
	target, err := repo.ParseGitHubURL("https://github.com/example/project")
	if err != nil {
		panic(err)
	}
	return jobs.Result{
		Request: jobs.Request{
			ID:         "job-1",
			Target:     target,
			ChatID:     testChatID,
			Runner:     jobs.RunnerCLI,
			Model:      config.ModelRef{ProviderID: "test", ModelID: "flash"},
			ModelAlias: "flash",
			EnqueuedAt: fixedNow,
		},
		Status:     state.StatusSucceeded,
		Docs:       docs,
		StartedAt:  fixedNow,
		FinishedAt: fixedNow.Add(testJobTime),
	}
}

func TestIgnoresNonOwnerGroupAndChannel(t *testing.T) {
	botUpdate := privateUpdate("/status")
	botUpdate.Message.From.IsBot = true

	cases := []struct {
		name   string
		update telegram.Update
	}{
		{name: "non-owner", update: updateFrom(8, testChatID, "private", "/status")},
		{name: "group", update: updateFrom(testOwnerID, -100, "group", "/status")},
		{name: "supergroup", update: updateFrom(testOwnerID, -100, "supergroup", "/status")},
		{name: "channel", update: updateFrom(testOwnerID, -100, "channel", "/status")},
		{name: "bot", update: botUpdate},
		{name: "no message", update: telegram.Update{UpdateID: 5}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newBotFixture(t)
			f.handle(tc.update)

			if got := len(f.sender.messageTexts()); got != 0 {
				t.Errorf("messages = %d, want 0", got)
			}
			if got := len(f.jobs.requestList()); got != 0 {
				t.Errorf("enqueued requests = %d, want 0", got)
			}
			if got := f.jobs.cancelCount(); got != 0 {
				t.Errorf("cancel calls = %d, want 0", got)
			}
		})
	}
}

func TestHelpIsCompactAndHasSettingsFooter(t *testing.T) {
	f := newBotFixture(t)
	f.handle(privateUpdate("/help@gistbot"))

	texts := f.sender.messageTexts()
	if len(texts) != 1 {
		t.Fatalf("messages = %d, want 1", len(texts))
	}
	text := texts[0]

	requireNotContains(t, text, "<pre>")
	requireContains(t, text, "<b>Gistbot</b> · commands")
	requireContains(t, text, "<b>")

	for _, command := range []string{
		"gist", "status", "cancel", "list", "show", "log", "model", "runner", "help",
	} {
		requireContains(t, text, "<code>/"+command)
	}
	if got := strings.Count(text, "flash · cli · idle"); got != 1 {
		t.Errorf("footer appears %d times, want exactly once:\n%s", got, text)
	}

	repoTarget, err := repo.ParseGitHubURL("https://github.com/example/project")
	if err != nil {
		t.Fatalf("ParseGitHubURL: %v", err)
	}
	f.jobs.status = jobs.Status{Running: &jobs.JobView{
		ID:        "job-9",
		RepoID:    repoTarget.ID,
		Model:     "flash",
		Runner:    "cli",
		StartedAt: fixedNow,
		Elapsed:   testJobTime,
	}}
	f.handle(privateUpdate("/help"))

	texts = f.sender.messageTexts()
	if len(texts) != 2 {
		t.Fatalf("messages = %d, want 2", len(texts))
	}
	requireContains(t, texts[1], "flash · cli · running example/project")
	if got := strings.Count(texts[1], "running example/project"); got != 1 {
		t.Errorf("job state appears %d times, want once:\n%s", got, texts[1])
	}
}

func TestStartIsShortAndFriendly(t *testing.T) {
	f := newBotFixture(t)

	f.handle(privateUpdate("/start"))

	texts := f.sender.messageTexts()
	if len(texts) != 1 {
		t.Fatalf("messages = %d, want 1", len(texts))
	}
	text := texts[0]

	requireContains(t, text, "<b>Gistbot</b>")
	requireContains(t, text, "GitHub")
	requireContains(t, text, "<code>/gist")
	requireContains(t, text, "<code>/help")
	requireContains(t, text, "flash · cli · idle")
	requireNotContains(t, text, "<pre>")
	if got := utf16Count(text); got > 600 {
		t.Errorf("start text = %d UTF-16 units, want a short page", got)
	}
}

func TestGistValidEnqueuesWithRunnerAndModel(t *testing.T) {
	t.Run("flash with sanitized focus", func(t *testing.T) {
		f := newBotFixture(t)
		f.setRunner(t, "api")

		f.handle(privateUpdate("/gist https://github.com/Example/Project.git fix\n  the\tauth \x07flow"))

		requests := f.jobs.requestList()
		if len(requests) != 1 {
			t.Fatalf("enqueued requests = %d, want 1", len(requests))
		}
		req := requests[0]

		if req.Target.URL != "https://github.com/Example/Project" {
			t.Errorf("Target.URL = %q", req.Target.URL)
		}
		if req.Target.Owner != "Example" || req.Target.Name != "Project" {
			t.Errorf("Target owner/name = %q/%q", req.Target.Owner, req.Target.Name)
		}
		if req.Target.ID != "github__example__project" {
			t.Errorf("Target.ID = %q", req.Target.ID)
		}
		if req.Focus != "fix the auth flow" {
			t.Errorf("Focus = %q, want %q", req.Focus, "fix the auth flow")
		}
		if req.ChatID != testChatID {
			t.Errorf("ChatID = %d, want %d", req.ChatID, testChatID)
		}
		if req.Runner != jobs.RunnerAPI {
			t.Errorf("Runner = %q, want api", req.Runner)
		}
		if req.Model != f.cfg.Models["flash"] {
			t.Errorf("Model = %v, want %v", req.Model, f.cfg.Models["flash"])
		}
		if req.ModelAlias != "flash" {
			t.Errorf("ModelAlias = %q, want flash", req.ModelAlias)
		}
		if !req.EnqueuedAt.Equal(fixedNow) {
			t.Errorf("EnqueuedAt = %v, want %v", req.EnqueuedAt, fixedNow)
		}
		wantID := fmt.Sprintf("job-%d-1", fixedNow.UnixNano())
		if req.ID != wantID {
			t.Errorf("ID = %q, want %q", req.ID, wantID)
		}

		texts := f.sender.messageTexts()
		if len(texts) != 1 {
			t.Fatalf("messages = %d, want 1", len(texts))
		}
		card := plainCard(texts[0])
		requireContains(t, card, "⚡ Running · Example/Project")
		requireContains(t, card, "flash · api")
	})

	t.Run("free override", func(t *testing.T) {
		f := newBotFixture(t)
		f.setModel(t, "free")
		if err := f.store.SetFreeModel("vendor/model#high"); err != nil {
			t.Fatalf("SetFreeModel: %v", err)
		}

		f.handle(privateUpdate("/gist https://github.com/example/project"))

		requests := f.jobs.requestList()
		if len(requests) != 1 {
			t.Fatalf("enqueued requests = %d, want 1", len(requests))
		}
		want := config.ModelRef{ProviderID: "vendor", ModelID: "model", Variant: "high"}
		if requests[0].Model != want {
			t.Errorf("Model = %v, want %v", requests[0].Model, want)
		}
		if requests[0].ModelAlias != "free" {
			t.Errorf("ModelAlias = %q, want free", requests[0].ModelAlias)
		}

		texts := f.sender.messageTexts()
		if len(texts) != 1 {
			t.Fatalf("messages = %d, want 1", len(texts))
		}
		requireContains(t, plainCard(texts[0]), "free (vendor/model#high)")
	})

	t.Run("queued position", func(t *testing.T) {
		f := newBotFixture(t)
		f.jobs.position = 2

		f.handle(privateUpdate("/gist https://github.com/example/other"))

		texts := f.sender.messageTexts()
		if len(texts) != 1 {
			t.Fatalf("messages = %d, want 1", len(texts))
		}
		card := plainCard(texts[0])
		requireContains(t, card, "🕓 Queued #2")
		requireContains(t, card, "example/other")
	})
}

func TestGistInvalidURLRepliesWithoutEnqueue(t *testing.T) {
	inputs := []struct {
		name string
		text string
	}{
		{name: "missing argument", text: "/gist"},
		{name: "wrong host", text: "/gist https://gitlab.com/a/b"},
		{name: "wrong scheme", text: "/gist http://github.com/a/b"},
		{name: "missing repo", text: "/gist https://github.com/a"},
		{name: "extra segment", text: "/gist https://github.com/a/b/extra"},
		{name: "not a url", text: "/gist not-a-url"},
	}

	for _, tc := range inputs {
		t.Run(tc.name, func(t *testing.T) {
			f := newBotFixture(t)
			f.handle(privateUpdate(tc.text))

			texts := f.sender.messageTexts()
			if len(texts) != 1 {
				t.Fatalf("messages = %d, want 1", len(texts))
			}
			requireContains(t, texts[0], "github.com")
			if got := len(f.jobs.requestList()); got != 0 {
				t.Errorf("enqueued requests = %d, want 0", got)
			}
			if tc.name == "wrong host" {
				requireNotContains(t, texts[0], "gitlab.com")
			}
		})
	}
}

func TestGistFocusOver200Rejected(t *testing.T) {
	f := newBotFixture(t)

	f.handle(privateUpdate("/gist https://github.com/example/project " + strings.Repeat("a", 200)))
	requests := f.jobs.requestList()
	if len(requests) != 1 {
		t.Fatalf("enqueued requests = %d, want 1 for a 200-rune focus", len(requests))
	}
	if got := len([]rune(requests[0].Focus)); got != 200 {
		t.Fatalf("focus length = %d, want 200", got)
	}

	f.handle(privateUpdate("/gist https://github.com/example/project " + strings.Repeat("b", 201)))
	if got := len(f.jobs.requestList()); got != 1 {
		t.Errorf("enqueued requests = %d, want 1 after oversized focus", got)
	}
	texts := f.sender.messageTexts()
	if len(texts) != 2 {
		t.Fatalf("messages = %d, want 2", len(texts))
	}
	requireContains(t, texts[1], "200")
	requireNotContains(t, texts[1], "Started")
}

func TestGistQueueFullReplies(t *testing.T) {
	f := newBotFixture(t)

	f.jobs.enqueueErr = jobs.ErrQueueFull
	f.handle(privateUpdate("/gist https://github.com/example/project"))
	texts := f.sender.messageTexts()
	if len(texts) != 1 {
		t.Fatalf("messages = %d, want 1", len(texts))
	}
	requireContains(t, texts[0], "Queue is full")

	f.jobs.enqueueErr = fmt.Errorf("jobs: persist queued job: %w", jobs.ErrQueueFull)
	f.handle(privateUpdate("/gist https://github.com/example/project"))
	texts = f.sender.messageTexts()
	if len(texts) != 2 {
		t.Fatalf("messages = %d, want 2", len(texts))
	}
	requireContains(t, texts[1], "Queue is full")

	f.jobs.enqueueErr = errors.New("jobs: persist queued job: disk unavailable")
	f.handle(privateUpdate("/gist https://github.com/example/project"))
	texts = f.sender.messageTexts()
	if len(texts) != 3 {
		t.Fatalf("messages = %d, want 3", len(texts))
	}
	requireContains(t, texts[2], "Could not queue")
	requireNotContains(t, texts[2], "disk unavailable")
}

func TestStatusIsJobAndQueueOnly(t *testing.T) {
	t.Run("running and queued", func(t *testing.T) {
		f := newBotFixture(t)
		f.jobs.status = jobs.Status{
			Running: &jobs.JobView{
				ID:        "job-9",
				RepoID:    "github__example__project",
				Model:     "flash",
				Runner:    "cli",
				StartedAt: fixedNow.Add(-testJobTime),
				Elapsed:   testJobTime,
			},
			Queued: []jobs.JobView{
				{ID: "job-10", RepoID: "github__other__repo", Model: "pro", Runner: "api"},
				{ID: "job-11", RepoID: "github__third__thing", Model: "flash", Runner: "cli"},
			},
		}

		f.handle(privateUpdate("/status"))

		texts := f.sender.messageTexts()
		if len(texts) != 1 {
			t.Fatalf("messages = %d, want 1", len(texts))
		}
		text := texts[0]
		for _, want := range []string{
			"Running", "example/project", "flash", "cli", "1m30s",
			"Queued", "#1", "other/repo", "#2", "third/thing", "pro", "api",
		} {
			requireContains(t, text, want)
		}
		for _, unwanted := range []string{"Model:", "Runner:", "Free override:", "<pre>"} {
			requireNotContains(t, text, unwanted)
		}
	})

	t.Run("nothing running", func(t *testing.T) {
		f := newBotFixture(t)
		f.handle(privateUpdate("/status"))

		texts := f.sender.messageTexts()
		if len(texts) != 1 {
			t.Fatalf("messages = %d, want 1", len(texts))
		}
		text := texts[0]
		requireContains(t, text, "no jobs running")
		requireNotContains(t, text, "Model:")
		requireNotContains(t, text, "Runner:")
		requireNotContains(t, text, "<pre>")
	})

	t.Run("settings are not repeated", func(t *testing.T) {
		f := newBotFixture(t)
		f.handle(privateUpdate("/model free test/pro#high"))
		f.handle(privateUpdate("/runner api"))

		f.handle(privateUpdate("/status"))

		text := f.lastMessage(t)
		for _, unwanted := range []string{
			"Model:", "Runner:", "Free override:", "test/pro#high", "api",
		} {
			requireNotContains(t, text, unwanted)
		}
		requireContains(t, text, "no jobs running")
	})
}

func TestCancelNoJob(t *testing.T) {
	f := newBotFixture(t)
	f.jobs.cancelResult = false

	f.handle(privateUpdate("/cancel"))

	texts := f.sender.messageTexts()
	if len(texts) != 1 {
		t.Fatalf("messages = %d, want 1", len(texts))
	}
	requireContains(t, texts[0], "No running job")
	if got := f.jobs.cancelCount(); got != 1 {
		t.Errorf("cancel calls = %d, want 1", got)
	}
}

func TestCancelRunning(t *testing.T) {
	f := newBotFixture(t)
	f.jobs.cancelResult = true

	f.handle(privateUpdate("/cancel"))

	texts := f.sender.messageTexts()
	if len(texts) != 1 {
		t.Fatalf("messages = %d, want 1", len(texts))
	}
	requireContains(t, texts[0], "Cancel requested")
	if got := f.jobs.cancelCount(); got != 1 {
		t.Errorf("cancel calls = %d, want 1", got)
	}
}

func TestGistQueuedCardIsStored(t *testing.T) {
	t.Run("queued", func(t *testing.T) {
		f := newBotFixture(t)
		f.jobs.position = 2

		req := f.enqueueGist("https://github.com/example/other")

		messages := f.sender.sentMessages()
		if len(messages) != 1 {
			t.Fatalf("messages = %d, want 1", len(messages))
		}
		want := "🕓 Queued #2 · example/other\nflash · cli"
		if got := plainCard(messages[0].html); got != want {
			t.Errorf("card = %q, want %q", got, want)
		}

		id, ok := f.bot.cardID(req.ID)
		if !ok {
			t.Fatalf("no card stored for %q", req.ID)
		}
		if id != messages[0].id {
			t.Errorf("stored card ID = %d, want %d", id, messages[0].id)
		}
	})

	t.Run("running", func(t *testing.T) {
		f := newBotFixture(t)

		req := f.enqueueGist("https://github.com/example/project")

		messages := f.sender.sentMessages()
		if len(messages) != 1 {
			t.Fatalf("messages = %d, want 1", len(messages))
		}
		want := "⚡ Running · example/project\nflash · cli · started " + fixedNow.Format("15:04")
		if got := plainCard(messages[0].html); got != want {
			t.Errorf("card = %q, want %q", got, want)
		}
		if id, ok := f.bot.cardID(req.ID); !ok || id != messages[0].id {
			t.Errorf("stored card = %d, %v; want %d, true", id, ok, messages[0].id)
		}
	})

	t.Run("send failure still queues", func(t *testing.T) {
		f := newBotFixture(t)
		f.sender.messageErr = errors.New("telegram unavailable")

		req := f.enqueueGist("https://github.com/example/project")

		if got := len(f.jobs.requestList()); got != 1 {
			t.Errorf("enqueued requests = %d, want 1", got)
		}
		if _, ok := f.bot.cardID(req.ID); ok {
			t.Error("a card ID was stored after a failed send")
		}
	})
}

func TestNotifyStartedEditsQueuedCardToRunning(t *testing.T) {
	f := newBotFixture(t)
	f.jobs.position = 2
	req := f.enqueueGist("https://github.com/example/other")

	f.bot.NotifyStarted(context.Background(), req)

	messages := f.sender.sentMessages()
	edits := f.sender.sentEdits()
	if len(edits) != 1 {
		t.Fatalf("edits = %d, want 1", len(edits))
	}
	if edits[0].chatID != testChatID {
		t.Errorf("edit chat ID = %d, want %d", edits[0].chatID, testChatID)
	}
	if edits[0].messageID != messages[0].id {
		t.Errorf("edited message = %d, want %d", edits[0].messageID, messages[0].id)
	}
	want := "⚡ Running · example/other\nflash · cli · started " + fixedNow.Format("15:04")
	if got := plainCard(edits[0].html); got != want {
		t.Errorf("card = %q, want %q", got, want)
	}
	if got := f.sender.editsHadDeadline(); len(got) != 1 || !got[0] {
		t.Errorf("start edit context deadlines = %v, want one bounded edit", got)
	}
	if got := len(f.sender.messageTexts()); got != 1 {
		t.Errorf("messages = %d, want the initial card only", got)
	}
}

func TestNotifyStartedWithoutCardDoesNothing(t *testing.T) {
	f := newBotFixture(t)

	f.bot.NotifyStarted(context.Background(), jobs.Request{
		ID:     "job-restart",
		ChatID: testChatID,
		Runner: jobs.RunnerCLI,
	})

	if got := len(f.sender.sentEdits()); got != 0 {
		t.Errorf("edits = %d, want 0", got)
	}
	if got := len(f.sender.messageTexts()); got != 0 {
		t.Errorf("messages = %d, want 0", got)
	}
}

func TestNotifySuccessEditsCardAndSendsAlbum(t *testing.T) {
	f := newBotFixture(t)
	req := f.enqueueGist("https://github.com/example/project")
	docs := writeGistDocs(t, map[string]string{
		"codemap":     "CODEMAP",
		"routing":     "ROUTING",
		"observation": "OBSERVATION",
	})

	res := testResult(docs)
	res.Request = req
	f.bot.Notify(context.Background(), res)

	messages := f.sender.sentMessages()
	if len(messages) != 1 {
		t.Fatalf("messages = %d, want the initial card only", len(messages))
	}
	edits := f.sender.sentEdits()
	if len(edits) != 1 {
		t.Fatalf("edits = %d, want 1", len(edits))
	}
	if edits[0].messageID != messages[0].id {
		t.Errorf("edited message = %d, want %d", edits[0].messageID, messages[0].id)
	}
	want := "✅ Gist ready · example/project\nflash · cli · 1m 30s · 3 docs"
	if got := plainCard(edits[0].html); got != want {
		t.Errorf("card = %q, want %q", got, want)
	}
	if _, ok := f.bot.cardID(req.ID); ok {
		t.Error("card entry was not removed after the final card")
	}

	groups := f.sender.sentGroups()
	if len(groups) != 1 {
		t.Fatalf("media groups = %d, want 1", len(groups))
	}
	if groups[0].chatID != testChatID {
		t.Errorf("album chat ID = %d, want %d", groups[0].chatID, testChatID)
	}
	wantNames := []string{"codemap.md", "routing.md", "observation.md"}
	wantBodies := []string{"CODEMAP", "ROUTING", "OBSERVATION"}
	if len(groups[0].docs) != len(wantNames) {
		t.Fatalf("album docs = %d, want %d", len(groups[0].docs), len(wantNames))
	}
	for i, doc := range groups[0].docs {
		if doc.Filename != wantNames[i] {
			t.Errorf("album[%d] filename = %q, want %q", i, doc.Filename, wantNames[i])
		}
		if string(doc.Data) != wantBodies[i] {
			t.Errorf("album[%d] body = %q, want %q", i, doc.Data, wantBodies[i])
		}
	}
	if got := len(f.sender.sentDocuments()); got != 0 {
		t.Errorf("separate documents = %d, want 0", got)
	}
	if len(f.sleeps) != 0 {
		t.Errorf("sleeps = %v, want none for one album", f.sleeps)
	}
	wantEvents := []string{"sendMessageID", "editMessageText", "sendMediaGroup"}
	if got := f.sender.eventLog(); !equalStrings(got, wantEvents) {
		t.Errorf("events = %v, want %v", got, wantEvents)
	}
}

func TestNotifyPartialSendsAlbumAndMissingFooter(t *testing.T) {
	f := newBotFixture(t)
	req := f.enqueueGist("https://github.com/example/project")
	docs := writeGistDocs(t, map[string]string{
		"codemap": "CODEMAP",
		"routing": "ROUTING",
	})

	res := testResult(docs)
	res.Request = req
	f.bot.Notify(context.Background(), res)

	edits := f.sender.sentEdits()
	if len(edits) != 1 {
		t.Fatalf("edits = %d, want 1", len(edits))
	}
	card := plainCard(edits[0].html)
	requireContains(t, card, "✅ Gist ready · example/project")
	requireContains(t, card, "flash · cli · 1m 30s · 2 docs")
	requireContains(t, card, "Missing: observation")

	groups := f.sender.sentGroups()
	if len(groups) != 1 {
		t.Fatalf("media groups = %d, want 1", len(groups))
	}
	if len(groups[0].docs) != 2 {
		t.Fatalf("album docs = %d, want 2", len(groups[0].docs))
	}
}

func TestNotifyFailureEditsCardAndSendsLogFile(t *testing.T) {
	f := newBotFixture(t)
	req := f.enqueueGist("https://github.com/example/project")

	var log strings.Builder
	for i := 1; i <= 30; i++ {
		switch i {
		case 29:
			fmt.Fprintf(&log, "\x1b[31mline-%02d\x1b[0m\r\n", i)
		default:
			fmt.Fprintf(&log, "line-%02d\n", i)
		}
	}
	logPath := filepath.Join(t.TempDir(), "job-1.log")
	if err := os.WriteFile(logPath, []byte(log.String()), 0o600); err != nil {
		t.Fatalf("write log: %v", err)
	}

	res := testResult(repo.Docs{})
	res.Request = req
	res.Status = state.StatusFailed
	res.Err = errors.New("clone failed")
	res.LogPath = logPath

	f.bot.Notify(context.Background(), res)

	edits := f.sender.sentEdits()
	if len(edits) != 1 {
		t.Fatalf("edits = %d, want 1", len(edits))
	}
	text := edits[0].html
	for _, want := range []string{
		"clone failed",
		"flash · cli · 1m 30s",
		"<pre>",
		"line-21",
		"line-29",
		"line-30",
	} {
		requireContains(t, text, want)
	}
	requireContains(t, plainCard(text), "❌ Gist failed · example/project")
	requireNotContains(t, text, "line-20")
	requireNotContains(t, text, "\x1b[")
	if _, ok := f.bot.cardID(req.ID); ok {
		t.Error("card entry was not removed after the failure card")
	}

	sent := f.sender.sentDocuments()
	if len(sent) != 1 {
		t.Fatalf("documents = %d, want the job log only", len(sent))
	}
	if sent[0].filename != req.ID+".log" {
		t.Errorf("log filename = %q, want %q", sent[0].filename, req.ID+".log")
	}
	if string(sent[0].data) != log.String() {
		t.Error("log attachment does not match the full job log")
	}
}

func TestNotifyMissingCardSendsNewCard(t *testing.T) {
	f := newBotFixture(t)
	docs := writeGistDocs(t, map[string]string{
		"codemap":     "CODEMAP",
		"routing":     "ROUTING",
		"observation": "OBSERVATION",
	})

	f.bot.Notify(context.Background(), testResult(docs))

	if got := len(f.sender.sentEdits()); got != 0 {
		t.Errorf("edits = %d, want 0 without a stored card", got)
	}
	messages := f.sender.sentMessages()
	if len(messages) != 1 {
		t.Fatalf("messages = %d, want one final card", len(messages))
	}
	want := "✅ Gist ready · example/project\nflash · cli · 1m 30s · 3 docs"
	if got := plainCard(messages[0].html); got != want {
		t.Errorf("card = %q, want %q", got, want)
	}
	if got := len(f.sender.sentGroups()); got != 1 {
		t.Errorf("media groups = %d, want 1", got)
	}
}

func TestNotifyEditFailureSendsNewCard(t *testing.T) {
	f := newBotFixture(t)
	req := f.enqueueGist("https://github.com/example/project")
	f.sender.editErr = errors.New("message to edit not found")
	docs := writeGistDocs(t, map[string]string{
		"codemap":     "CODEMAP",
		"routing":     "ROUTING",
		"observation": "OBSERVATION",
	})

	res := testResult(docs)
	res.Request = req
	f.bot.Notify(context.Background(), res)

	if got := len(f.sender.sentEdits()); got != 1 {
		t.Fatalf("edits = %d, want 1 failed edit", got)
	}
	messages := f.sender.sentMessages()
	if len(messages) != 2 {
		t.Fatalf("messages = %d, want initial card plus fallback card", len(messages))
	}
	if got := plainCard(messages[1].html); !strings.HasPrefix(got, "✅ Gist ready") {
		t.Errorf("fallback card = %q, want a Gist ready card", got)
	}
	if got := len(f.sender.sentGroups()); got != 1 {
		t.Errorf("media groups = %d, want 1", got)
	}
}

func TestAlbumFallsBackToSeparateDocuments(t *testing.T) {
	f := newBotFixture(t)
	req := f.enqueueGist("https://github.com/example/project")
	f.sender.mediaGroupErr = errors.New("media group rejected")
	docs := writeGistDocs(t, map[string]string{
		"codemap":     "CODEMAP",
		"routing":     "ROUTING",
		"observation": "OBSERVATION",
	})

	res := testResult(docs)
	res.Request = req
	f.bot.Notify(context.Background(), res)

	if got := len(f.sender.sentGroups()); got != 1 {
		t.Errorf("media group attempts = %d, want 1", got)
	}
	sent := f.sender.sentDocuments()
	if len(sent) != 3 {
		t.Fatalf("documents = %d, want 3 fallbacks", len(sent))
	}
	wantNames := []string{"codemap.md", "routing.md", "observation.md"}
	for i, doc := range sent {
		if doc.filename != wantNames[i] {
			t.Errorf("document[%d] filename = %q, want %q", i, doc.filename, wantNames[i])
		}
	}
	if len(f.sleeps) != 3 {
		t.Fatalf("sleeps = %d, want 3", len(f.sleeps))
	}
	for i, d := range f.sleeps {
		if d != time.Second {
			t.Errorf("sleep[%d] = %v, want 1s", i, d)
		}
	}
}

func TestSingleDocumentIsNotAnAlbum(t *testing.T) {
	f := newBotFixture(t)
	req := f.enqueueGist("https://github.com/example/project")
	docs := writeGistDocs(t, map[string]string{"codemap": "CODEMAP"})

	res := testResult(docs)
	res.Request = req
	f.bot.Notify(context.Background(), res)

	if got := len(f.sender.sentGroups()); got != 0 {
		t.Errorf("media groups = %d, want 0", got)
	}
	sent := f.sender.sentDocuments()
	if len(sent) != 1 {
		t.Fatalf("documents = %d, want 1", len(sent))
	}
	if sent[0].filename != "codemap.md" || string(sent[0].data) != "CODEMAP" {
		t.Errorf("document = %q %q, want codemap.md CODEMAP", sent[0].filename, sent[0].data)
	}
	if len(f.sleeps) != 0 {
		t.Errorf("sleeps = %v, want none for a single document", f.sleeps)
	}
}

func TestFailureLogAttachmentIsCapped(t *testing.T) {
	const maxBytes = 5 << 20
	f := newBotFixture(t)
	logPath := filepath.Join(t.TempDir(), "job-1.log")
	body := strings.Repeat("x", maxBytes) + "\nTAIL-END\n"
	if err := os.WriteFile(logPath, []byte(body), 0o600); err != nil {
		t.Fatalf("write log: %v", err)
	}

	res := testResult(repo.Docs{})
	res.Status = state.StatusFailed
	res.Err = errors.New("clone failed")
	res.LogPath = logPath
	f.bot.Notify(context.Background(), res)

	sent := f.sender.sentDocuments()
	if len(sent) != 1 {
		t.Fatalf("documents = %d, want 1", len(sent))
	}
	if got := len(sent[0].data); got > maxBytes {
		t.Errorf("log attachment = %d bytes, want at most %d", got, maxBytes)
	}
	if !strings.Contains(string(sent[0].data[:64]), "truncated") {
		t.Errorf("log attachment does not carry a truncation marker: %q", sent[0].data[:64])
	}
	if !strings.HasSuffix(string(sent[0].data), "TAIL-END\n") {
		t.Error("log attachment does not end with the newest log data")
	}
}

func TestCardTextIsHTMLSafe(t *testing.T) {
	f := newBotFixture(t)

	logPath := filepath.Join(t.TempDir(), "job-1.log")
	if err := os.WriteFile(logPath, []byte(`<b>bold</b> & 'quoted' < >`+"\n"), 0o600); err != nil {
		t.Fatalf("write log: %v", err)
	}

	res := testResult(repo.Docs{})
	res.Request.Target = repo.Target{URL: `<script>alert("repo")</script>`}
	res.Status = state.StatusFailed
	res.Err = errors.New(`<script>alert("x")</script>`)
	res.LogPath = logPath

	f.bot.Notify(context.Background(), res)

	texts := f.sender.messageTexts()
	if len(texts) != 1 {
		t.Fatalf("messages = %d, want 1", len(texts))
	}
	text := texts[0]
	for _, banned := range []string{"<script>", "<b>bold", "< >"} {
		requireNotContains(t, text, banned)
	}
	for _, want := range []string{"&lt;script&gt;", "&lt;b&gt;bold&lt;/b&gt;", "&amp;"} {
		requireContains(t, text, want)
	}
	if got := utf16Count(text); got > maxMessageUTF16 {
		t.Errorf("card = %d UTF-16 units, want at most %d", got, maxMessageUTF16)
	}
}

func equalStrings(got, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}

func TestNotifyDocumentCapAtOneMiB(t *testing.T) {
	f := newBotFixture(t)
	huge := strings.Repeat("x", 3*(1<<20)/2)
	docs := writeGistDocs(t, map[string]string{"codemap": huge})

	f.bot.Notify(context.Background(), testResult(docs))

	sent := f.sender.sentDocuments()
	if len(sent) != 1 {
		t.Fatalf("documents = %d, want 1", len(sent))
	}
	if len(sent[0].data) != maxDocBytes {
		t.Errorf("document size = %d, want %d", len(sent[0].data), maxDocBytes)
	}
}

func TestTailFileStripsANSIAndKeepsLastLines(t *testing.T) {
	path := filepath.Join(t.TempDir(), "job.log")
	body := "first\n\x1b[31msecond\x1b[0m\r\nthird\x00\nfourth\nfifth"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write log: %v", err)
	}

	got, err := tailFile(path, 2)
	if err != nil {
		t.Fatalf("tailFile: %v", err)
	}
	if want := "fourth\nfifth"; got != want {
		t.Errorf("tailFile = %q, want %q", got, want)
	}

	got, err = tailFile(path, 0)
	if err != nil {
		t.Fatalf("tailFile with zero lines: %v", err)
	}
	if got != "" {
		t.Errorf("tailFile with zero lines = %q, want empty", got)
	}

	if _, err := tailFile(filepath.Join(t.TempDir(), "missing.log"), 5); err == nil {
		t.Error("tailFile on a missing file returned nil error")
	}
}

func TestCommandMenuListsAllCommands(t *testing.T) {
	menu := CommandMenu()
	want := []string{"gist", "status", "cancel", "list", "show", "log", "model", "runner", "help"}
	if len(menu) != len(want) {
		t.Fatalf("menu entries = %d, want %d", len(menu), len(want))
	}
	for i, command := range want {
		if menu[i].Command != command {
			t.Errorf("menu[%d] command = %q, want %q", i, menu[i].Command, command)
		}
		if strings.TrimSpace(menu[i].Description) == "" {
			t.Errorf("menu[%d] (%s) has no description", i, menu[i].Command)
		}
	}

	menu[0].Command = "changed"
	if CommandMenu()[0].Command != "gist" {
		t.Error("CommandMenu returns shared mutable state")
	}
}

func TestUnknownCommandRepliesWithHelp(t *testing.T) {
	f := newBotFixture(t)
	f.handle(privateUpdate("/nope"))

	texts := f.sender.messageTexts()
	if len(texts) != 1 {
		t.Fatalf("messages = %d, want 1", len(texts))
	}
	requireContains(t, texts[0], "/help")
}
