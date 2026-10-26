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

// messageSend and documentSend record one fake Telegram send.
type messageSend struct {
	chatID int64
	html   string
}

type documentSend struct {
	chatID   int64
	filename string
	data     []byte
}

// fakeSender records outgoing Telegram traffic.
type fakeSender struct {
	mu          sync.Mutex
	messages    []messageSend
	documents   []documentSend
	messageErr  error
	documentErr error
}

func (s *fakeSender) SendMessage(_ context.Context, chatID int64, html string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.messages = append(s.messages, messageSend{chatID: chatID, html: html})
	return s.messageErr
}

func (s *fakeSender) SendDocument(_ context.Context, chatID int64, filename string, data []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	kept := make([]byte, len(data))
	copy(kept, data)
	s.documents = append(s.documents, documentSend{chatID: chatID, filename: filename, data: kept})
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

func (s *fakeSender) sentDocuments() []documentSend {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]documentSend(nil), s.documents...)
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

func TestStartAndHelpShowModelRunnerAndJob(t *testing.T) {
	f := newBotFixture(t)

	f.handle(privateUpdate("/start"))
	f.handle(privateUpdate("/help@ZeroTwoBot"))

	texts := f.sender.messageTexts()
	if len(texts) != 2 {
		t.Fatalf("messages = %d, want 2", len(texts))
	}
	for i, text := range texts {
		for _, want := range []string{
			"flash", "test/flash", "cli", "idle",
			"/gist", "/status", "/cancel", "/list", "/show", "/log", "/model", "/runner", "/help",
		} {
			if !strings.Contains(text, want) {
				t.Errorf("message[%d] does not contain %q:\n%s", i, want, text)
			}
		}
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
	if len(texts) != 3 {
		t.Fatalf("messages = %d, want 3", len(texts))
	}
	requireContains(t, texts[2], "running")
	requireContains(t, texts[2], "example/project")
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
		requireContains(t, texts[0], "Started Example/Project · flash · api")
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
		requireContains(t, texts[0], "free (vendor/model#high)")
	})

	t.Run("queued position", func(t *testing.T) {
		f := newBotFixture(t)
		f.jobs.position = 2

		f.handle(privateUpdate("/gist https://github.com/example/other"))

		texts := f.sender.messageTexts()
		if len(texts) != 1 {
			t.Fatalf("messages = %d, want 1", len(texts))
		}
		requireContains(t, texts[0], "Queued #2")
		requireContains(t, texts[0], "example/other")
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

func TestStatusShowsElapsedAndQueue(t *testing.T) {
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
		for _, want := range []string{
			"<pre>", "example/project", "flash", "cli", "1m30s",
			"#1", "other/repo", "#2", "third/thing", "pro", "api",
		} {
			requireContains(t, texts[0], want)
		}
	})

	t.Run("nothing running", func(t *testing.T) {
		f := newBotFixture(t)
		f.handle(privateUpdate("/status"))

		texts := f.sender.messageTexts()
		if len(texts) != 1 {
			t.Fatalf("messages = %d, want 1", len(texts))
		}
		requireContains(t, texts[0], "No jobs running")
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

func TestNotifySuccessSendsSummaryAndThreeDocs(t *testing.T) {
	f := newBotFixture(t)
	docs := writeGistDocs(t, map[string]string{
		"codemap":     "CODEMAP",
		"routing":     "ROUTING",
		"observation": "OBSERVATION",
	})

	f.bot.Notify(context.Background(), testResult(docs))

	texts := f.sender.messageTexts()
	if len(texts) != 1 {
		t.Fatalf("messages = %d, want 1", len(texts))
	}
	for _, want := range []string{
		"<pre>", "example/project", "flash", "cli", "1m30s",
		"codemap", "routing", "observation",
	} {
		requireContains(t, texts[0], want)
	}
	requireNotContains(t, texts[0], "Missing")

	sent := f.sender.sentDocuments()
	if len(sent) != 3 {
		t.Fatalf("documents = %d, want 3", len(sent))
	}
	wantNames := []string{"codemap.md", "routing.md", "observation.md"}
	wantBodies := []string{"CODEMAP", "ROUTING", "OBSERVATION"}
	for i, doc := range sent {
		if doc.chatID != testChatID {
			t.Errorf("document[%d] chatID = %d, want %d", i, doc.chatID, testChatID)
		}
		if doc.filename != wantNames[i] {
			t.Errorf("document[%d] filename = %q, want %q", i, doc.filename, wantNames[i])
		}
		if string(doc.data) != wantBodies[i] {
			t.Errorf("document[%d] body = %q, want %q", i, doc.data, wantBodies[i])
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

func TestNotifyPartialSendsAvailableDocsAndWarning(t *testing.T) {
	f := newBotFixture(t)
	docs := writeGistDocs(t, map[string]string{"codemap": "CODEMAP"})

	f.bot.Notify(context.Background(), testResult(docs))

	texts := f.sender.messageTexts()
	if len(texts) != 1 {
		t.Fatalf("messages = %d, want 1", len(texts))
	}
	requireContains(t, texts[0], "Missing: routing, observation")

	sent := f.sender.sentDocuments()
	if len(sent) != 1 {
		t.Fatalf("documents = %d, want 1", len(sent))
	}
	if sent[0].filename != "codemap.md" {
		t.Errorf("document filename = %q, want codemap.md", sent[0].filename)
	}
	if string(sent[0].data) != "CODEMAP" {
		t.Errorf("document body = %q, want CODEMAP", sent[0].data)
	}
}

func TestNotifyFailureSendsLogTail(t *testing.T) {
	f := newBotFixture(t)

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
	res.Status = state.StatusFailed
	res.Err = errors.New("clone failed")
	res.LogPath = logPath

	f.bot.Notify(context.Background(), res)

	texts := f.sender.messageTexts()
	if len(texts) != 1 {
		t.Fatalf("messages = %d, want 1", len(texts))
	}
	text := texts[0]
	for _, want := range []string{"failed", "clone failed", "<pre>", "line-11", "line-29", "line-30"} {
		requireContains(t, text, want)
	}
	requireNotContains(t, text, "line-10")
	requireNotContains(t, text, "\x1b[")
	if got := len(f.sender.sentDocuments()); got != 0 {
		t.Errorf("documents = %d, want 0", got)
	}
}

func TestNotifyHTMLIsEscaped(t *testing.T) {
	f := newBotFixture(t)

	logPath := filepath.Join(t.TempDir(), "job-1.log")
	if err := os.WriteFile(logPath, []byte(`<b>bold</b> & 'quoted' < >`+"\n"), 0o600); err != nil {
		t.Fatalf("write log: %v", err)
	}

	res := testResult(repo.Docs{})
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

func TestUnknownCommandRepliesWithHelp(t *testing.T) {
	f := newBotFixture(t)
	f.handle(privateUpdate("/nope"))

	texts := f.sender.messageTexts()
	if len(texts) != 1 {
		t.Fatalf("messages = %d, want 1", len(texts))
	}
	requireContains(t, texts[0], "/help")
}
