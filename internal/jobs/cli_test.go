package jobs

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/w7Pratham/gistbot/internal/config"
	"github.com/w7Pratham/gistbot/internal/repo"
)

func fakeOpenCodePath(t *testing.T) string {
	t.Helper()
	path, err := filepath.Abs(filepath.Join("testdata", "fake-opencode.sh"))
	if err != nil {
		t.Fatalf("resolve fake opencode path: %v", err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat fake opencode: %v", err)
	}
	if info.Mode().Perm()&0o111 == 0 {
		t.Fatalf("fake opencode fixture is not executable")
	}
	return path
}

func mustModel(t *testing.T, raw string) config.ModelRef {
	t.Helper()
	model, err := config.ParseModelRef(raw)
	if err != nil {
		t.Fatalf("parse model %q: %v", raw, err)
	}
	return model
}

func writeScript(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "fake-opencode.sh")
	if err := os.WriteFile(path, []byte(body), 0o700); err != nil {
		t.Fatalf("write fake script: %v", err)
	}
	return path
}

func readFileString(t *testing.T, path string) string {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(raw)
}

func fileLines(t *testing.T, path string) []string {
	t.Helper()
	raw := strings.TrimSuffix(readFileString(t, path), "\n")
	if raw == "" {
		return nil
	}
	return strings.Split(raw, "\n")
}

func parseEnvFile(t *testing.T, path string) map[string]string {
	t.Helper()
	env := make(map[string]string)
	for _, line := range fileLines(t, path) {
		key, value, ok := strings.Cut(line, "=")
		if ok {
			env[key] = value
		}
	}
	return env
}

func processGone(pid int) bool {
	if err := syscall.Kill(pid, 0); errors.Is(err, syscall.ESRCH) {
		return true
	}
	// A zombie still answers kill(pid, 0) until it is reaped.
	raw, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), "stat"))
	if err != nil {
		return false
	}
	fields := strings.Fields(string(raw))
	return len(fields) > 2 && fields[2] == "Z"
}

