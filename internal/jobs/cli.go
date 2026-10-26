package jobs

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"syscall"
	"time"
)

const (
	// terminateGrace bounds the SIGTERM-to-SIGKILL escalation.
	terminateGrace = 10 * time.Second
	// postExitGrace bounds the wait for a private-server child to exit after
	// its parent has already gone.
	postExitGrace = 500 * time.Millisecond

	logFileMode  = 0o600
	tailLimit    = 16 << 10
	summaryLines = 5
)

// oscEscape, csiEscape, and controlChars strip terminal control sequences from
// the summary that is sent to Telegram.
var (
	oscEscape    = regexp.MustCompile(`\x1b\][^\x07\x1b]*(\x07|\x1b\\)`)
	csiEscape    = regexp.MustCompile(`\x1b\[[0-9;?]*[ -/]*[@-~]`)
	controlChars = regexp.MustCompile(`[\x00-\x08\x0b-\x1f\x7f]`)
)

// errRunnerBusy rejects a second concurrent Run. It is unexported so tests can
// match it without committing to the message.
var errRunnerBusy = errors.New("jobs: CLI runner is already running a job")

// CLIRunner executes a job with `opencode run --standalone` in its own process
// group so that cancellation also stops the private server OpenCode spawns.
// A CLIRunner runs one job at a time and is safe to call Cancel concurrently
// with Run.
type CLIRunner struct {
	bin string

	// grace is the SIGTERM-to-SIGKILL delay. It is a field so tests can
	// shorten it; production keeps terminateGrace.
	grace time.Duration

	mu              sync.Mutex
	running         bool
	cmd             *exec.Cmd
	pgid            int
	done            chan struct{}
	cancelCh        chan struct{}
	cancelRequested bool
}

// NewCLIRunner returns a runner that executes bin.
func NewCLIRunner(bin string) *CLIRunner {
	return &CLIRunner{bin: bin, grace: terminateGrace}
}

// Run executes one job. The child receives the fixed standalone repo-scout
// argv, runs with req.RunDir as its working directory, and writes stdout and
// stderr to logPath (created with mode 0600 if needed). Cancellation or a
// deadline kills the whole process group and returns ErrCancelled or
// ErrTimeout. A non-zero child exit is returned as an *exec.ExitError.
func (r *CLIRunner) Run(ctx context.Context, req Request, logPath string) (RunResult, error) {
	if ctx == nil {
		return RunResult{ExitCode: -1}, errors.New("jobs: context is nil")
	}
	if err := validateCLIRequest(r.bin, req, logPath); err != nil {
		return RunResult{ExitCode: -1}, err
	}

	// Reserve the runner before any child can start. A second concurrent Run
	// is rejected here, so it can never race on cmd/pgid.
	r.mu.Lock()
	if r.running {
		r.mu.Unlock()
		return RunResult{ExitCode: -1}, errRunnerBusy
	}
	r.running = true
	r.cancelRequested = false
	cancelCh := make(chan struct{})
	r.cancelCh = cancelCh
	r.mu.Unlock()

	defer func() {
		r.mu.Lock()
		r.running = false
		r.cmd = nil
		r.pgid = 0
		r.done = nil
		r.cancelCh = nil
		r.cancelRequested = false
		r.mu.Unlock()
	}()

	if ctx.Err() != nil {
		return RunResult{ExitCode: -1}, contextFailure(ctx)
	}

	logFile, err := openJobLog(logPath)
	if err != nil {
		return RunResult{ExitCode: -1}, err
	}
	defer logFile.Close()

	// The argv is fixed; no user-supplied string ever reaches a shell.
	args := []string{
		"run",
		"--standalone",
		"--agent",
		"repo-scout",
		"--model",
		req.Model.String(),
		req.Prompt,
	}
	cmd := exec.Command(r.bin, args...)
	cmd.Dir = req.RunDir
	cmd.Env = childEnv(os.Environ())
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	out := &jobOutput{log: logFile, tail: newTailBuffer(tailLimit)}
	pr, pw, err := os.Pipe()
	if err != nil {
		return RunResult{ExitCode: -1}, fmt.Errorf("jobs: create output pipe: %w", err)
	}
	// Give the child the write end directly. os/exec then does not add copy
	// goroutines that Wait would block on when a private-server child keeps
	// the descriptor open after the parent exits.
	cmd.Stdout = pw
	cmd.Stderr = pw

	if err := cmd.Start(); err != nil {
		pw.Close()
		pr.Close()
		return RunResult{ExitCode: -1}, fmt.Errorf("jobs: start opencode: %w", err)
	}
	pw.Close()

	copyDone := make(chan struct{})
	go func() {
		defer close(copyDone)
		if _, err := io.Copy(out, pr); err != nil {
			// The log failed. Keep draining so the child cannot block on a
			// full pipe; Run reports out.Err after the process is gone.
			_, _ = io.Copy(io.Discard, pr)
		}
	}()

	waitCh := make(chan error, 1)
	done := make(chan struct{})
	go func() {
		waitCh <- cmd.Wait()
		close(done)
	}()

	pgid := cmd.Process.Pid
	r.mu.Lock()
	r.cmd = cmd
	r.pgid = pgid
	r.done = done
	r.mu.Unlock()

	var waitErr error
	waited := false
	select {
	case waitErr = <-waitCh:
		waited = true
	case <-ctx.Done():
	case <-cancelCh:
	}

	if r.cancelWasRequested() || ctx.Err() != nil {
		// The job was cancelled or timed out. TERM the whole group, escalate
		// to KILL after the grace period, and reap the direct child.
		r.terminate(done)
		if !waited {
			waitErr = <-waitCh
		}
	} else {
		// OpenCode's private `serve --stdio` child may outlive a normal exit.
		// TERM the group once; ESRCH means there is nothing left to stop.
		r.stopGroupAfterExit(pgid, copyDone)
	}
	drainOutput(pr, copyDone)

	res := RunResult{ExitCode: -1, Summary: summarize(out.tail.String())}
	if cmd.ProcessState != nil {
		res.ExitCode = cmd.ProcessState.ExitCode()
	}

	if r.cancelWasRequested() {
		return res, ErrCancelled
	}
	if ctxErr := ctx.Err(); ctxErr != nil {
		if errors.Is(ctxErr, context.DeadlineExceeded) {
			return res, ErrTimeout
		}
		return res, ErrCancelled
	}
	if waitErr != nil {
		return res, waitErr
	}
	if outErr := out.Err(); outErr != nil {
		return res, fmt.Errorf("jobs: write job log: %w", outErr)
	}
	return res, nil
}

