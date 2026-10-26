package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"gistbot/internal/bot"
	"gistbot/internal/config"
	"gistbot/internal/state"
	"gistbot/internal/telegram"
)

const (
	// testToken and testAPIPassword are deliberately fake. No test prints
	// them on failure, so a real credential can never leak through copied
	// output.
	testToken       = "123456:test-token-not-for-logs"
	testAPIPassword = "test-api-password-not-for-logs"
	testOwnerID     = int64(4242)
	testRepoID      = "github__owner__repo"
)

func TestRunRegistersCommandMenu(t *testing.T) {
	t.Run("registers after deleteWebhook", func(t *testing.T) {
		fake := newFakeTelegram(t)
		env := newTestEnv(t, fake.server.URL)

		handle := startRun(t, env.cfg, discardLogger())
		waitFor(t, "command menu", func() bool { return fake.commandCallsCount() > 0 })
		waitFor(t, "initial poll", func() bool { return fake.hasPollOffset(0) })
		handle.stop(t)

		got := fake.commandsCopy()
		want := bot.CommandMenu()
		if len(got) != len(want) {
			t.Fatalf("registered commands = %d, want %d", len(got), len(want))
		}
		for i := range want {
			if got[i] != want[i] {
				t.Errorf("command[%d] = %+v, want %+v", i, got[i], want[i])
			}
		}

		deleteAt := fake.eventIndex("deleteWebhook")
		setAt := fake.eventIndex("setMyCommands")
		pollAt := fake.eventIndex("getUpdates")
		if deleteAt < 0 || setAt < 0 {
			t.Fatalf("events = %v, want deleteWebhook and setMyCommands", fake.eventsCopy())
		}
		if deleteAt > setAt {
			t.Errorf("setMyCommands ran before deleteWebhook: %v", fake.eventsCopy())
		}
		if pollAt >= 0 && setAt > pollAt {
			t.Errorf("setMyCommands ran after polling started: %v", fake.eventsCopy())
		}
	})

	t.Run("failure is logged and startup continues", func(t *testing.T) {
		fake := newFakeTelegram(t)
		fake.failSetMyCommands(http.StatusInternalServerError, "Internal Server Error")
		env := newTestEnv(t, fake.server.URL)

		logs := &syncBuffer{}
		logger := slog.New(slog.NewTextHandler(logs, nil))
		handle := startRun(t, env.cfg, logger)
		waitFor(t, "initial poll after menu failure", func() bool {
			return fake.hasPollOffset(0)
		})
		handle.stop(t)

		if got := fake.commandCallsCount(); got == 0 {
			t.Error("setMyCommands was not attempted")
		}
		logged := logs.String()
		if !strings.Contains(logged, "command menu") {
			t.Errorf("menu failure was not logged:\n%s", logged)
		}
		if strings.Contains(logged, testToken) {
			t.Error("log output contains the Telegram token")
		}
		if strings.Contains(logged, testAPIPassword) {
			t.Error("log output contains the OpenCode API password")
		}
	})
}

func TestRunDeletesWebhookAndPolls(t *testing.T) {
	fake := newFakeTelegram(t)
	env := newTestEnv(t, fake.server.URL)

	handle := startRun(t, env.cfg, discardLogger())
	waitFor(t, "webhook deletion", func() bool { return fake.deleteCount() > 0 })
	waitFor(t, "initial poll", func() bool { return fake.hasPollOffset(0) })
	handle.stop(t)
}

