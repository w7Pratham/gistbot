package jobs

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/w7Pratham/gistbot/internal/config"
	"github.com/w7Pratham/gistbot/internal/repo"
	"github.com/w7Pratham/gistbot/internal/state"
)

const (
	testJobA = "job-1"
	testJobB = "job-2"
	testJobC = "job-3"
	testJobD = "job-4"
	testJobE = "job-5"

	testWait = 5 * time.Second
)

var (
	testTarget = repo.Target{
		URL:   "https://github.com/example/project",
		Host:  "github.com",
		Owner: "example",
		Name:  "project",
		ID:    "github__example__project",
	}

	allDocs = []string{
		"docs/codemap.md",
		"docs/routing.md",
		"docs/observation.md",
	}
)

func testRequest(id string, runner RunnerKind) Request {
	return Request{
		ID:         id,
		Target:     testTarget,
		Focus:      "auth flow",
		ChatID:     42,
		Runner:     runner,
		Model:      config.ModelRef{ProviderID: "test", ModelID: "model"},
		ModelAlias: "flash",
		EnqueuedAt: time.Now(),
	}
}

// managerFixture wires a Manager to fakes backed by a real state store and a
// private temporary workspace.
type managerFixture struct {
	t       *testing.T
	workDir string
	logDir  string
	cfg     config.Config
	store   *state.Store
	prep    *fakePreparer
	cli     *fakeRunner
	api     *fakeRunner
	notify  *fakeNotifier

	// notifier is what the manager receives; tests can replace it with a
	// notifier that also implements StartNotifier.
	notifier Notifier

	mgr    *Manager
	ctx    context.Context
	cancel context.CancelFunc
}

// fixtureOption adjusts the fakes a fixture hands to the manager before the
// manager is constructed.
type fixtureOption func(*managerFixture)

func newManagerFixture(t *testing.T, mutate func(*config.Config), opts ...fixtureOption) *managerFixture {
	t.Helper()

	root := t.TempDir()
	f := &managerFixture{
		t:       t,
		workDir: filepath.Join(root, "work"),
		logDir:  filepath.Join(root, "logs"),
	}
	if err := os.MkdirAll(f.workDir, 0o700); err != nil {
		t.Fatalf("create work directory: %v", err)
	}

	st, err := state.Open(filepath.Join(root, "state.json"), state.Data{Runner: "cli", Model: "flash"})
	if err != nil {
		t.Fatalf("open state: %v", err)
	}
	f.store = st
	f.prep = newFakePreparer(f.workDir)
	f.cli = &fakeRunner{kind: RunnerCLI}
	f.api = &fakeRunner{kind: RunnerAPI}
	f.notify = newFakeNotifier()
	f.notifier = f.notify

	f.cfg = config.Config{
		WorkDir:       f.workDir,
		LogDir:        f.logDir,
		MaxQueue:      3,
		JobTimeout:    testWait,
		DefaultRunner: "cli",
	}
	if mutate != nil {
		mutate(&f.cfg)
	}
	for _, opt := range opts {
		opt(f)
	}

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	f.mgr = NewManager(f.cfg, f.store, f.prep, map[RunnerKind]Runner{
		RunnerCLI: f.cli,
		RunnerAPI: f.api,
	}, f.notifier, logger)

	f.ctx, f.cancel = context.WithCancel(context.Background())
	t.Cleanup(func() {
		f.mgr.Stop(context.Background())
		f.cancel()
	})
	return f
}

func (f *managerFixture) start() {
	f.t.Helper()
	f.mgr.Start(f.ctx)
}

func (f *managerFixture) enqueue(req Request) int {
	f.t.Helper()
	position, err := f.mgr.Enqueue(req)
	if err != nil {
		f.t.Fatalf("Enqueue(%s): %v", req.ID, err)
	}
	return position
}

func (f *managerFixture) jobRecord(id string) state.JobRecord {
	f.t.Helper()
	for _, job := range f.store.Snapshot().Jobs {
		if job.ID == id {
			return job
		}
	}
	f.t.Fatalf("job %s is missing from state", id)
	return state.JobRecord{}
}

func waitUntil(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(testWait)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(2 * time.Millisecond)
	}
}

func testReadFile(t *testing.T, path string) string {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(raw)
}

func testWriteFile(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatalf("create directory for %s: %v", path, err)
	}
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

// fakePreparer imitates repo.Cloner without touching the network. It writes the
// requested documents into <workDir>/.work/<jobID>/repo and moves that tree to
// <workDir>/<target.ID> on Install.
type fakePreparer struct {
	mu       sync.Mutex
	workDir  string
	defaults []string
	docs     map[string][]string
	prepares []string
	installs []repo.Target
	aborts   []string
	events   []string

	prepareErr error
	installErr error
	hook       func(context.Context) error
}

func newFakePreparer(workDir string) *fakePreparer {
	return &fakePreparer{
		workDir:  workDir,
		defaults: append([]string(nil), allDocs...),
		docs:     make(map[string][]string),
	}
}

func (p *fakePreparer) setDocs(jobID string, rels ...string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.docs[jobID] = append([]string(nil), rels...)
}

func (p *fakePreparer) setDefaults(rels ...string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.defaults = append([]string(nil), rels...)
}