// Cancel stops the job that Run is currently executing. It TERMs the process
// group, waits up to the runner's grace period (or ctx, if sooner), then KILLs
// the group and waits for the child to be reaped. It also wakes a Run that is
// blocked waiting for normal completion. When no job is running it is a no-op.
func (r *CLIRunner) Cancel(ctx context.Context) error {
	r.mu.Lock()
	r.cancelRequested = true
	if r.cancelCh != nil {
		// Wake Run even if the caller never cancels Run's context. Closing is
		// idempotent because the whole block runs under r.mu.
		select {
		case <-r.cancelCh:
		default:
			close(r.cancelCh)
		}
	}
	pgid := r.pgid
	done := r.done
	r.mu.Unlock()

	if pgid <= 0 {
		return nil
	}
	if err := syscall.Kill(-pgid, syscall.SIGTERM); err != nil && !errors.Is(err, syscall.ESRCH) {
		return fmt.Errorf("jobs: signal CLI process group: %w", err)
	}
	if done == nil {
		return nil
	}

	// Cancel owns the full TERM -> grace -> KILL escalation. It must not
	// depend on the caller also cancelling Run's context: the caller's context
	// can only make the escalation happen sooner.
	var stop <-chan struct{}
	if ctx != nil {
		stop = ctx.Done()
	}
	r.escalate(pgid, done, stop)
	return nil
}

// terminate stops the whole process group, escalating to SIGKILL after the
// grace period, and waits for the direct child to be reaped. Watching the group
// rather than only the parent catches a detached private-server child that
// ignores SIGTERM.
func (r *CLIRunner) terminate(done <-chan struct{}) {
	pgid := r.processGroup()
	if pgid <= 0 {
		return
	}
	_ = syscall.Kill(-pgid, syscall.SIGTERM)
	r.escalate(pgid, done, nil)
}

// escalate waits for every member of pgid's process group to exit, sending
// SIGKILL when the grace period or stop expires. SIGTERM must already have been
// sent. The direct child is always reaped through done.
func (r *CLIRunner) escalate(pgid int, done <-chan struct{}, stop <-chan struct{}) {
	deadline := time.NewTimer(r.grace)
	defer deadline.Stop()
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()

	reaped := false
	for groupAlive(pgid) {
		var waitDone <-chan struct{}
		if !reaped {
			waitDone = done
		}
		select {
		case <-waitDone:
			reaped = true
		case <-stop:
			_ = syscall.Kill(-pgid, syscall.SIGKILL)
			if !reaped {
				<-done
			}
			return
		case <-deadline.C:
			_ = syscall.Kill(-pgid, syscall.SIGKILL)
			if !reaped {
				<-done
			}
			return
		case <-ticker.C:
		}
	}
	if !reaped {
		<-done
	}
}

// stopGroupAfterExit TERMs a group left behind by the CLI parent and waits
// briefly for its output pipe to close. It never escalates to SIGKILL: a
// normal exit must not kill unrelated work.
func (r *CLIRunner) stopGroupAfterExit(pgid int, copyDone <-chan struct{}) {
	if pgid <= 0 {
		return
	}
	_ = syscall.Kill(-pgid, syscall.SIGTERM)
	timer := time.NewTimer(postExitGrace)
	defer timer.Stop()
	select {
	case <-copyDone:
	case <-timer.C:
	}
}