func waitForPIDFile(t *testing.T, path string, timeout time.Duration) int {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		raw, err := os.ReadFile(path)
		if err == nil {
			if pid, convErr := strconv.Atoi(strings.TrimSpace(string(raw))); convErr == nil && pid > 0 {
				return pid
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for pid file %s", path)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func waitForGone(pid int, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for {
		if processGone(pid) {
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// cliFixture builds an isolated job sandbox with a ./repo target.
type cliFixture struct {
	runDir  string
	repoDir string
	docsDir string
	logPath string
	bin     string
	req     Request
}

func newCLIFixture(t *testing.T) *cliFixture {
	t.Helper()
	runDir := t.TempDir()
	repoDir := filepath.Join(runDir, "repo")
	if err := os.MkdirAll(repoDir, 0o700); err != nil {
		t.Fatalf("create repo dir: %v", err)
	}

	f := &cliFixture{
		runDir:  runDir,
		repoDir: repoDir,
		docsDir: filepath.Join(repoDir, "docs"),
		logPath: filepath.Join(t.TempDir(), "job-1.log"),
		bin:     fakeOpenCodePath(t),
	}
	f.req = Request{
		ID:         "job-1",
		Target:     repo.Target{URL: "https://github.com/owner/repo", Host: "github.com", Owner: "owner", Name: "repo", ID: "github__owner__repo"},
		ChatID:     42,
		Runner:     RunnerCLI,
		Model:      mustModel(t, "deepseek/deepseek-flash#repo-scout"),
		ModelAlias: "flash",
		EnqueuedAt: time.Now(),
		RunDir:     runDir,
		TargetDir:  repoDir,
		Prompt:     "Target repo: ./repo\nUse the repo-gist skill. Focus: none.",
	}
	t.Setenv("FAKE_DOCS_DIR", f.docsDir)
	return f
}

func TestCLIRunnerUsesStandaloneAgentAndModel(t *testing.T) {
	f := newCLIFixture(t)
	argsPath := filepath.Join(t.TempDir(), "args")
	t.Setenv("FAKE_ARGS", argsPath)
	// The fixture records one argument per line, so keep this prompt on one line.
	f.req.Prompt = "Target repo: ./repo Use the repo-gist skill."

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	res, err := NewCLIRunner(f.bin).Run(ctx, f.req, f.logPath)
	if err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	if res.ExitCode != 0 {
		t.Fatalf("ExitCode = %d, want 0", res.ExitCode)
	}

	got := fileLines(t, argsPath)
	want := []string{
		"run",
		"--standalone",
		"--agent",
		"repo-scout",
		"--model",
		"deepseek/deepseek-flash#repo-scout",
		f.req.Prompt,
	}
	if len(got) != len(want) {
		t.Fatalf("argv has %d entries (%q), want %d", len(got), got, len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("argv[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

func TestCLIRunnerFiltersTelegramTokenFromEnv(t *testing.T) {
	f := newCLIFixture(t)
	envPath := filepath.Join(t.TempDir(), "env")
	t.Setenv("FAKE_ENV", envPath)
	t.Setenv("FAKE_TEST_HOOK", "keep-me")

	forbidden := map[string]string{
		"TELEGRAM_BOT_TOKEN":       "123456:fixture-token",
		"TELEGRAM_API_URL":         "https://telegram.invalid",
		"TELEGRAM_CHAT_ID":         "42",
		"OWNER_USER_ID":            "42",
		"OPENCODE_PASSWORD":        "fixture-server-password",
		"OPENCODE_SERVER_PASSWORD": "fixture-server-password-2",
		"OPENCODE_API_PASSWORD":    "fixture-api-password",
	}
	for key, value := range forbidden {
		t.Setenv(key, value)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	res, err := NewCLIRunner(f.bin).Run(ctx, f.req, f.logPath)
	if err != nil {
		t.Fatalf("Run returned error: %v", err)
	}

	env := parseEnvFile(t, envPath)
	for key := range forbidden {
		if _, ok := env[key]; ok {
			t.Errorf("%s was passed to the child environment", key)
		}
	}
	if got := env["FAKE_TEST_HOOK"]; got != "keep-me" {
		t.Errorf("FAKE_TEST_HOOK = %q, want keep-me", got)
	}
	if _, ok := env["PATH"]; !ok {
		t.Errorf("PATH was dropped from the child environment")
	}

	log := readFileString(t, f.logPath)
	for key, value := range forbidden {
		if strings.Contains(log, value) || strings.Contains(res.Summary, value) {
			t.Errorf("the value of %s appeared in the job log or summary", key)
		}
	}
}

func TestCLIRunnerSuccessReturnsSummaryAndLog(t *testing.T) {
	f := newCLIFixture(t)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	res, err := NewCLIRunner(f.bin).Run(ctx, f.req, f.logPath)
	if err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	if res.ExitCode != 0 {
		t.Errorf("ExitCode = %d, want 0", res.ExitCode)
	}

	log := readFileString(t, f.logPath)
	for _, want := range []string{"Gist complete", "Files: codemap.md routing.md observation.md"} {
		if !strings.Contains(res.Summary, want) {
			t.Errorf("Summary %q does not contain %q", res.Summary, want)
		}
		if !strings.Contains(log, want) {
			t.Errorf("log does not contain %q", want)
		}
	}
	for _, name := range []string{"codemap.md", "routing.md", "observation.md"} {
		if _, err := os.Stat(filepath.Join(f.docsDir, name)); err != nil {
			t.Errorf("expected %s to be written: %v", name, err)
		}
	}
}

func TestCLIRunnerFailureReturnsExitError(t *testing.T) {
	f := newCLIFixture(t)
	t.Setenv("FAKE_EXIT", "3")

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	res, err := NewCLIRunner(f.bin).Run(ctx, f.req, f.logPath)
	if err == nil {
		t.Fatal("Run returned a nil error, want an exit error")
	}
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) {
		t.Fatalf("Run error is %T, want *exec.ExitError", err)
	}
	if res.ExitCode != 3 {
		t.Errorf("ExitCode = %d, want 3", res.ExitCode)
	}
	if errors.Is(err, ErrCancelled) || errors.Is(err, ErrTimeout) {
		t.Errorf("failure was reported as cancellation or timeout")
	}
	if !strings.Contains(res.Summary, "Gist complete") {
		t.Errorf("Summary %q lost the child output", res.Summary)
	}
}

func TestCLIRunnerCancelKillsProcessGroupAndChild(t *testing.T) {
	f := newCLIFixture(t)
	t.Setenv("FAKE_MODE", "sleep")
	childPath := filepath.Join(t.TempDir(), "child.pid")
	t.Setenv("FAKE_CHILD", childPath)

	runner := NewCLIRunner(f.bin)
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = runner.Cancel(ctx)
	})

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	type outcome struct {
		res RunResult
		err error
	}
	done := make(chan outcome, 1)
	go func() {
		res, err := runner.Run(ctx, f.req, f.logPath)
		done <- outcome{res: res, err: err}
	}()

	childPID := waitForPIDFile(t, childPath, 10*time.Second)

	cancelCtx, cancelCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancelCancel()
	if err := runner.Cancel(cancelCtx); err != nil {
		t.Fatalf("Cancel returned error: %v", err)
	}

	select {
	case out := <-done:
		if !errors.Is(out.err, ErrCancelled) {
			t.Fatalf("Run error = %v, want ErrCancelled", out.err)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("Run did not return after Cancel")
	}
	if !waitForGone(childPID, 5*time.Second) {
		t.Errorf("fake child process %d survived cancellation", childPID)
	}
}

func TestCLIRunnerTimeoutKillsProcessGroup(t *testing.T) {
	f := newCLIFixture(t)
	t.Setenv("FAKE_MODE", "sleep")
	childPath := filepath.Join(t.TempDir(), "child.pid")
	t.Setenv("FAKE_CHILD", childPath)

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	start := time.Now()
	_, err := NewCLIRunner(f.bin).Run(ctx, f.req, f.logPath)
	if !errors.Is(err, ErrTimeout) {
		t.Fatalf("Run error = %v, want ErrTimeout", err)
	}
	if elapsed := time.Since(start); elapsed > 15*time.Second {
		t.Errorf("Run took %s after the deadline", elapsed)
	}

	childPID := waitForPIDFile(t, childPath, 5*time.Second)
	if !waitForGone(childPID, 5*time.Second) {
		t.Errorf("fake child process %d survived the timeout", childPID)
	}
}

func TestCLIRunnerLogIsPrivate(t *testing.T) {
	t.Run("new file", func(t *testing.T) {
		f := newCLIFixture(t)
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if _, err := NewCLIRunner(f.bin).Run(ctx, f.req, f.logPath); err != nil {
			t.Fatalf("Run returned error: %v", err)
		}
		info, err := os.Stat(f.logPath)
		if err != nil {
			t.Fatalf("stat log: %v", err)
		}
		if perm := info.Mode().Perm(); perm != 0o600 {
			t.Errorf("log mode = %o, want 600", perm)
		}
	})

	t.Run("existing file", func(t *testing.T) {
		f := newCLIFixture(t)
		if err := os.WriteFile(f.logPath, []byte("clone output\n"), 0o644); err != nil {
			t.Fatalf("seed log: %v", err)
		}
		if err := os.Chmod(f.logPath, 0o644); err != nil {
			t.Fatalf("chmod seed log: %v", err)
		}
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if _, err := NewCLIRunner(f.bin).Run(ctx, f.req, f.logPath); err != nil {
			t.Fatalf("Run returned error: %v", err)
		}
		info, err := os.Stat(f.logPath)
		if err != nil {
			t.Fatalf("stat log: %v", err)
		}
		if perm := info.Mode().Perm(); perm != 0o600 {
			t.Errorf("log mode = %o, want 600", perm)
		}
	})
}

func TestCLIRunnerSummaryStripsControlSequences(t *testing.T) {
	f := newCLIFixture(t)
	script := writeScript(t, `#!/bin/sh
printf '%b' '\033[1;31mred\033[0m ans\n'
printf 'progress\rcleared\n'
printf 'line1\nline2\nline3\nline4\nline5\nline6\n'
`)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	res, err := NewCLIRunner(script).Run(ctx, f.req, f.logPath)
	if err != nil {
		t.Fatalf("Run returned error: %v", err)
	}

	want := "line2\nline3\nline4\nline5\nline6"
	if res.Summary != want {
		t.Errorf("Summary = %q, want %q", res.Summary, want)
	}
}

func TestCLIRunnerTeesStderrToLog(t *testing.T) {
	f := newCLIFixture(t)
	script := writeScript(t, `#!/bin/sh
printf 'to stderr\n' >&2
printf 'to stdout\n'
`)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	res, err := NewCLIRunner(script).Run(ctx, f.req, f.logPath)
	if err != nil {
		t.Fatalf("Run returned error: %v", err)
	}

	log := readFileString(t, f.logPath)
	for _, want := range []string{"to stderr", "to stdout"} {
		if !strings.Contains(log, want) {
			t.Errorf("log does not contain %q", want)
		}
		if !strings.Contains(res.Summary, want) {
			t.Errorf("Summary %q does not contain %q", res.Summary, want)
		}
	}
}

func TestCLIRunnerKillsGroupAfterNormalExit(t *testing.T) {
	f := newCLIFixture(t)
	childPath := filepath.Join(t.TempDir(), "child.pid")
	t.Setenv("FAKE_CHILD", childPath)
	script := writeScript(t, `#!/bin/sh
sleep 300 &
echo $! > "$FAKE_CHILD"
echo "parent done"
`)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	res, err := NewCLIRunner(script).Run(ctx, f.req, f.logPath)
	if err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	if !strings.Contains(res.Summary, "parent done") {
		t.Errorf("Summary = %q, want the child output", res.Summary)
	}

	childPID := waitForPIDFile(t, childPath, 5*time.Second)
	if !waitForGone(childPID, 5*time.Second) {
		t.Errorf("private-server child %d survived a normal exit", childPID)
	}
}

func TestCLIRunnerKillsProcessThatIgnoresSIGTERM(t *testing.T) {
	f := newCLIFixture(t)
	childPath := filepath.Join(t.TempDir(), "child.pid")
	t.Setenv("FAKE_CHILD", childPath)
	script := writeScript(t, `#!/bin/sh
trap '' TERM
echo $$ > "$FAKE_CHILD"
while :; do sleep 1; done
`)

	runner := NewCLIRunner(script)
	runner.grace = 100 * time.Millisecond

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	done := make(chan error, 1)
	go func() {
		_, err := runner.Run(ctx, f.req, f.logPath)
		done <- err
	}()

	pid := waitForPIDFile(t, childPath, 5*time.Second)
	cancel()

	select {
	case err := <-done:
		if !errors.Is(err, ErrCancelled) {
			t.Fatalf("Run error = %v, want ErrCancelled", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return after the SIGKILL grace period")
	}
	if !waitForGone(pid, 5*time.Second) {
		t.Errorf("process %d survived SIGKILL", pid)
	}
}

// TestCLIRunnerCancelEscalatesToSIGKILL covers Cancel on its own: the Run
// context is never cancelled, so only Cancel's TERM -> grace -> KILL sequence
// can stop a child that ignores SIGTERM.
func TestCLIRunnerCancelEscalatesToSIGKILL(t *testing.T) {
	f := newCLIFixture(t)
	childPath := filepath.Join(t.TempDir(), "child.pid")
	t.Setenv("FAKE_CHILD", childPath)
	script := writeScript(t, `#!/bin/sh
trap '' TERM
echo $$ > "$FAKE_CHILD"
while :; do sleep 1; done
`)

	runner := NewCLIRunner(script)
	runner.grace = 100 * time.Millisecond
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = runner.Cancel(ctx)
	})

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	runDone := make(chan error, 1)
	go func() {
		_, err := runner.Run(ctx, f.req, f.logPath)
		runDone <- err
	}()

	pid := waitForPIDFile(t, childPath, 5*time.Second)

	// A background context must not be needed to make Cancel escalate.
	cancelDone := make(chan error, 1)
	start := time.Now()
	go func() {
		cancelDone <- runner.Cancel(context.Background())
	}()
	select {
	case err := <-cancelDone:
		if err != nil {
			t.Fatalf("Cancel returned error: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Cancel did not return; it never escalated to SIGKILL")
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Errorf("Cancel took %s, want less than the 5s bound", elapsed)
	}

	select {
	case err := <-runDone:
		if !errors.Is(err, ErrCancelled) {
			t.Fatalf("Run error = %v, want ErrCancelled", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return after Cancel woke it")
	}
	if !waitForGone(pid, 5*time.Second) {
		t.Errorf("process %d survived Cancel", pid)
	}
}

func TestCLIRunnerRejectsConcurrentRun(t *testing.T) {
	f := newCLIFixture(t)
	t.Setenv("FAKE_MODE", "sleep")
	childPath := filepath.Join(t.TempDir(), "child.pid")
	t.Setenv("FAKE_CHILD", childPath)

	runner := NewCLIRunner(f.bin)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	t.Cleanup(func() {
		cctx, ccancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer ccancel()
		_ = runner.Cancel(cctx)
	})

	logDir := t.TempDir()
	logs := [2]string{
		filepath.Join(logDir, "job-1.log"),
		filepath.Join(logDir, "job-2.log"),
	}

	type outcome struct {
		log string
		err error
	}
	start := make(chan struct{})
	results := make(chan outcome, len(logs))
	for i := range logs {
		go func(logPath string) {
			<-start
			_, err := runner.Run(ctx, f.req, logPath)
			results <- outcome{log: logPath, err: err}
		}(logs[i])
	}
	// Release both calls together so they race for the single-job guard.
	close(start)

	var rejected outcome
	select {
	case rejected = <-results:
	case <-time.After(10 * time.Second):
		t.Fatal("neither concurrent Run was rejected")
	}
	if !errors.Is(rejected.err, errRunnerBusy) {
		t.Fatalf("first finished Run error = %v, want errRunnerBusy", rejected.err)
	}

	// The active call must be running now.
	childPID := waitForPIDFile(t, childPath, 10*time.Second)

	// The rejected call must not even have opened its log.
	existing := 0
	for _, logPath := range logs {
		if _, err := os.Stat(logPath); err == nil {
			existing++
		}
	}
	if existing != 1 {
		t.Errorf("%d job logs exist, want only the active job's log", existing)
	}

	cancelCtx, cancelCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancelCancel()
	if err := runner.Cancel(cancelCtx); err != nil {
		t.Fatalf("Cancel returned error: %v", err)
	}

	select {
	case active := <-results:
		if !errors.Is(active.err, ErrCancelled) {
			t.Fatalf("active Run error = %v, want ErrCancelled", active.err)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("active Run did not return after Cancel")
	}
	if !waitForGone(childPID, 5*time.Second) {
		t.Errorf("fake child process %d survived cancellation", childPID)
	}
}
