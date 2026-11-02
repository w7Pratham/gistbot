package jobs

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/w7Pratham/gistbot/internal/config"
	"github.com/w7Pratham/gistbot/internal/repo"
	"github.com/w7Pratham/gistbot/internal/state"
)

const (
	// cancelTimeout bounds a runner's Cancel call. The CLI runner escalates
	// TERM to KILL inside this window.
	cancelTimeout = 10 * time.Second
	// shutdownWait bounds how long Stop waits for the worker to finish the
	// current job and deliver its notification.
	shutdownWait = 15 * time.Second

	logDirMode = os.FileMode(0o700)

	defaultJobTimeout = 20 * time.Minute
	defaultMaxQueue   = 3
)

// managerJobIDPattern keeps a job ID a single, non-hidden path component. The
// bot generates IDs, but Enqueue validates them so a malformed ID can never
// become a path inside WORK_DIR or LOG_DIR.
var managerJobIDPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`)

// docOrder is the canonical order used when persisting document names.
var docOrder = []string{"codemap", "routing", "observation"}

var (
	// ErrQueueFull is returned by Enqueue when every queue slot is taken.
	ErrQueueFull = errors.New("jobs: queue is full")
	// ErrManagerStopped is returned by Enqueue after Stop began.
	ErrManagerStopped = errors.New("jobs: manager is stopped")
	// errNoDocuments reports a run that installed nothing because the agent
	// wrote no gist document.
	errNoDocuments = errors.New("jobs: runner wrote no gist documents")
)

// Preparer clones and installs repositories for jobs. The production
// implementation is *repo.Cloner.
type Preparer interface {
	Prepare(ctx context.Context, t repo.Target, jobID string, log io.Writer) (repo.Prepared, error)
	Install(t repo.Target, repoDir string) error
	Abort(jobDir string) error
}

// Manager serializes all job execution through one worker. Start launches the
// worker; Enqueue admits a job; Cancel stops only the current job; Stop drains
// the queue and waits for the worker. Every lifecycle state write happens on
// the worker goroutine so state updates for a job can never race each other.
type Manager struct {
	cfg      config.Config
	state    *state.Store
	preparer Preparer
	runners  map[RunnerKind]Runner
	notifier Notifier
	log      *slog.Logger

	mu      sync.Mutex
	queue   []Request
	current *activeJob
	started bool
	stopped bool
	root    context.Context
	wake    chan struct{}
	done    chan struct{}
}

// activeJob is the job the worker is currently responsible for, from the pop
// until its notification has been delivered.
type activeJob struct {
	req       Request
	startedAt time.Time
	cancel    context.CancelFunc

	cancelRequested bool
	runDone         bool
	cancelDone      chan struct{}
}

// NewManager returns a single-worker job manager. The runners map and the
// configuration are copied so later caller mutation cannot change a running
// manager.
func NewManager(cfg config.Config, st *state.Store, preparer Preparer,
	runners map[RunnerKind]Runner, notifier Notifier, log *slog.Logger) *Manager {
	if cfg.MaxQueue <= 0 {
		cfg.MaxQueue = defaultMaxQueue
	}
	if cfg.JobTimeout <= 0 {
		cfg.JobTimeout = defaultJobTimeout
	}

	copied := make(map[RunnerKind]Runner, len(runners))
	for kind, runner := range runners {
		copied[kind] = runner
	}

	return &Manager{
		cfg:      cfg,
		state:    st,
		preparer: preparer,
		runners:  copied,
		notifier: notifier,
		log:      log,
		wake:     make(chan struct{}, 1),
		done:     make(chan struct{}),
	}
}

// Start creates the log directory and launches the worker. Only the first call
// has an effect. The supplied context is the manager's root: it bounds every
// job and is used for notifications so a cancelled job context cannot suppress
// its own failure or cancellation message.
func (m *Manager) Start(ctx context.Context) {
	if ctx == nil {
		ctx = context.Background()
	}

	m.mu.Lock()
	if m.started || m.stopped {
		m.mu.Unlock()
		return
	}
	m.started = true
	m.root = ctx
	m.mu.Unlock()

	if err := os.MkdirAll(m.cfg.LogDir, logDirMode); err != nil {
		m.logError("create job log directory", err)
	} else if err := os.Chmod(m.cfg.LogDir, logDirMode); err != nil {
		m.logError("set job log directory mode", err)
	}

	go m.worker(ctx)
}

// Enqueue validates and stores a job, returning its queue position. Position 0
// means the worker was idle and the job starts immediately; otherwise the
// position is 1-based in the queue. The queued record is persisted before
// Enqueue returns.
func (m *Manager) Enqueue(req Request) (int, error) {
	if !managerJobIDPattern.MatchString(req.ID) {
		return 0, errors.New("jobs: job ID is not a safe path component")
	}
	if req.Runner == "" {
		return 0, errors.New("jobs: runner is required")
	}
	if m.runners[req.Runner] == nil {
		return 0, fmt.Errorf("jobs: runner %q is not configured", req.Runner)
	}
	if !repo.ValidID(req.Target.ID) {
		return 0, errors.New("jobs: target ID is not canonical")
	}
	if m.state == nil {
		return 0, errors.New("jobs: state store is required")
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	if m.stopped {
		return 0, ErrManagerStopped
	}
	if m.root != nil && m.root.Err() != nil {
		m.stopped = true
		m.queue = nil
		return 0, ErrManagerStopped
	}
	if len(m.queue) >= m.cfg.MaxQueue {
		return 0, ErrQueueFull
	}

	record := state.JobRecord{
		ID:     req.ID,
		RepoID: req.Target.ID,
		URL:    req.Target.URL,
		Runner: string(req.Runner),
		Model:  modelLabel(req),
		Status: state.StatusQueued,
	}
	if err := m.state.UpsertJob(record); err != nil {
		return 0, fmt.Errorf("jobs: persist queued job: %w", err)
	}

	idle := m.current == nil && len(m.queue) == 0
	m.queue = append(m.queue, req)
	if idle {
		m.signal()
		return 0, nil
	}
	return len(m.queue), nil
}

// Status returns a snapshot of the running and queued jobs.
func (m *Manager) Status() Status {
	m.mu.Lock()
	defer m.mu.Unlock()

	now := time.Now()
	var status Status
	if m.current != nil {
		view := jobView(m.current.req)
		view.StartedAt = m.current.startedAt
		if elapsed := now.Sub(m.current.startedAt); elapsed > 0 {
			view.Elapsed = elapsed
		}
		status.Running = &view
	}
	for _, req := range m.queue {
		status.Queued = append(status.Queued, jobView(req))
	}
	return status
}

// Cancel requests cancellation of the current job only. It returns false when
// no job is running. Queued jobs are left alone; the next one starts after the
// cancelled job has fully finished.
func (m *Manager) Cancel(ctx context.Context) bool {
	m.mu.Lock()
	job := m.current
	m.mu.Unlock()
	if job == nil {
		return false
	}
	return m.cancelJob(ctx, job)
}

// Stop cancels the current job, clears the queue, and waits up to shutdownWait
// in total for the worker to finish and notify. The supplied context can end
// the wait sooner.
func (m *Manager) Stop(ctx context.Context) {
	if ctx == nil {
		ctx = context.Background()
	}
	deadline := time.Now().Add(shutdownWait)

	m.mu.Lock()
	m.stopped = true
	m.queue = nil
	job := m.current
	started := m.started
	m.mu.Unlock()

	m.signal()

	if job != nil && !m.cancelJob(ctx, job) {
		m.logf("stop: current job was already finishing", "job", job.req.ID)
	}
	if !started {
		return
	}

	remaining := time.Until(deadline)
	if remaining <= 0 {
		select {
		case <-m.done:
		default:
			m.logWarn("stop: timed out waiting for the worker")
		}
		return
	}

	timer := time.NewTimer(remaining)
	defer timer.Stop()
	select {
	case <-m.done:
	case <-ctx.Done():
	case <-timer.C:
		m.logWarn("stop: timed out waiting for the worker")
	}
}

func (m *Manager) worker(ctx context.Context) {
	defer close(m.done)

	for {
		m.mu.Lock()
		if m.stopped {
			m.mu.Unlock()
			return
		}
		if ctx.Err() != nil {
			m.stopped = true
			m.queue = nil
			m.mu.Unlock()
			return
		}
		if len(m.queue) == 0 {
			m.mu.Unlock()
			select {
			case <-m.wake:
			case <-ctx.Done():
			}
			continue
		}

		req := m.queue[0]
		m.queue[0] = Request{}
		m.queue = m.queue[1:]

		jobCtx, cancel := context.WithTimeout(ctx, m.cfg.JobTimeout)
		job := &activeJob{
			req:       req,
			startedAt: time.Now(),
			cancel:    cancel,
		}
		m.current = job
		m.mu.Unlock()

		m.process(jobCtx, job)

		// A Cancel call can still be inside runner.Cancel when Run returns.
		// Wait for it before clearing current so the next job can never share
		// a runner with an in-flight cancellation.
		m.settleCancel(job)
		cancel()

		m.mu.Lock()
		m.current = nil
		m.mu.Unlock()
	}
}

// process runs one job: prepare, fill the request, execute the selected
// runner, install documents atomically, and report. Every failure path keeps
// the previously installed repository.
func (m *Manager) process(ctx context.Context, job *activeJob) {
	req := job.req
	res := Result{
		Request:   req,
		StartedAt: job.startedAt,
		LogPath:   filepath.Join(m.cfg.LogDir, req.ID+".log"),
	}

	m.updateJob(req.ID, func(record *state.JobRecord) {
		record.Status = state.StatusRunning
		record.StartedAt = job.startedAt
	})

	// Optional start hook: the job is already recorded as running, so a
	// Notifier can edit its queued card in place. It is best-effort and uses
	// the root context so a cancelled job cannot suppress its start event.
	if starter, ok := m.notifier.(StartNotifier); ok {
		starter.NotifyStarted(m.notifyContext(), req)
	}

	if m.preparer == nil {
		m.finish(res, state.StatusFailed, errors.New("jobs: repository preparer is not configured"), repo.Docs{})
		return
	}

	logFile, err := openJobLog(res.LogPath)
	if err != nil {
		m.finish(res, state.StatusFailed, err, repo.Docs{})
		return
	}
	prepared, prepareErr := m.preparer.Prepare(ctx, req.Target, req.ID, logFile)
	_ = logFile.Close()
	if prepareErr != nil {
		status, outcomeErr := m.outcome(ctx, job, prepareErr)
		m.finish(res, status, outcomeErr, repo.Docs{})
		return
	}

	req.RunDir = prepared.JobDir
	req.TargetDir = prepared.RepoDir
	req.Prompt = BuildPrompt(req.Focus)
	res.Request = req

	runner := m.runners[req.Runner]
	if runner == nil {
		_ = m.preparer.Abort(prepared.JobDir)
		m.finish(res, state.StatusFailed, fmt.Errorf("jobs: runner %q is not configured", req.Runner), repo.Docs{})
		return
	}

	runResult, runErr := runner.Run(ctx, req, res.LogPath)
	res.Summary = runResult.Summary

	m.mu.Lock()
	job.runDone = true
	cancelled := job.cancelRequested
	m.mu.Unlock()

	if cancelled || errors.Is(runErr, ErrCancelled) {
		m.abort(prepared.JobDir)
		m.finish(res, state.StatusCancelled, ErrCancelled, repo.Docs{})
		return
	}
	if errors.Is(runErr, ErrTimeout) || isDeadline(ctx) {
		m.abort(prepared.JobDir)
		m.finish(res, state.StatusTimedOut, ErrTimeout, repo.Docs{})
		return
	}
	if runErr != nil {
		m.abort(prepared.JobDir)
		m.finish(res, state.StatusFailed, runErr, repo.Docs{})
		return
	}

	staged, err := repo.DiscoverDocs(prepared.RepoDir)
	if err != nil {
		m.abort(prepared.JobDir)
		m.finish(res, state.StatusFailed, err, repo.Docs{})
		return
	}
	if len(staged.Files) == 0 {
		m.abort(prepared.JobDir)
		m.finish(res, state.StatusFailed, errNoDocuments, staged)
		return
	}

	if err := m.preparer.Install(req.Target, prepared.RepoDir); err != nil {
		m.abort(prepared.JobDir)
		m.finish(res, state.StatusFailed, err, staged)
		return
	}
	// Install moved the clone out of the sandbox. Remove the now-empty job
	// directory; a leftover sandbox is harmless but Recover would clean it.
	if err := m.preparer.Abort(prepared.JobDir); err != nil {
		m.logWarn("abort job sandbox after install", "job", req.ID, "error", err)
	}

	final := staged
	if finalDir := repo.RepoDir(m.cfg.WorkDir, req.Target.ID); finalDir != "" {
		discovered, discoverErr := repo.DiscoverDocs(finalDir)
		if discoverErr != nil {
			m.logWarn("discover installed documents", "job", req.ID, "error", discoverErr)
			discovered = relocateDocs(staged, prepared.RepoDir, finalDir)
		}
		final = discovered
	}
	m.finish(res, state.StatusSucceeded, nil, final)
}

// finish records the terminal state and delivers the notification. It runs on
// the worker so the queued, running, and terminal writes for a job can never
// interleave.
func (m *Manager) finish(res Result, status state.JobStatus, err error, docs repo.Docs) {
	res.Status = status
	res.Err = err
	res.Docs = docs
	res.FinishedAt = time.Now()
	if res.FinishedAt.Before(res.StartedAt) {
		res.FinishedAt = res.StartedAt
	}

	m.updateJob(res.Request.ID, func(record *state.JobRecord) {
		record.Status = status
		record.FinishedAt = res.FinishedAt
		record.Docs = docNames(docs.Files)
		if err != nil {
			record.Error = err.Error()
		} else {
			record.Error = ""
		}
	})

	m.logf("job finished",
		"job", res.Request.ID,
		"repo", res.Request.Target.ID,
		"status", string(status),
		"duration", res.FinishedAt.Sub(res.StartedAt).String(),
	)

	if m.notifier != nil {
		m.notifier.Notify(m.notifyContext(), res)
	}
}

func (m *Manager) cancelJob(ctx context.Context, job *activeJob) bool {
	m.mu.Lock()
	if m.current != job || job.runDone {
		m.mu.Unlock()
		return false
	}
	if job.cancelRequested {
		m.mu.Unlock()
		return true
	}
	job.cancelRequested = true
	settled := make(chan struct{})
	job.cancelDone = settled
	runner := m.runners[job.req.Runner]
	cancel := job.cancel
	m.mu.Unlock()

	m.logf("cancel: stopping job", "job", job.req.ID)

	cancelCtx := context.Background()
	if ctx != nil {
		cancelCtx = context.WithoutCancel(ctx)
	}
	cancelCtx, finish := context.WithTimeout(cancelCtx, cancelTimeout)
	if runner != nil {
		if err := runner.Cancel(cancelCtx); err != nil {
			m.logWarn("cancel: runner cancel failed", "job", job.req.ID, "error", err)
		}
	}
	if cancel != nil {
		cancel()
	}
	finish()
	close(settled)
	return true
}

func (m *Manager) settleCancel(job *activeJob) {
	m.mu.Lock()
	settled := job.cancelDone
	m.mu.Unlock()
	if settled != nil {
		<-settled
	}
}

// outcome maps a preparation failure to a terminal status. A clone timeout is
// a failed job; only an expired job deadline or an explicit cancel changes the
// status.
func (m *Manager) outcome(ctx context.Context, job *activeJob, err error) (state.JobStatus, error) {
	m.mu.Lock()
	cancelled := job.cancelRequested
	m.mu.Unlock()

	switch {
	case cancelled, errors.Is(err, ErrCancelled):
		return state.StatusCancelled, ErrCancelled
	case isDeadline(ctx), errors.Is(err, ErrTimeout):
		return state.StatusTimedOut, ErrTimeout
	default:
		return state.StatusFailed, err
	}
}

func (m *Manager) abort(jobDir string) {
	if err := m.preparer.Abort(jobDir); err != nil {
		m.logWarn("abort job sandbox", "dir", jobDir, "error", err)
	}
}

func (m *Manager) updateJob(id string, fn func(*state.JobRecord)) {
	if m.state == nil {
		return
	}
	if err := m.state.UpdateJob(id, fn); err != nil {
		m.logWarn("update job state", "job", id, "error", err)
	}
}

func (m *Manager) notifyContext() context.Context {
	m.mu.Lock()
	root := m.root
	m.mu.Unlock()
	if root == nil {
		return context.Background()
	}
	return root
}

// signal wakes the worker without blocking. The channel holds one pending
// wake-up; extra signals are redundant because the worker re-checks the queue.
func (m *Manager) signal() {
	select {
	case m.wake <- struct{}{}:
	default:
	}
}

func (m *Manager) logf(msg string, args ...any) {
	if m.log != nil {
		m.log.Info(msg, args...)
	}
}

func (m *Manager) logWarn(msg string, args ...any) {
	if m.log != nil {
		m.log.Warn(msg, args...)
	}
}

func (m *Manager) logError(msg string, err error) {
	if m.log != nil {
		m.log.Error(msg, "error", err)
	}
}

func jobView(req Request) JobView {
	return JobView{
		ID:     req.ID,
		RepoID: req.Target.ID,
		Runner: string(req.Runner),
		Model:  modelLabel(req),
	}
}

func modelLabel(req Request) string {
	if req.ModelAlias != "" {
		return req.ModelAlias
	}
	if req.Model.ProviderID == "" && req.Model.ModelID == "" {
		return ""
	}
	return req.Model.String()
}

// docNames returns the present document names in canonical order.
func docNames(files map[string]string) []string {
	if len(files) == 0 {
		return nil
	}
	names := make([]string, 0, len(files))
	for _, name := range docOrder {
		if _, ok := files[name]; ok {
			names = append(names, name)
		}
	}
	return names
}

// relocateDocs rebases document paths from the staged sandbox to the installed
// repository. It is only used when discovery at the final path fails after a
// successful Install, so the notifier still reads files that exist.
func relocateDocs(docs repo.Docs, fromDir, toDir string) repo.Docs {
	moved := repo.Docs{
		Files:   make(map[string]string, len(docs.Files)),
		Missing: append([]string(nil), docs.Missing...),
	}
	for name, path := range docs.Files {
		rel, err := filepath.Rel(fromDir, path)
		if err != nil || rel == "." || rel == ".." || filepath.IsAbs(rel) ||
			strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			continue
		}
		moved.Files[name] = filepath.Join(toDir, rel)
	}
	return moved
}

func isDeadline(ctx context.Context) bool {
	err := ctx.Err()
	return err != nil && errors.Is(err, context.DeadlineExceeded)
}