func (p *fakePreparer) jobDir(jobID string) string {
	return filepath.Join(p.workDir, ".work", jobID)
}

func (p *fakePreparer) Prepare(ctx context.Context, t repo.Target, jobID string, log io.Writer) (repo.Prepared, error) {
	p.mu.Lock()
	p.prepares = append(p.prepares, jobID)
	p.events = append(p.events, "prepare:"+jobID)
	docs, ok := p.docs[jobID]
	if !ok {
		docs = p.defaults
	}
	docs = append([]string(nil), docs...)
	prepareErr := p.prepareErr
	hook := p.hook
	p.mu.Unlock()

	if log != nil {
		fmt.Fprintf(log, "prepare %s\n", jobID)
	}
	if err := ctx.Err(); err != nil {
		return repo.Prepared{}, err
	}
	if hook != nil {
		if err := hook(ctx); err != nil {
			return repo.Prepared{}, err
		}
	}
	if prepareErr != nil {
		return repo.Prepared{}, prepareErr
	}

	jobDir := p.jobDir(jobID)
	repoDir := filepath.Join(jobDir, "repo")
	if err := os.MkdirAll(repoDir, 0o700); err != nil {
		return repo.Prepared{}, err
	}
	for _, rel := range docs {
		path := filepath.Join(repoDir, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			return repo.Prepared{}, err
		}
		if err := os.WriteFile(path, []byte("body of "+rel+"\n"), 0o600); err != nil {
			return repo.Prepared{}, err
		}
	}
	return repo.Prepared{JobDir: jobDir, RepoDir: repoDir}, nil
}

func (p *fakePreparer) Install(t repo.Target, repoDir string) error {
	p.mu.Lock()
	p.installs = append(p.installs, t)
	p.events = append(p.events, "install:"+t.ID)
	installErr := p.installErr
	p.mu.Unlock()

	if installErr != nil {
		return installErr
	}
	finalDir := filepath.Join(p.workDir, t.ID)
	if err := os.RemoveAll(finalDir); err != nil {
		return err
	}
	return os.Rename(repoDir, finalDir)
}

func (p *fakePreparer) Abort(jobDir string) error {
	p.mu.Lock()
	p.aborts = append(p.aborts, jobDir)
	p.events = append(p.events, "abort:"+filepath.Base(jobDir))
	p.mu.Unlock()
	return os.RemoveAll(jobDir)
}

func (p *fakePreparer) snapshot() (prepares []string, installs []repo.Target, aborts []string, events []string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]string(nil), p.prepares...),
		append([]repo.Target(nil), p.installs...),
		append([]string(nil), p.aborts...),
		append([]string(nil), p.events...)
}

// fakeRunner records each Run and can block until its context is cancelled.
type fakeRunner struct {
	mu          sync.Mutex
	kind        RunnerKind
	runFn       func(context.Context, Request) (RunResult, error)
	runs        []Request
	logs        []string
	cancelCalls int
}

func (r *fakeRunner) Run(ctx context.Context, req Request, logPath string) (RunResult, error) {
	r.mu.Lock()
	r.runs = append(r.runs, req)
	r.logs = append(r.logs, logPath)
	runFn := r.runFn
	r.mu.Unlock()

	if logPath != "" {
		file, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
		if err != nil {
			return RunResult{ExitCode: -1}, err
		}
		fmt.Fprintf(file, "runner %s %s\n", r.kind, req.ID)
		file.Close()
	}

	if runFn != nil {
		return runFn(ctx, req)
	}
	return RunResult{ExitCode: 0, Summary: "summary " + req.ID}, nil
}

func (r *fakeRunner) Cancel(ctx context.Context) error {
	r.mu.Lock()
	r.cancelCalls++
	r.mu.Unlock()
	return nil
}

func (r *fakeRunner) snapshot() ([]Request, []string, int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]Request(nil), r.runs...),
		append([]string(nil), r.logs...),
		r.cancelCalls
}

type notification struct {
	ctx context.Context
	res Result
}

type fakeNotifier struct {
	ch chan notification
}

func newFakeNotifier() *fakeNotifier {
	return &fakeNotifier{ch: make(chan notification, 32)}
}

func (n *fakeNotifier) Notify(ctx context.Context, res Result) {
	n.ch <- notification{ctx: ctx, res: res}
}

func (n *fakeNotifier) wait(t *testing.T) notification {
	t.Helper()
	select {
	case got := <-n.ch:
		return got
	case <-time.After(testWait):
		t.Fatal("timed out waiting for a job notification")
		return notification{}
	}
}

func (n *fakeNotifier) pending() int {
	return len(n.ch)
}

// startNotification is one observed NotifyStarted call together with the
// manager state visible at that moment.
type startNotification struct {
	ctx      context.Context
	req      Request
	status   state.JobStatus
	prepares int
}

// fakeStartNotifier implements both Notifier and StartNotifier. It captures the
// persisted job status and the number of Prepare calls at the moment the start
// hook fires, which lets the tests prove the hook's ordering.
type fakeStartNotifier struct {
	*fakeNotifier

	store *state.Store
	prep  *fakePreparer

	mu    sync.Mutex
	calls []startNotification
}

func newFakeStartNotifier() *fakeStartNotifier {
	return &fakeStartNotifier{fakeNotifier: newFakeNotifier()}
}