// cancelWasRequested reports whether Cancel was called for the running job.
func (r *CLIRunner) cancelWasRequested() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.cancelRequested
}

func (r *CLIRunner) processGroup() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.pgid
}

// groupAlive reports whether any process remains in pgid's process group.
func groupAlive(pgid int) bool {
	if pgid <= 0 {
		return false
	}
	err := syscall.Kill(-pgid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}

// drainOutput waits for the output copier to finish. A detached descendant
// may hold the pipe open forever, so the read end is closed after a short
// grace period and the copier is then given the chance to stop.
func drainOutput(pr *os.File, copyDone <-chan struct{}) {
	select {
	case <-copyDone:
	case <-time.After(postExitGrace):
		_ = pr.Close()
		<-copyDone
	}
	_ = pr.Close()
}

func contextFailure(ctx context.Context) error {
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return ErrTimeout
	}
	return ErrCancelled
}

func validateCLIRequest(bin string, req Request, logPath string) error {
	if strings.TrimSpace(bin) == "" {
		return errors.New("jobs: opencode binary is required")
	}
	if req.Model.ProviderID == "" || req.Model.ModelID == "" {
		return errors.New("jobs: model reference is required")
	}
	if strings.TrimSpace(req.Prompt) == "" {
		return errors.New("jobs: prompt is required")
	}
	if req.RunDir == "" || !filepath.IsAbs(req.RunDir) {
		return errors.New("jobs: run directory must be an absolute path")
	}
	if logPath == "" || !filepath.IsAbs(logPath) {
		return errors.New("jobs: log path must be an absolute path")
	}
	return nil
}

func openJobLog(path string) (*os.File, error) {
	file, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, logFileMode)
	if err != nil {
		return nil, fmt.Errorf("jobs: open job log: %w", err)
	}
	// O_CREATE's mode is ignored for an existing file, and the manager may
	// have created it earlier; enforce the private mode every time.
	if err := file.Chmod(logFileMode); err != nil {
		file.Close()
		return nil, fmt.Errorf("jobs: set job log mode: %w", err)
	}
	return file, nil
}

// childEnv removes bot credentials and Telegram configuration from the
// environment the OpenCode child inherits. FAKE_* test hooks survive.
func childEnv(environ []string) []string {
	out := make([]string, 0, len(environ))
	for _, entry := range environ {
		key, _, _ := strings.Cut(entry, "=")
		if strings.HasPrefix(key, "TELEGRAM_") {
			continue
		}
		switch key {
		case "OWNER_USER_ID",
			"OPENCODE_PASSWORD",
			"OPENCODE_SERVER_PASSWORD",
			"OPENCODE_API_PASSWORD":
			continue
		}
		out = append(out, entry)
	}
	return out
}

// jobOutput tees child output to the job log and a bounded tail buffer.
type jobOutput struct {
	mu   sync.Mutex
	log  io.Writer
	tail *tailBuffer
	err  error
}

func (o *jobOutput) Write(p []byte) (int, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.err != nil {
		return 0, o.err
	}
	n, err := o.log.Write(p)
	if err != nil {
		o.err = err
		return n, err
	}
	_, _ = o.tail.Write(p[:n])
	return n, nil
}

func (o *jobOutput) Err() error {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.err
}

// tailBuffer keeps at most max bytes of the most recent output, trimming at a
// line boundary when possible.
type tailBuffer struct {
	mu  sync.Mutex
	max int
	buf []byte
}

func newTailBuffer(max int) *tailBuffer {
	return &tailBuffer{max: max}
}

func (t *tailBuffer) Write(p []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.buf = append(t.buf, p...)
	if over := len(t.buf) - t.max; over > 0 {
		cut := over
		if i := bytes.IndexByte(t.buf[cut:], '\n'); i >= 0 {
			cut += i + 1
		}
		t.buf = append(t.buf[:0], t.buf[cut:]...)
	}
	return len(p), nil
}

func (t *tailBuffer) String() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return string(t.buf)
}

// summarize returns the last summaryLines non-empty lines with terminal escape
// sequences and control characters removed.
func summarize(raw string) string {
	lines := strings.Split(stripControl(raw), "\n")
	kept := make([]string, 0, summaryLines)
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		kept = append(kept, line)
	}
	if len(kept) > summaryLines {
		kept = kept[len(kept)-summaryLines:]
	}
	return strings.Join(kept, "\n")
}

func stripControl(s string) string {
	s = oscEscape.ReplaceAllString(s, "")
	s = csiEscape.ReplaceAllString(s, "")
	s = controlChars.ReplaceAllString(s, "")
	return s
}