func TestRunDeleteWebhookErrorDoesNotLogSecrets(t *testing.T) {
	fake := newFakeTelegram(t)
	fake.failDeleteWebhook(http.StatusUnauthorized, "Unauthorized: bot "+testToken+" rejected")
	env := newTestEnv(t, fake.server.URL)

	logs := &syncBuffer{}
	logger := slog.New(slog.NewTextHandler(logs, nil))
	err := runAndLog(context.Background(), env.cfg, logger)
	if err == nil {
		t.Fatal("run succeeded despite a deleteWebhook error")
	}
	if !strings.Contains(err.Error(), "delete telegram webhook") {
		t.Errorf("error %q does not identify the webhook step", err)
	}
	if strings.Contains(err.Error(), testToken) {
		t.Error("returned error contains the Telegram token")
	}
	if strings.Contains(err.Error(), testAPIPassword) {
		t.Error("returned error contains the OpenCode API password")
	}

	logged := logs.String()
	if logged == "" {
		t.Fatal("the startup error was not logged")
	}
	if strings.Contains(logged, testToken) {
		t.Error("log output contains the Telegram token")
	}
	if strings.Contains(logged, testAPIPassword) {
		t.Error("log output contains the OpenCode API password")
	}
	if !strings.Contains(logged, "<redacted>") {
		t.Error("log output does not show the redaction marker")
	}

	if got := fake.deleteCount(); got == 0 {
		t.Error("deleteWebhook was not attempted")
	}
	if fake.hasPollOffset(0) {
		t.Error("run started polling after deleteWebhook failed")
	}
}

func TestRunOwnerGistEndToEnd(t *testing.T) {
	fake := newFakeTelegram(t, ownerUpdate(1, "/gist https://github.com/owner/repo"))
	env := newTestEnv(t, fake.server.URL)

	logs := &syncBuffer{}
	logger := slog.New(slog.NewTextHandler(logs, nil))
	handle := startRun(t, env.cfg, logger)

	waitFor(t, "one gist album", func() bool { return fake.mediaGroupCount() >= 1 })
	handle.stop(t)

	messages := fake.messagesCopy()
	initial := ""
	for _, message := range messages {
		if strings.Contains(message.text, "⚡ Running") {
			initial = message.text
		}
	}
	if initial == "" {
		t.Fatalf("no running card in %d messages", len(messages))
	}
	for _, want := range []string{"⚡ Running", "owner/repo", "flash", "cli"} {
		if !strings.Contains(initial, want) {
			t.Errorf("initial card %q does not contain %q", initial, want)
		}
	}

	edits := fake.editsCopy()
	summary := ""
	for _, edit := range edits {
		if strings.Contains(edit.text, "Gist ready") {
			summary = edit.text
		}
	}
	if summary == "" {
		t.Fatalf("no completion card in %d edits", len(edits))
	}
	for _, want := range []string{"Gist ready", "owner/repo", "3 docs"} {
		if !strings.Contains(summary, want) {
			t.Errorf("summary %q does not contain %q", summary, want)
		}
	}

	groups := fake.mediaGroupsCopy()
	if len(groups) != 1 {
		t.Fatalf("media groups = %d, want 1", len(groups))
	}
	documents := groups[0].documents
	if len(documents) != 3 {
		t.Fatalf("album documents = %d, want 3", len(documents))
	}
	if got := fake.documentCount(); got != 0 {
		t.Errorf("separate documents = %d, want 0", got)
	}
	contents := make(map[string]string, len(documents))
	for _, document := range documents {
		contents[document.filename] = string(document.data)
	}
	for name, want := range map[string]string{
		"codemap.md":     "codemap\n",
		"routing.md":     "routing\n",
		"observation.md": "observation\n",
	} {
		if got := contents[name]; got != want {
			t.Errorf("document %s = %q, want %q", name, got, want)
		}
	}

	installed := filepath.Join(env.workDir, testRepoID, "docs", "codemap.md")
	if _, err := os.Stat(installed); err != nil {
		t.Errorf("installed gist document is missing: %v", err)
	}

	logged := logs.String()
	if strings.Contains(logged, env.cfg.TelegramToken) {
		t.Error("logs contain the Telegram token")
	}
	if strings.Contains(logged, env.cfg.OpenCodeAPIPassword) {
		t.Error("logs contain the OpenCode API password")
	}
}