func withStartNotifier(n *fakeStartNotifier) fixtureOption {
	return func(f *managerFixture) {
		n.store = f.store
		n.prep = f.prep
		f.notifier = n
		f.notify = n.fakeNotifier
	}
}

func (n *fakeStartNotifier) NotifyStarted(ctx context.Context, req Request) {
	var status state.JobStatus
	if n.store != nil {
		for _, job := range n.store.Snapshot().Jobs {
			if job.ID == req.ID {
				status = job.Status
				break
			}
		}
	}

	prepares := 0
	if n.prep != nil {
		all, _, _, _ := n.prep.snapshot()
		prepares = len(all)
	}

	n.mu.Lock()
	n.calls = append(n.calls, startNotification{
		ctx:      ctx,
		req:      req,
		status:   status,
		prepares: prepares,
	})
	n.mu.Unlock()
}

func (n *fakeStartNotifier) started() []startNotification {
	n.mu.Lock()
	defer n.mu.Unlock()
	return append([]startNotification(nil), n.calls...)
}

// blockUntilCancelled is the run body for a job that must be stopped by the
// manager. Both paths return an error so the manager never treats it as success.
func blockUntilCancelled(started chan struct{}, once *sync.Once) func(context.Context, Request) (RunResult, error) {
	return func(ctx context.Context, req Request) (RunResult, error) {
		if started != nil {
			once.Do(func() { close(started) })
		}
		<-ctx.Done()
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return RunResult{ExitCode: -1}, ctx.Err()
		}
		return RunResult{ExitCode: -1}, ErrCancelled
	}
}

func TestEnqueueRejectsWhenThreeAreQueued(t *testing.T) {
	f := newManagerFixture(t, nil)
	started := make(chan struct{})
	var once sync.Once
	f.cli.runFn = blockUntilCancelled(started, &once)
	f.start()

	if position := f.enqueue(testRequest(testJobA, RunnerCLI)); position != 0 {
		t.Errorf("first Enqueue position = %d, want 0", position)
	}
	waitUntil(t, "first job running", func() bool {
		select {
		case <-started:
			return true
		default:
			return false
		}
	})

	for i, id := range []string{testJobB, testJobC, testJobD} {
		if position := f.enqueue(testRequest(id, RunnerCLI)); position != i+1 {
			t.Errorf("Enqueue(%s) position = %d, want %d", id, position, i+1)
		}
	}

	if _, err := f.mgr.Enqueue(testRequest(testJobE, RunnerCLI)); !errors.Is(err, ErrQueueFull) {
		t.Fatalf("Enqueue with a full queue error = %v, want ErrQueueFull", err)
	}

	jobs := f.store.Snapshot().Jobs
	if len(jobs) != 4 {
		t.Fatalf("persisted jobs = %d, want 4", len(jobs))
	}
	statuses := make(map[string]state.JobStatus, len(jobs))
	for _, job := range jobs {
		statuses[job.ID] = job.Status
	}
	for _, id := range []string{testJobB, testJobC, testJobD} {
		if statuses[id] != state.StatusQueued {
			t.Errorf("job %s status = %q, want %q", id, statuses[id], state.StatusQueued)
		}
	}
	if _, ok := statuses[testJobE]; ok {
		t.Errorf("rejected job %s was persisted", testJobE)
	}
}

func TestEnqueueValidatesJobIDAndRunner(t *testing.T) {
	f := newManagerFixture(t, nil)

	for _, id := range []string{
		"",
		"..",
		"../escape",
		"a/b",
		".hidden",
		"/absolute",
		strings.Repeat("a", 129),
	} {
		if _, err := f.mgr.Enqueue(testRequest(id, RunnerCLI)); err == nil {
			t.Errorf("Enqueue(%q) returned no error", id)
		}
	}
	if _, err := f.mgr.Enqueue(testRequest("job-ok", RunnerKind("bogus"))); err == nil {
		t.Error("Enqueue with an unknown runner returned no error")
	}
	badTarget := testRequest("job-bad-target", RunnerCLI)
	badTarget.Target.ID = "not-canonical"
	if _, err := f.mgr.Enqueue(badTarget); err == nil {
		t.Error("Enqueue with a non-canonical target ID returned no error")
	}

	if queued := f.mgr.Status().Queued; len(queued) != 0 {
		t.Errorf("queued jobs = %v, want none", queued)
	}
	if jobs := f.store.Snapshot().Jobs; len(jobs) != 0 {
		t.Errorf("persisted jobs = %v, want none", jobs)
	}
}

func TestStatusShowsRunningAndQueued(t *testing.T) {
	f := newManagerFixture(t, nil)
	started := make(chan struct{})
	var once sync.Once
	f.cli.runFn = blockUntilCancelled(started, &once)
	f.start()

	f.enqueue(testRequest(testJobA, RunnerCLI))
	waitUntil(t, "first job running", func() bool {
		select {
		case <-started:
			return true
		default:
			return false
		}
	})
	f.enqueue(testRequest(testJobB, RunnerCLI))
	f.enqueue(testRequest(testJobC, RunnerCLI))

	status := f.mgr.Status()
	if status.Running == nil {
		t.Fatal("Status().Running = nil, want the running job")
	}
	if status.Running.ID != testJobA {
		t.Errorf("Running.ID = %q, want %q", status.Running.ID, testJobA)
	}
	if status.Running.RepoID != testTarget.ID {
		t.Errorf("Running.RepoID = %q, want %q", status.Running.RepoID, testTarget.ID)
	}
	if status.Running.Runner != "cli" {
		t.Errorf("Running.Runner = %q, want cli", status.Running.Runner)
	}
	if status.Running.Model != "flash" {
		t.Errorf("Running.Model = %q, want flash", status.Running.Model)
	}
	if status.Running.StartedAt.IsZero() {
		t.Error("Running.StartedAt is zero")
	}
	if status.Running.Elapsed < 0 {
		t.Errorf("Running.Elapsed = %v, want a non-negative duration", status.Running.Elapsed)
	}

	if len(status.Queued) != 2 {
		t.Fatalf("Queued = %v, want two jobs", status.Queued)
	}
	for i, id := range []string{testJobB, testJobC} {
		view := status.Queued[i]
		if view.ID != id {
			t.Errorf("Queued[%d].ID = %q, want %q", i, view.ID, id)
		}
		if view.RepoID != testTarget.ID || view.Runner != "cli" || view.Model != "flash" {
			t.Errorf("Queued[%d] = %+v, want repo/runner/model filled in", i, view)
		}
		if !view.StartedAt.IsZero() || view.Elapsed != 0 {
			t.Errorf("Queued[%d] started timing before it ran: %+v", i, view)
		}
	}
}