func TestRunRestartDoesNotReplay(t *testing.T) {
	update := ownerUpdate(1, "/help")

	first := newFakeTelegram(t, update)
	env := newTestEnv(t, first.server.URL)
	handle := startRun(t, env.cfg, discardLogger())
	waitFor(t, "first reply", func() bool { return first.messageCount() >= 1 })
	handle.stop(t)

	data := readStateData(t, env.stateFile)
	if data.UpdateOffset != 2 {
		t.Fatalf("persisted update offset = %d, want 2", data.UpdateOffset)
	}

	second := newFakeTelegram(t, update)
	cfg := env.cfg
	cfg.TelegramAPI = second.server.URL
	restarted := startRun(t, cfg, discardLogger())
	waitFor(t, "restart poll at offset 2", func() bool { return second.hasPollOffset(2) })
	time.Sleep(300 * time.Millisecond)
	restarted.stop(t)

	if got := second.messageCount(); got != 0 {
		t.Fatalf("restart replayed the update: got %d messages", got)
	}
}

func TestRunShutdownCancelsRunningJob(t *testing.T) {
	fake := newFakeTelegram(t, ownerUpdate(1, "/gist https://github.com/owner/repo"))
	env := newTestEnv(t, fake.server.URL)

	childFile := filepath.Join(t.TempDir(), "child.pid")
	t.Setenv("FAKE_MODE", "sleep")
	t.Setenv("FAKE_CHILD", childFile)

	handle := startRun(t, env.cfg, discardLogger())
	waitFor(t, "running fake opencode", func() bool {
		raw, err := os.ReadFile(childFile)
		return err == nil && strings.TrimSpace(string(raw)) != ""
	})

	raw, err := os.ReadFile(childFile)
	if err != nil {
		t.Fatalf("read fake child pid: %v", err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(raw)))
	if err != nil || pid <= 0 {
		t.Fatalf("fake child pid is invalid")
	}

	handle.cancel()
	select {
	case err := <-handle.errCh:
		if err != nil {
			t.Fatalf("run returned an error on shutdown: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("run did not return promptly after cancellation")
	}

	if err := syscall.Kill(pid, 0); !errors.Is(err, syscall.ESRCH) {
		t.Fatalf("fake opencode child %d is still running (err=%v)", pid, err)
	}
	if got := fake.documentCount(); got != 0 {
		t.Fatalf("cancelled job sent %d documents, want 0", got)
	}
}

func TestRunMarksInterruptedJobsOnStartup(t *testing.T) {
	fake := newFakeTelegram(t)
	env := newTestEnv(t, fake.server.URL)

	store, err := state.Open(env.stateFile, state.Data{Runner: "cli", Model: "flash"})
	if err != nil {
		t.Fatalf("open state: %v", err)
	}
	for _, record := range []state.JobRecord{
		{ID: "job-running", RepoID: testRepoID, URL: "https://github.com/owner/repo", Runner: "cli", Model: "flash", Status: state.StatusRunning},
		{ID: "job-queued", RepoID: testRepoID, URL: "https://github.com/owner/repo", Runner: "cli", Model: "flash", Status: state.StatusQueued},
		{ID: "job-done", RepoID: testRepoID, URL: "https://github.com/owner/repo", Runner: "cli", Model: "flash", Status: state.StatusSucceeded},
	} {
		if err := store.UpsertJob(record); err != nil {
			t.Fatalf("seed job %s: %v", record.ID, err)
		}
	}

	handle := startRun(t, env.cfg, discardLogger())
	waitFor(t, "initial poll", func() bool { return fake.hasPollOffset(0) })
	handle.stop(t)

	data := readStateData(t, env.stateFile)
	statuses := make(map[string]state.JobStatus, len(data.Jobs))
	for _, job := range data.Jobs {
		statuses[job.ID] = job.Status
		if (job.ID == "job-running" || job.ID == "job-queued") && job.Status == state.StatusInterrupted {
			if job.Error != "bot restarted" {
				t.Errorf("%s error = %q, want %q", job.ID, job.Error, "bot restarted")
			}
			if job.FinishedAt.IsZero() {
				t.Errorf("%s has no finish time", job.ID)
			}
		}
	}
	if got := statuses["job-running"]; got != state.StatusInterrupted {
		t.Errorf("running job status = %q, want interrupted", got)
	}
	if got := statuses["job-queued"]; got != state.StatusInterrupted {
		t.Errorf("queued job status = %q, want interrupted", got)
	}
	if got := statuses["job-done"]; got != state.StatusSucceeded {
		t.Errorf("succeeded job status = %q, want succeeded", got)
	}
}

func TestRunRecoversStaleWorkDirs(t *testing.T) {
	fake := newFakeTelegram(t)
	env := newTestEnv(t, fake.server.URL)

	stale := filepath.Join(env.workDir, ".work", "job-stale", "repo")
	if err := os.MkdirAll(stale, 0o700); err != nil {
		t.Fatalf("create stale work directory: %v", err)
	}
	if err := os.WriteFile(filepath.Join(stale, "leftover"), []byte("x"), 0o600); err != nil {
		t.Fatalf("write stale file: %v", err)
	}
	installedDocs := filepath.Join(env.workDir, testRepoID, "docs")
	if err := os.MkdirAll(installedDocs, 0o700); err != nil {
		t.Fatalf("create installed repository: %v", err)
	}
	if err := os.WriteFile(filepath.Join(installedDocs, "codemap.md"), []byte("kept\n"), 0o600); err != nil {
		t.Fatalf("write installed document: %v", err)
	}

	handle := startRun(t, env.cfg, discardLogger())
	waitFor(t, "initial poll", func() bool { return fake.hasPollOffset(0) })
	handle.stop(t)

	if _, err := os.Stat(filepath.Join(env.workDir, ".work", "job-stale")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("stale work directory was not removed (err=%v)", err)
	}
	if _, err := os.Stat(filepath.Join(installedDocs, "codemap.md")); err != nil {
		t.Errorf("installed repository was removed: %v", err)
	}
}

// testEnv owns the temporary binary directory and configuration shared by the
// wiring tests.
type testEnv struct {
	cfg       config.Config
	workDir   string
	stateFile string
	logDir    string
}

func newTestEnv(t *testing.T, telegramURL string) *testEnv {
	t.Helper()

	root := t.TempDir()
	binDir := filepath.Join(root, "bin")
	workDir := filepath.Join(root, "work")
	stateFile := filepath.Join(root, "state", "state.json")
	logDir := filepath.Join(root, "logs")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatalf("create fake binary directory: %v", err)
	}

	writeExecutable(t, binDir, "git", filepath.Join("..", "..", "internal", "repo", "testdata", "fake-git.sh"))
	openCodeBin := writeExecutable(t, binDir, "opencode", filepath.Join("..", "..", "internal", "jobs", "testdata", "fake-opencode.sh"))
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	cfg := config.Config{
		TelegramToken:       testToken,
		TelegramAPI:         telegramURL,
		OwnerUserID:         testOwnerID,
		WorkDir:             workDir,
		OpenCodeBin:         openCodeBin,
		OpenCodeAPI:         "http://127.0.0.1:4096",
		OpenCodeAPIPassword: testAPIPassword,
		OpenCodeUnit:        "opencode-gist.service",
		Models: map[string]config.ModelRef{
			"free":  {ProviderID: "test", ModelID: "free"},
			"flash": {ProviderID: "test", ModelID: "flash"},
			"pro":   {ProviderID: "test", ModelID: "pro"},
		},
		DefaultModel:  "flash",
		DefaultRunner: "cli",
		JobTimeout:    20 * time.Second,
		CloneTimeout:  5 * time.Second,
		MaxQueue:      3,
		AllowedHosts:  []string{"github.com"},
		StateFile:     stateFile,
		LogDir:        logDir,
	}

	return &testEnv{cfg: cfg, workDir: workDir, stateFile: stateFile, logDir: logDir}
}

func writeExecutable(t *testing.T, dir, name, source string) string {
	t.Helper()

	data, err := os.ReadFile(source)
	if err != nil {
		t.Fatalf("read fake %s script: %v", name, err)
	}
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, data, 0o755); err != nil {
		t.Fatalf("write fake %s script: %v", name, err)
	}
	return path
}