func TestSuccessRunsClonePromptRunnerInstallAndNotify(t *testing.T) {
	f := newManagerFixture(t, nil)
	f.start()

	if position := f.enqueue(testRequest(testJobA, RunnerCLI)); position != 0 {
		t.Errorf("Enqueue position = %d, want 0", position)
	}
	got := f.notify.wait(t)
	res := got.res

	if res.Status != state.StatusSucceeded {
		t.Fatalf("Result.Status = %q, want %q (err %v)", res.Status, state.StatusSucceeded, res.Err)
	}
	if res.Err != nil {
		t.Errorf("Result.Err = %v, want nil", res.Err)
	}
	if got.ctx.Err() != nil {
		t.Errorf("Notifier received an already-cancelled context: %v", got.ctx.Err())
	}

	jobDir := f.prep.jobDir(testJobA)
	finalDir := filepath.Join(f.workDir, testTarget.ID)
	wantLog := filepath.Join(f.logDir, testJobA+".log")

	if res.Request.RunDir != jobDir {
		t.Errorf("RunDir = %q, want %q", res.Request.RunDir, jobDir)
	}
	if res.Request.TargetDir != filepath.Join(jobDir, "repo") {
		t.Errorf("TargetDir = %q, want %q", res.Request.TargetDir, filepath.Join(jobDir, "repo"))
	}
	if res.Request.Prompt != BuildPrompt("auth flow") {
		t.Errorf("Prompt = %q, want BuildPrompt(focus)", res.Request.Prompt)
	}
	if res.LogPath != wantLog {
		t.Errorf("LogPath = %q, want %q", res.LogPath, wantLog)
	}
	if res.StartedAt.IsZero() {
		t.Error("StartedAt is zero")
	}
	if res.FinishedAt.Before(res.StartedAt) {
		t.Errorf("FinishedAt %v is before StartedAt %v", res.FinishedAt, res.StartedAt)
	}
	if res.Summary != "summary "+testJobA {
		t.Errorf("Summary = %q, want the runner summary", res.Summary)
	}

	runs, logs, _ := f.cli.snapshot()
	if len(runs) != 1 {
		t.Fatalf("CLI runner runs = %d, want 1", len(runs))
	}
	if runs[0].ID != testJobA || runs[0].Runner != RunnerCLI {
		t.Errorf("runner request = %+v, want the enqueued job", runs[0])
	}
	if runs[0].Prompt != BuildPrompt("auth flow") {
		t.Errorf("runner Prompt = %q, want BuildPrompt(focus)", runs[0].Prompt)
	}
	if logs[0] != wantLog {
		t.Errorf("runner log path = %q, want %q", logs[0], wantLog)
	}

	prepares, installs, aborts, events := f.prep.snapshot()
	if len(prepares) != 1 || prepares[0] != testJobA {
		t.Errorf("prepares = %v, want [%s]", prepares, testJobA)
	}
	if len(installs) != 1 || installs[0].ID != testTarget.ID {
		t.Errorf("installs = %+v, want one install of %s", installs, testTarget.ID)
	}
	if len(aborts) != 1 || aborts[0] != jobDir {
		t.Errorf("aborts = %v, want [%s]", aborts, jobDir)
	}
	wantEvents := []string{"prepare:" + testJobA, "install:" + testTarget.ID, "abort:" + testJobA}
	if strings.Join(events, ",") != strings.Join(wantEvents, ",") {
		t.Errorf("preparer events = %v, want %v", events, wantEvents)
	}

	docs, err := repo.DiscoverDocs(finalDir)
	if err != nil {
		t.Fatalf("DiscoverDocs(final): %v", err)
	}
	if len(docs.Files) != len(allDocs) || len(docs.Missing) != 0 {
		t.Errorf("final docs = %+v, want all three present", docs)
	}
	for name, path := range res.Docs.Files {
		if want := filepath.Join(finalDir, "docs", name+".md"); path != want {
			t.Errorf("Result docs[%s] = %q, want %q", name, path, want)
		}
	}
	if _, err := os.Stat(jobDir); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("job sandbox still exists after install: %v", err)
	}

	body := testReadFile(t, wantLog)
	for _, want := range []string{"prepare " + testJobA, "runner cli " + testJobA} {
		if !strings.Contains(body, want) {
			t.Errorf("job log does not contain %q:\n%s", want, body)
		}
	}
	info, err := os.Stat(wantLog)
	if err != nil {
		t.Fatalf("stat job log: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("job log mode = %o, want 600", perm)
	}
	info, err = os.Stat(f.logDir)
	if err != nil {
		t.Fatalf("stat log directory: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o700 {
		t.Errorf("log directory mode = %o, want 700", perm)
	}

	record := f.jobRecord(testJobA)
	if record.Status != state.StatusSucceeded {
		t.Errorf("state status = %q, want %q", record.Status, state.StatusSucceeded)
	}
	if record.RepoID != testTarget.ID || record.URL != testTarget.URL {
		t.Errorf("state repo = %q / %q, want %q / %q", record.RepoID, record.URL, testTarget.ID, testTarget.URL)
	}
	if record.Runner != "cli" || record.Model != "flash" {
		t.Errorf("state runner/model = %q / %q, want cli / flash", record.Runner, record.Model)
	}
	if record.Error != "" {
		t.Errorf("state error = %q, want empty", record.Error)
	}
	if len(record.Docs) != 3 || strings.Join(record.Docs, ",") != "codemap,routing,observation" {
		t.Errorf("state docs = %v, want the canonical three", record.Docs)
	}
	if record.StartedAt.IsZero() || record.FinishedAt.IsZero() {
		t.Errorf("state timestamps = %v / %v, want both set", record.StartedAt, record.FinishedAt)
	}
}

func TestZeroDocsAbortsAndPreservesOldRepo(t *testing.T) {
	f := newManagerFixture(t, nil)
	f.prep.setDocs(testJobA)

	finalDir := filepath.Join(f.workDir, testTarget.ID)
	oldFile := filepath.Join(finalDir, "old.txt")
	testWriteFile(t, oldFile, "keep me")

	f.start()
	f.enqueue(testRequest(testJobA, RunnerCLI))
	got := f.notify.wait(t)
	res := got.res

	if res.Status != state.StatusFailed {
		t.Fatalf("Result.Status = %q, want %q", res.Status, state.StatusFailed)
	}
	if res.Err == nil {
		t.Fatal("Result.Err = nil, want a no-documents error")
	}
	if len(res.Docs.Missing) != 3 {
		t.Errorf("Result.Docs.Missing = %v, want all three", res.Docs.Missing)
	}

	_, installs, aborts, _ := f.prep.snapshot()
	if len(installs) != 0 {
		t.Errorf("installs = %+v, want none", installs)
	}
	if len(aborts) != 1 || aborts[0] != f.prep.jobDir(testJobA) {
		t.Errorf("aborts = %v, want the job sandbox", aborts)
	}
	if body := testReadFile(t, oldFile); body != "keep me" {
		t.Errorf("old repository contents = %q, want preserved", body)
	}
	if _, err := os.Stat(f.prep.jobDir(testJobA)); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("job sandbox still exists: %v", err)
	}

	record := f.jobRecord(testJobA)
	if record.Status != state.StatusFailed {
		t.Errorf("state status = %q, want %q", record.Status, state.StatusFailed)
	}
	if record.Error == "" {
		t.Error("state error is empty, want a failure reason")
	}
}

func TestPartialDocsInstallsAndReportsMissing(t *testing.T) {
	f := newManagerFixture(t, nil)
	f.prep.setDocs(testJobA, "docs/codemap.md", "docs/routing.md")
	f.start()

	f.enqueue(testRequest(testJobA, RunnerCLI))
	res := f.notify.wait(t).res

	if res.Status != state.StatusSucceeded {
		t.Fatalf("Result.Status = %q, want %q (err %v)", res.Status, state.StatusSucceeded, res.Err)
	}
	if len(res.Docs.Files) != 2 {
		t.Errorf("Result.Docs.Files = %v, want codemap and routing", res.Docs.Files)
	}
	if len(res.Docs.Missing) != 1 || res.Docs.Missing[0] != "observation" {
		t.Errorf("Result.Docs.Missing = %v, want [observation]", res.Docs.Missing)
	}

	_, installs, _, _ := f.prep.snapshot()
	if len(installs) != 1 {
		t.Errorf("installs = %+v, want one", installs)
	}
	finalDir := filepath.Join(f.workDir, testTarget.ID)
	if body := testReadFile(t, filepath.Join(finalDir, "docs", "routing.md")); !strings.Contains(body, "routing") {
		t.Errorf("installed routing.md = %q, want router content", body)
	}
	if _, err := os.Stat(filepath.Join(finalDir, "docs", "observation.md")); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("observation.md unexpectedly installed: %v", err)
	}

	record := f.jobRecord(testJobA)
	if strings.Join(record.Docs, ",") != "codemap,routing" {
		t.Errorf("state docs = %v, want [codemap routing]", record.Docs)
	}
}

func TestCancelStopsCurrentLeavesQueueAndNextStarts(t *testing.T) {
	f := newManagerFixture(t, nil)

	aStarted := make(chan struct{})
	var aOnce sync.Once
	bStarted := make(chan struct{})
	bRelease := make(chan struct{})
	f.cli.runFn = func(ctx context.Context, req Request) (RunResult, error) {
		switch req.ID {
		case testJobA:
			aOnce.Do(func() { close(aStarted) })
			<-ctx.Done()
			return RunResult{ExitCode: -1}, ErrCancelled
		case testJobB:
			close(bStarted)
			<-bRelease
			return RunResult{ExitCode: 0, Summary: "second done"}, nil
		default:
			return RunResult{ExitCode: 0, Summary: "unexpected"}, nil
		}
	}
	f.start()

	f.enqueue(testRequest(testJobA, RunnerCLI))
	waitUntil(t, "first job running", func() bool {
		select {
		case <-aStarted:
			return true
		default:
			return false
		}
	})
	f.enqueue(testRequest(testJobB, RunnerCLI))

	if !f.mgr.Cancel(context.Background()) {
		t.Fatal("Cancel() = false, want true for the running job")
	}

	gotA := f.notify.wait(t)
	if gotA.res.Status != state.StatusCancelled {
		t.Errorf("first Result.Status = %q, want %q", gotA.res.Status, state.StatusCancelled)
	}
	if !errors.Is(gotA.res.Err, ErrCancelled) {
		t.Errorf("first Result.Err = %v, want ErrCancelled", gotA.res.Err)
	}
	if gotA.ctx.Err() != nil {
		t.Errorf("cancel notification used the cancelled job context: %v", gotA.ctx.Err())
	}

	waitUntil(t, "queued job to start after cancellation", func() bool {
		select {
		case <-bStarted:
			return true
		default:
			return false
		}
	})
	status := f.mgr.Status()
	if status.Running == nil || status.Running.ID != testJobB {
		t.Errorf("Status().Running = %+v, want %s", status.Running, testJobB)
	}
	if len(status.Queued) != 0 {
		t.Errorf("Status().Queued = %v, want none", status.Queued)
	}

	_, installs, aborts, _ := f.prep.snapshot()
	if len(installs) != 0 {
		t.Errorf("installs = %+v, want none for a cancelled job", installs)
	}
	if len(aborts) != 1 {
		t.Errorf("aborts = %v, want exactly the cancelled job's sandbox", aborts)
	}

	close(bRelease)
	gotB := f.notify.wait(t)
	if gotB.res.Status != state.StatusSucceeded {
		t.Errorf("second Result.Status = %q, want %q (err %v)", gotB.res.Status, state.StatusSucceeded, gotB.res.Err)
	}
	if gotB.res.Summary != "second done" {
		t.Errorf("second Result.Summary = %q, want the runner summary", gotB.res.Summary)
	}

	_, _, cancelCalls := f.cli.snapshot()
	if cancelCalls != 1 {
		t.Errorf("runner Cancel calls = %d, want 1", cancelCalls)
	}
	if f.mgr.Cancel(context.Background()) {
		t.Error("Cancel() = true with no running job")
	}
	if record := f.jobRecord(testJobA); record.Status != state.StatusCancelled {
		t.Errorf("first job state = %q, want %q", record.Status, state.StatusCancelled)
	}
	if record := f.jobRecord(testJobB); record.Status != state.StatusSucceeded {
		t.Errorf("second job state = %q, want %q", record.Status, state.StatusSucceeded)
	}
}

func TestCancelWithoutRunningJobReturnsFalse(t *testing.T) {
	f := newManagerFixture(t, nil)
	if f.mgr.Cancel(context.Background()) {
		t.Error("Cancel() = true while idle, want false")
	}
}

func TestTimeoutMarksTimedOut(t *testing.T) {
	f := newManagerFixture(t, func(cfg *config.Config) {
		cfg.JobTimeout = 100 * time.Millisecond
	})
	f.cli.runFn = func(ctx context.Context, req Request) (RunResult, error) {
		<-ctx.Done()
		return RunResult{ExitCode: -1}, ctx.Err()
	}
	f.start()

	f.enqueue(testRequest(testJobA, RunnerCLI))
	res := f.notify.wait(t).res

	if res.Status != state.StatusTimedOut {
		t.Fatalf("Result.Status = %q, want %q (err %v)", res.Status, state.StatusTimedOut, res.Err)
	}
	if !errors.Is(res.Err, ErrTimeout) {
		t.Errorf("Result.Err = %v, want ErrTimeout", res.Err)
	}

	_, installs, aborts, _ := f.prep.snapshot()
	if len(installs) != 0 {
		t.Errorf("installs = %+v, want none for a timed-out job", installs)
	}
	if len(aborts) != 1 {
		t.Errorf("aborts = %v, want the timed-out job's sandbox", aborts)
	}
	if record := f.jobRecord(testJobA); record.Status != state.StatusTimedOut {
		t.Errorf("state status = %q, want %q", record.Status, state.StatusTimedOut)
	}
}