type runHandle struct {
	cancel context.CancelFunc
	errCh  chan error
}

func startRun(t *testing.T, cfg config.Config, logger *slog.Logger) *runHandle {
	t.Helper()

	ctx, cancel := context.WithCancel(context.Background())
	handle := &runHandle{cancel: cancel, errCh: make(chan error, 1)}
	go func() {
		handle.errCh <- run(ctx, cfg, logger)
	}()
	return handle
}

func (h *runHandle) stop(t *testing.T) {
	t.Helper()

	h.cancel()
	select {
	case err := <-h.errCh:
		if err != nil {
			t.Fatalf("run returned an error: %v", err)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("run did not return after cancellation")
	}
}

func readStateData(t *testing.T, path string) state.Data {
	t.Helper()

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read state file: %v", err)
	}
	var data state.Data
	if err := json.Unmarshal(raw, &data); err != nil {
		t.Fatalf("decode state file: %v", err)
	}
	return data
}

func ownerUpdate(updateID int64, text string) telegram.Update {
	return telegram.Update{
		UpdateID: updateID,
		Message: &telegram.Message{
			MessageID: updateID,
			From:      &telegram.User{ID: testOwnerID, FirstName: "Owner"},
			Chat:      telegram.Chat{ID: testOwnerID, Type: "private"},
			Text:      text,
		},
	}
}

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func waitFor(t *testing.T, what string, condition func() bool) {
	t.Helper()

	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		if condition() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

type sentMessage struct {
	chatID string
	text   string
}

type sentEdit struct {
	chatID    string
	messageID string
	text      string
}

type sentDocument struct {
	chatID   string
	filename string
	data     []byte
}

type sentMediaGroup struct {
	chatID    string
	documents []sentDocument
}

// fakeTelegram is an in-process Bot API subset. It blocks getUpdates until the
// request is cancelled when no pending update matches the offset.
type fakeTelegram struct {
	token   string
	server  *httptest.Server
	updates []telegram.Update
	closed  chan struct{}

	mu          sync.Mutex
	deleteCalls int
	deleteCode  int
	deleteDesc  string
	pollOffsets []int64
	messages    []sentMessage
	edits       []sentEdit
	documents   []sentDocument
	mediaGroups []sentMediaGroup
	events      []string

	commandCalls int
	commandCode  int
	commandDesc  string
	commands     []telegram.BotCommand
}

func newFakeTelegram(t *testing.T, updates ...telegram.Update) *fakeTelegram {
	t.Helper()

	fake := &fakeTelegram{
		token:   testToken,
		updates: updates,
		closed:  make(chan struct{}),
	}
	fake.server = httptest.NewServer(fake)
	t.Cleanup(func() {
		close(fake.closed)
		fake.server.CloseClientConnections()
		fake.server.Close()
	})
	return fake
}

// failDeleteWebhook makes the next deleteWebhook calls return a Bot API style
// error. description is included verbatim so a test can prove redaction.
func (f *fakeTelegram) failDeleteWebhook(code int, description string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.deleteCode = code
	f.deleteDesc = description
}

// failSetMyCommands makes the next setMyCommands calls return a Bot API style
// error. The command menu is optional, so this must not stop startup.
func (f *fakeTelegram) failSetMyCommands(code int, description string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.commandCode = code
	f.commandDesc = description
}

func (f *fakeTelegram) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	prefix := "/bot" + f.token + "/"
	if !strings.HasPrefix(r.URL.Path, prefix) {
		http.NotFound(w, r)
		return
	}

	switch method := strings.TrimPrefix(r.URL.Path, prefix); method {
	case "deleteWebhook":
		f.mu.Lock()
		f.deleteCalls++
		f.events = append(f.events, "deleteWebhook")
		code, description := f.deleteCode, f.deleteDesc
		f.mu.Unlock()
		if code != 0 {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(code)
			_, _ = io.WriteString(w,
				fmt.Sprintf(`{"ok":false,"error_code":%d,"description":%q}`, code, description))
			return
		}
		writeTelegramResult(w, "true")

	case "setMyCommands":
		if err := r.ParseForm(); err != nil {
			http.Error(w, "invalid form", http.StatusBadRequest)
			return
		}
		var commands []telegram.BotCommand
		if raw := r.PostForm.Get("commands"); raw != "" {
			if err := json.Unmarshal([]byte(raw), &commands); err != nil {
				http.Error(w, "invalid commands", http.StatusBadRequest)
				return
			}
		}
		f.mu.Lock()
		f.commandCalls++
		f.events = append(f.events, "setMyCommands")
		f.commands = append([]telegram.BotCommand(nil), commands...)
		code, description := f.commandCode, f.commandDesc
		f.mu.Unlock()
		if code != 0 {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(code)
			_, _ = io.WriteString(w,
				fmt.Sprintf(`{"ok":false,"error_code":%d,"description":%q}`, code, description))
			return
		}
		writeTelegramResult(w, "true")

	case "getUpdates":
		f.mu.Lock()
		f.events = append(f.events, "getUpdates")
		f.mu.Unlock()
		if err := r.ParseForm(); err != nil {
			http.Error(w, "invalid form", http.StatusBadRequest)
			return
		}
		offset, err := strconv.ParseInt(r.PostForm.Get("offset"), 10, 64)
		if err != nil {
			offset = 0
		}

		f.mu.Lock()
		f.pollOffsets = append(f.pollOffsets, offset)
		matching := make([]telegram.Update, 0, len(f.updates))
		for _, update := range f.updates {
			if update.UpdateID >= offset {
				matching = append(matching, update)
			}
		}
		f.mu.Unlock()

		if len(matching) > 0 {
			raw, err := json.Marshal(matching)
			if err != nil {
				http.Error(w, "encode updates", http.StatusInternalServerError)
				return
			}
			writeTelegramResult(w, string(raw))
			return
		}

		select {
		case <-r.Context().Done():
		case <-f.closed:
		}

	case "sendMessage":
		if err := r.ParseForm(); err != nil {
			http.Error(w, "invalid form", http.StatusBadRequest)
			return
		}
		f.mu.Lock()
		f.messages = append(f.messages, sentMessage{
			chatID: r.PostForm.Get("chat_id"),
			text:   r.PostForm.Get("text"),
		})
		f.mu.Unlock()
		writeTelegramResult(w, `{"message_id":1}`)

	case "sendDocument":
		if err := r.ParseMultipartForm(1 << 20); err != nil {
			http.Error(w, "invalid multipart form", http.StatusBadRequest)
			return
		}
		document := sentDocument{chatID: r.FormValue("chat_id")}
		if files := r.MultipartForm.File["document"]; len(files) > 0 {
			file, err := files[0].Open()
			if err != nil {
				http.Error(w, "open document", http.StatusInternalServerError)
				return
			}
			data, readErr := io.ReadAll(file)
			_ = file.Close()
			if readErr != nil {
				http.Error(w, "read document", http.StatusInternalServerError)
				return
			}
			document.filename = files[0].Filename
			document.data = data
		}
		f.mu.Lock()
		f.documents = append(f.documents, document)
		f.mu.Unlock()
		writeTelegramResult(w, `{"message_id":2}`)

	case "editMessageText":
		if err := r.ParseForm(); err != nil {
			http.Error(w, "invalid form", http.StatusBadRequest)
			return
		}
		f.mu.Lock()
		f.edits = append(f.edits, sentEdit{
			chatID:    r.PostForm.Get("chat_id"),
			messageID: r.PostForm.Get("message_id"),
			text:      r.PostForm.Get("text"),
		})
		f.mu.Unlock()
		writeTelegramResult(w, "true")

	case "sendMediaGroup":
		if err := r.ParseMultipartForm(1 << 20); err != nil {
			http.Error(w, "invalid multipart form", http.StatusBadRequest)
			return
		}
		var media []struct {
			Media string `json:"media"`
		}
		if err := json.Unmarshal([]byte(r.FormValue("media")), &media); err != nil {
			http.Error(w, "invalid media", http.StatusBadRequest)
			return
		}
		group := sentMediaGroup{chatID: r.FormValue("chat_id")}
		for _, item := range media {
			field := strings.TrimPrefix(item.Media, "attach://")
			files := r.MultipartForm.File[field]
			if len(files) == 0 {
				http.Error(w, "missing media file", http.StatusBadRequest)
				return
			}
			file, err := files[0].Open()
			if err != nil {
				http.Error(w, "open media file", http.StatusInternalServerError)
				return
			}
			data, readErr := io.ReadAll(file)
			_ = file.Close()
			if readErr != nil {
				http.Error(w, "read media file", http.StatusInternalServerError)
				return
			}
			group.documents = append(group.documents, sentDocument{
				chatID:   group.chatID,
				filename: files[0].Filename,
				data:     data,
			})
		}
		f.mu.Lock()
		f.mediaGroups = append(f.mediaGroups, group)
		f.mu.Unlock()
		writeTelegramResult(w, `[{"message_id":3}]`)

	default:
		http.NotFound(w, r)
	}
}