func TestRunnerErrorFailsWithoutRetry(t *testing.T) {
	f := newManagerFixture(t, nil)
	boom := errors.New("runner exploded")
	f.cli.runFn = func(ctx context.Context, req Request) (RunResult, error) {
		return RunResult{ExitCode: 2, Summary: "partial output"}, boom
	}
	f.api.runFn = func(ctx context.Context, req Request) (RunResult, error) {
		return RunResult{ExitCode: 0, Summary: "api output"}, nil
	}
	f.start()

	f.enqueue(testRequest(testJobA, RunnerCLI))
	res := f.notify.wait(t).res

	if res.Status != state.StatusFailed {
		t.Fatalf("Result.Status = %q, want %q", res.Status, state.StatusFailed)
	}
	if !errors.Is(res.Err, boom) {
		t.Errorf("Result.Err = %v, want %v", res.Err, boom)
	}
	if res.Summary != "partial output" {
		t.Errorf("Result.Summary = %q, want the runner's partial output", res.Summary)
	}

	cliRuns, _, _ := f.cli.snapshot()
	apiRuns, _, _ := f.api.snapshot()
	if len(cliRuns) != 1 {
		t.Errorf("CLI runs = %d, want 1 (no retry)", len(cliRuns))
	}
	if len(apiRuns) != 0 {
		t.Errorf("API runs = %d, want 0 (never retry with the other runner)", len(apiRuns))
	}

	_, installs, aborts, _ := f.prep.snapshot()
	if len(installs) != 0 {
		t.Errorf("installs = %+v, want none after a runner error", installs)
	}
	if len(aborts) != 1 {
		t.Errorf("aborts = %v, want the failed job's sandbox", aborts)
	}
	if record := f.jobRecord(testJobA); record.Status != state.StatusFailed {
		t.Errorf("state status = %q, want %q", record.Status, state.StatusFailed)
	}
}

func TestQueuedJobKeepsRunnerSnapshot(t *testing.T) {
	f := newManagerFixture(t, nil)

	aStarted := make(chan struct{})
	aRelease := make(chan struct{})
	var aOnce sync.Once
	f.cli.runFn = func(ctx context.Context, req Request) (RunResult, error) {
		aOnce.Do(func() { close(aStarted) })
		<-aRelease
		return RunResult{ExitCode: 0, Summary: "cli done"}, nil
	}
	f.api.runFn = func(ctx context.Context, req Request) (RunResult, error) {
		return RunResult{ExitCode: 0, Summary: "api done"}, nil
	}
	f.start()

	f.enqueue(testRequest(testJobA, RunnerCLI))
	waitUntil(t, "first job running", func() bool {
		select {
		case <-aStarted:
			return true
		default:
			return false
		}
	})

	queued := testRequest(testJobB, RunnerAPI)
	queued.Focus = "snapshot focus"
	queued.ModelAlias = "pro"
	f.enqueue(queued)

	status := f.mgr.Status()
	if len(status.Queued) != 1 {
		t.Fatalf("Queued = %v, want the API job", status.Queued)
	}
	if status.Queued[0].Runner != "api" || status.Queued[0].Model != "pro" {
		t.Errorf("Queued[0] = %+v, want the enqueued runner/model snapshot", status.Queued[0])
	}

	close(aRelease)
	first := f.notify.wait(t).res
	second := f.notify.wait(t).res
	if first.Request.ID != testJobA || second.Request.ID != testJobB {
		t.Fatalf("notification order = %s, %s, want A then B", first.Request.ID, second.Request.ID)
	}
	if second.Status != state.StatusSucceeded {
		t.Errorf("second Status = %q, want %q", second.Status, state.StatusSucceeded)
	}

	apiRuns, _, _ := f.api.snapshot()
	if len(apiRuns) != 1 || apiRuns[0].ID != testJobB || apiRuns[0].Runner != RunnerAPI {
		t.Fatalf("API runs = %+v, want only %s", apiRuns, testJobB)
	}
	if apiRuns[0].Focus != "snapshot focus" || apiRuns[0].ModelAlias != "pro" {
		t.Errorf("API request = %+v, want the enqueued snapshot", apiRuns[0])
	}
	if apiRuns[0].RunDir != f.prep.jobDir(testJobB) {
		t.Errorf("API RunDir = %q, want the job sandbox", apiRuns[0].RunDir)
	}
	if apiRuns[0].TargetDir != filepath.Join(f.prep.jobDir(testJobB), "repo") {
		t.Errorf("API TargetDir = %q, want the absolute clone path", apiRuns[0].TargetDir)
	}
	if apiRuns[0].Prompt != BuildPrompt("snapshot focus") {
		t.Errorf("API prompt = %q, want BuildPrompt(snapshot focus)", apiRuns[0].Prompt)
	}
	cliRuns, _, _ := f.cli.snapshot()
	if len(cliRuns) != 1 || cliRuns[0].ID != testJobA {
		t.Errorf("CLI runs = %+v, want only %s", cliRuns, testJobA)
	}
}