func writeTelegramResult(w http.ResponseWriter, result string) {
	w.Header().Set("Content-Type", "application/json")
	_, _ = io.WriteString(w, fmt.Sprintf(`{"ok":true,"result":%s}`, result))
}

func (f *fakeTelegram) deleteCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.deleteCalls
}

func (f *fakeTelegram) hasPollOffset(offset int64) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, seen := range f.pollOffsets {
		if seen == offset {
			return true
		}
	}
	return false
}

func (f *fakeTelegram) commandCallsCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.commandCalls
}

func (f *fakeTelegram) commandsCopy() []telegram.BotCommand {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]telegram.BotCommand(nil), f.commands...)
}

func (f *fakeTelegram) eventsCopy() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.events...)
}

// eventIndex returns the index of the first recorded event with name, or -1.
func (f *fakeTelegram) eventIndex(name string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	for i, event := range f.events {
		if event == name {
			return i
		}
	}
	return -1
}

func (f *fakeTelegram) messageCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.messages)
}

func (f *fakeTelegram) messagesCopy() []sentMessage {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]sentMessage(nil), f.messages...)
}

func (f *fakeTelegram) editsCopy() []sentEdit {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]sentEdit(nil), f.edits...)
}

func (f *fakeTelegram) mediaGroupCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.mediaGroups)
}

func (f *fakeTelegram) mediaGroupsCopy() []sentMediaGroup {
	f.mu.Lock()
	defer f.mu.Unlock()
	groups := make([]sentMediaGroup, len(f.mediaGroups))
	for i, group := range f.mediaGroups {
		copied := sentMediaGroup{chatID: group.chatID}
		copied.documents = append([]sentDocument(nil), group.documents...)
		groups[i] = copied
	}
	return groups
}

func (f *fakeTelegram) documentCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.documents)
}

func (f *fakeTelegram) documentsCopy() []sentDocument {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]sentDocument(nil), f.documents...)
}

type syncBuffer struct {
	mu  sync.Mutex
	buf []byte
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.buf = append(b.buf, p...)
	return len(p), nil
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return string(b.buf)
}