func TestManagerStopCancelsCurrentAndDrainsQueue(t *testing.T) {
	f := newManagerFixture(t, nil)

	started := make(chan struct{})
	var once sync.Once
	f.cli.runFn = blockUntilCancelled(started, &once)
	f.start()

	f.enqueue(testRequest(testJobA, RunnerCLI))
	waitUntil(t, "first job running", func() bool {
		select {
		case <-started:
			return true
		default:
			return false
		}
	})
	f.enqueue(testRequest(testJobB, RunnerCLI))
	f.enqueue(testRequest(testJobC, RunnerCLI))

	stopDone := make(chan struct{})
	go func() {
		defer close(stopDone)
		f.mgr.Stop(context.Background())
	}()
	select {
	case <-stopDone:
	case <-time.After(15 * time.Second):
		t.Fatal("Stop did not return within 15s")
	}

	if got := f.notify.wait(t); got.res.Status != state.StatusCancelled {
		t.Errorf("stopped job Status = %q, want %q", got.res.Status, state.StatusCancelled)
	}
	if pending := f.notify.pending(); pending != 0 {
		t.Errorf("notifications delivered = %d extra, want only the running job", pending)
	}

	status := f.mgr.Status()
	if status.Running != nil {
		t.Errorf("Status().Running = %+v after Stop, want nil", status.Running)
	}
	if len(status.Queued) != 0 {
		t.Errorf("Status().Queued = %v after Stop, want none", status.Queued)
	}

	runs, _, _ := f.cli.snapshot()
	if len(runs) != 1 || runs[0].ID != testJobA {
		t.Errorf("runs = %+v, want only the stopped job", runs)
	}
	if record := f.jobRecord(testJobA); record.Status != state.StatusCancelled {
		t.Errorf("state status = %q, want %q", record.Status, state.StatusCancelled)
	}

	if _, err := f.mgr.Enqueue(testRequest(testJobD, RunnerCLI)); !errors.Is(err, ErrManagerStopped) {
		t.Errorf("Enqueue after Stop error = %v, want ErrManagerStopped", err)
	}
}

func TestManagerNotifiesStartedBeforePrepare(t *testing.T) {
	start := newFakeStartNotifier()
	f := newManagerFixture(t, nil, withStartNotifier(start))
	f.start()

	f.enqueue(testRequest(testJobA, RunnerCLI))
	res := f.notify.wait(t).res
	if res.Status != state.StatusSucceeded {
		t.Fatalf("Result.Status = %q, want %q (err %v)", res.Status, state.StatusSucceeded, res.Err)
	}

	calls := start.started()
	if len(calls) != 1 {
		t.Fatalf("NotifyStarted calls = %d, want 1", len(calls))
	}
	got := calls[0]

	if got.req.ID != testJobA {
		t.Errorf("NotifyStarted request ID = %q, want %q", got.req.ID, testJobA)
	}
	if got.req.Target.ID != testTarget.ID {
		t.Errorf("NotifyStarted target ID = %q, want %q", got.req.Target.ID, testTarget.ID)
	}
	if got.req.ChatID != 42 {
		t.Errorf("NotifyStarted chat ID = %d, want 42", got.req.ChatID)
	}
	if got.req.RunDir != "" || got.req.Prompt != "" {
		t.Errorf("NotifyStarted saw a prepared request: RunDir=%q Prompt=%q", got.req.RunDir, got.req.Prompt)
	}
	if got.status != state.StatusRunning {
		t.Errorf("state at NotifyStarted = %q, want %q", got.status, state.StatusRunning)
	}
	if got.prepares != 0 {
		t.Errorf("Prepare calls before NotifyStarted = %d, want 0", got.prepares)
	}
	if got.ctx.Err() != nil {
		t.Errorf("NotifyStarted context already ended: %v", got.ctx.Err())
	}
	if _, hasDeadline := got.ctx.Deadline(); hasDeadline {
		t.Error("NotifyStarted received the job context, want the manager root context")
	}

	prepares, installs, _, _ := f.prep.snapshot()
	if len(prepares) != 1 || prepares[0] != testJobA {
		t.Errorf("prepares = %v, want [%s]", prepares, testJobA)
	}
	if len(installs) != 1 || installs[0].ID != testTarget.ID {
		t.Errorf("installs = %+v, want one install of %s", installs, testTarget.ID)
	}
}

func TestManagerWithoutStartNotifierStillRuns(t *testing.T) {
	f := newManagerFixture(t, nil)
	if _, ok := any(f.notifier).(StartNotifier); ok {
		t.Fatal("the Notify-only fake implements StartNotifier; this test would not cover the optional hook")
	}
	f.start()

	f.enqueue(testRequest(testJobA, RunnerCLI))
	res := f.notify.wait(t).res
	if res.Status != state.StatusSucceeded {
		t.Fatalf("Result.Status = %q, want %q (err %v)", res.Status, state.StatusSucceeded, res.Err)
	}

	prepares, installs, _, _ := f.prep.snapshot()
	if len(prepares) != 1 || prepares[0] != testJobA {
		t.Errorf("prepares = %v, want [%s]", prepares, testJobA)
	}
	if len(installs) != 1 || installs[0].ID != testTarget.ID {
		t.Errorf("installs = %+v, want one install of %s", installs, testTarget.ID)
	}
	if record := f.jobRecord(testJobA); record.Status != state.StatusSucceeded {
		t.Errorf("state status = %q, want %q", record.Status, state.StatusSucceeded)
	}
}
