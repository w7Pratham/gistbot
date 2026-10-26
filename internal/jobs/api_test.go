package jobs

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"gistbot/internal/opencode"
)

const apiFakeSessionID = "ses_api_fixture"

// apiFakeClient is a scriptable APIClient. Every hook is optional; the
// defaults describe a healthy server and a successful session.
type apiFakeClient struct {
	mu sync.Mutex

	calls     []string
	infoCalls int

	infoHook      func(context.Context, int) error
	createHook    func(context.Context, string, string, string, string, string) (string, error)
	promptHook    func(context.Context, string, string) error
	waitHook      func(context.Context, string) error
	interruptHook func(context.Context, string) (bool, error)
	messagesHook  func(context.Context, string) ([]opencode.Message, error)

	createLocation  string
	createAgent     string
	createProvider  string
	createModel     string
	createVariant   string
	promptSession   string
	promptText      string
	waitSession     string
	messageSession  string
	interruptTarget string
	interruptCtxErr error
	interruptDeadl  time.Time
	interruptHasDL  bool
}

func (f *apiFakeClient) record(name string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, name)
}

func (f *apiFakeClient) countCall(name string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	count := 0
	for _, call := range f.calls {
		if call == name {
			count++
		}
	}
	return count
}

func (f *apiFakeClient) onInfo(hook func(context.Context, int) error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.infoHook = hook
}

func (f *apiFakeClient) onCreateSession(hook func(context.Context, string, string, string, string, string) (string, error)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.createHook = hook
}

func (f *apiFakeClient) onPrompt(hook func(context.Context, string, string) error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.promptHook = hook
}

func (f *apiFakeClient) onWait(hook func(context.Context, string) error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.waitHook = hook
}

func (f *apiFakeClient) onInterrupt(hook func(context.Context, string) (bool, error)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.interruptHook = hook
}

func (f *apiFakeClient) onMessages(hook func(context.Context, string) ([]opencode.Message, error)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.messagesHook = hook
}

func (f *apiFakeClient) Info(ctx context.Context) error {
	f.mu.Lock()
	f.calls = append(f.calls, "info")
	f.infoCalls++
	call := f.infoCalls
	hook := f.infoHook
	f.mu.Unlock()
	if hook != nil {
		return hook(ctx, call)
	}
	return nil
}

func (f *apiFakeClient) CreateSession(ctx context.Context, location, agent, providerID, modelID, variant string) (string, error) {
	f.mu.Lock()
	f.calls = append(f.calls, "create")
	f.createLocation = location
	f.createAgent = agent
	f.createProvider = providerID
	f.createModel = modelID
	f.createVariant = variant
	hook := f.createHook
	f.mu.Unlock()
	if hook != nil {
		return hook(ctx, location, agent, providerID, modelID, variant)
	}
	return apiFakeSessionID, nil
}

func (f *apiFakeClient) Prompt(ctx context.Context, sessionID, text string) error {
	f.mu.Lock()
	f.calls = append(f.calls, "prompt")
	f.promptSession = sessionID
	f.promptText = text
	hook := f.promptHook
	f.mu.Unlock()
	if hook != nil {
		return hook(ctx, sessionID, text)
	}
	return nil
}

func (f *apiFakeClient) Wait(ctx context.Context, sessionID string) error {
	f.mu.Lock()
	f.calls = append(f.calls, "wait")
	f.waitSession = sessionID
	hook := f.waitHook
	f.mu.Unlock()
	if hook != nil {
		return hook(ctx, sessionID)
	}
	return nil
}

func (f *apiFakeClient) Interrupt(ctx context.Context, sessionID string) (bool, error) {
	f.mu.Lock()
	f.calls = append(f.calls, "interrupt")
	f.interruptTarget = sessionID
	f.interruptCtxErr = ctx.Err()
	f.interruptDeadl, f.interruptHasDL = ctx.Deadline()
	hook := f.interruptHook
	f.mu.Unlock()
	if hook != nil {
		return hook(ctx, sessionID)
	}
	return true, nil
}

func (f *apiFakeClient) Messages(ctx context.Context, sessionID string) ([]opencode.Message, error) {
	f.mu.Lock()
	f.calls = append(f.calls, "messages")
	f.messageSession = sessionID
	hook := f.messagesHook
	f.mu.Unlock()
	if hook != nil {
		return hook(ctx, sessionID)
	}
	return apiSuccessMessages("Gist complete."), nil
}

// apiSnapshot is a lock-free copy of the fake's recorded calls.
type apiSnapshot struct {
	calls           []string
	infoCalls       int
	createLocation  string
	createAgent     string
	createProvider  string
	createModel     string
	createVariant   string
	promptSession   string
	promptText      string
	waitSession     string
	messageSession  string
	interruptTarget string
	interruptCtxErr error
	interruptDeadl  time.Time
	interruptHasDL  bool
}

func (f *apiFakeClient) snapshot() apiSnapshot {
	f.mu.Lock()
	defer f.mu.Unlock()
	return apiSnapshot{
		calls:           append([]string(nil), f.calls...),
		infoCalls:       f.infoCalls,
		createLocation:  f.createLocation,
		createAgent:     f.createAgent,
		createProvider:  f.createProvider,
		createModel:     f.createModel,
		createVariant:   f.createVariant,
		promptSession:   f.promptSession,
		promptText:      f.promptText,
		waitSession:     f.waitSession,
		messageSession:  f.messageSession,
		interruptTarget: f.interruptTarget,
		interruptCtxErr: f.interruptCtxErr,
		interruptDeadl:  f.interruptDeadl,
		interruptHasDL:  f.interruptHasDL,
	}
}

// apiRestartRecorder counts restart calls, records the context it received,
// and can fail restarts on demand.
type apiRestartRecorder struct {
	mu          sync.Mutex
	calls       int
	err         error
	ctxErr      error
	deadline    time.Time
	hasDeadline bool
}

func (r *apiRestartRecorder) fn(ctx context.Context) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls++
	r.ctxErr = ctx.Err()
	r.deadline, r.hasDeadline = ctx.Deadline()
	return r.err
}

func (r *apiRestartRecorder) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.calls
}

type apiRestartSnapshot struct {
	calls       int
	ctxErr      error
	deadline    time.Time
	hasDeadline bool
}

func (r *apiRestartRecorder) snapshot() apiRestartSnapshot {
	r.mu.Lock()
	defer r.mu.Unlock()
	return apiRestartSnapshot{
		calls:       r.calls,
		ctxErr:      r.ctxErr,
		deadline:    r.deadline,
		hasDeadline: r.hasDeadline,
	}
}

type apiTestFixture struct {
	runner   *APIRunner
	client   *apiFakeClient
	restarts *apiRestartRecorder
	req      Request
	logPath  string
}

func newAPITestFixture(t *testing.T) *apiTestFixture {
	t.Helper()
	client := &apiFakeClient{}
	restarts := &apiRestartRecorder{}
	runner := NewAPIRunner(client, "opencode-gist.service", restarts.fn)
	// Keep the health window short so a permanently unhealthy server does not
	// stall the test for the production 30 seconds.
	runner.healthWait = 50 * time.Millisecond

	return &apiTestFixture{
		runner:   runner,
		client:   client,
		restarts: restarts,
		logPath:  filepath.Join(t.TempDir(), "job-1.log"),
		req: Request{
			ID:     "job-1",
			Model:  mustModel(t, "deepseek/deepseek-flash#repo-scout"),
			RunDir: t.TempDir(),
			Prompt: "Target repo: ./repo\nUse the repo-gist skill. Focus: none.",
		},
	}
}

func apiSuccessMessages(summary string) []opencode.Message {
	return []opencode.Message{
		{Type: "assistant", Content: []opencode.MessageContent{{Type: "text", Text: summary}}},
		{Type: "idle", Outcome: "succeeded"},
	}
}

func apiInterruptedMessages() []opencode.Message {
	return []opencode.Message{
		{Type: "idle", Outcome: "interrupted"},
	}
}

func awaitSignal(t *testing.T, ch <-chan struct{}, what string) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(5 * time.Second):
		t.Fatalf("timed out waiting for %s", what)
	}
}

func TestAPIRunnerCreatesSessionAtRunDirWithAgentAndModel(t *testing.T) {
	f := newAPITestFixture(t)

	var storedSession string
	f.client.onWait(func(context.Context, string) error {
		f.runner.mu.Lock()
		storedSession = f.runner.sessionID
		f.runner.mu.Unlock()
		return nil
	})

	res, err := f.runner.Run(context.Background(), f.req, f.logPath)
	if err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	if res.ExitCode != 0 {
		t.Errorf("ExitCode = %d, want 0", res.ExitCode)
	}

	snap := f.client.snapshot()
	if snap.createLocation != f.req.RunDir {
		t.Errorf("session location = %q, want %q", snap.createLocation, f.req.RunDir)
	}
	if snap.createAgent != "repo-scout" {
		t.Errorf("session agent = %q, want repo-scout", snap.createAgent)
	}
	if snap.createProvider != f.req.Model.ProviderID {
		t.Errorf("session provider = %q, want %q", snap.createProvider, f.req.Model.ProviderID)
	}
	if snap.createModel != f.req.Model.ModelID {
		t.Errorf("session model = %q, want %q", snap.createModel, f.req.Model.ModelID)
	}
	if snap.createVariant != f.req.Model.Variant {
		t.Errorf("session variant = %q, want %q", snap.createVariant, f.req.Model.Variant)
	}
	if storedSession != apiFakeSessionID {
		t.Errorf("sessionID under mutex = %q, want %q", storedSession, apiFakeSessionID)
	}
	if snap.promptSession != apiFakeSessionID {
		t.Errorf("Prompt session = %q, want %q", snap.promptSession, apiFakeSessionID)
	}
	if snap.promptText != f.req.Prompt {
		t.Errorf("Prompt text = %q, want the request prompt", snap.promptText)
	}
	if snap.waitSession != apiFakeSessionID {
		t.Errorf("Wait session = %q, want %q", snap.waitSession, apiFakeSessionID)
	}
	if snap.messageSession != apiFakeSessionID {
		t.Errorf("Messages session = %q, want %q", snap.messageSession, apiFakeSessionID)
	}
}

func TestAPIRunnerSuccessUsesLastAssistantText(t *testing.T) {
	f := newAPITestFixture(t)
	f.client.onMessages(func(context.Context, string) ([]opencode.Message, error) {
		// Messages are newest first, so the first assistant message is the
		// last one in time and supplies the summary.
		return []opencode.Message{
			{Type: "assistant", Content: []opencode.MessageContent{
				{Type: "text", Text: "final gist summary"},
				{Type: "reasoning", Text: "hidden reasoning"},
			}},
			{Type: "assistant", Content: []opencode.MessageContent{
				{Type: "text", Text: "older assistant message"},
			}},
			{Type: "idle", Outcome: "succeeded"},
		}, nil
	})

	res, err := f.runner.Run(context.Background(), f.req, f.logPath)
	if err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	if res.Summary != "final gist summary" {
		t.Errorf("Summary = %q, want %q", res.Summary, "final gist summary")
	}
	if res.ExitCode != 0 {
		t.Errorf("ExitCode = %d, want 0", res.ExitCode)
	}

	log := readFileString(t, f.logPath)
	for _, want := range []string{apiFakeSessionID, "health", "prompt", "wait", "outcome", "succeeded"} {
		if !strings.Contains(log, want) {
			t.Errorf("log does not contain %q", want)
		}
	}
}

func TestAPIRunnerHealthFailureRestartsOnceThenSucceeds(t *testing.T) {
	f := newAPITestFixture(t)
	f.client.onInfo(func(_ context.Context, call int) error {
		if call == 1 {
			return errors.New("connection refused")
		}
		return nil
	})

	res, err := f.runner.Run(context.Background(), f.req, f.logPath)
	if err != nil {
		t.Fatalf("Run returned error after recovery: %v", err)
	}
	if res.ExitCode != 0 {
		t.Errorf("ExitCode = %d, want 0", res.ExitCode)
	}
	if got := f.restarts.count(); got != 1 {
		t.Errorf("restart calls = %d, want 1", got)
	}
	if got := f.client.snapshot().infoCalls; got != 2 {
		t.Errorf("Info calls = %d, want 2", got)
	}
	if !strings.Contains(readFileString(t, f.logPath), "restart") {
		t.Errorf("log does not record the restart")
	}
}

func TestAPIRunnerHealthStillBadReturnsError(t *testing.T) {
	f := newAPITestFixture(t)
	f.client.onInfo(func(context.Context, int) error {
		return errors.New("server down")
	})

	start := time.Now()
	_, err := f.runner.Run(context.Background(), f.req, f.logPath)
	if err == nil {
		t.Fatal("Run returned a nil error, want a health failure")
	}
	if !strings.Contains(err.Error(), "unhealthy") {
		t.Errorf("Run error = %v, want it to mention the unhealthy API", err)
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Errorf("Run took %s, want the shortened health window", elapsed)
	}
	if got := f.restarts.count(); got != 1 {
		t.Errorf("restart calls = %d, want 1", got)
	}
	if got := f.client.countCall("create"); got != 0 {
		t.Errorf("CreateSession calls = %d, want 0 while unhealthy", got)
	}
	if !strings.Contains(readFileString(t, f.logPath), "health") {
		t.Errorf("log does not record the health failure")
	}
}

func TestAPIRunnerWaitFailureReturnsError(t *testing.T) {
	f := newAPITestFixture(t)
	f.client.onWait(func(context.Context, string) error {
		return errors.New("wait failed")
	})

	_, err := f.runner.Run(context.Background(), f.req, f.logPath)
	if err == nil {
		t.Fatal("Run returned a nil error, want the wait failure")
	}
	if !strings.Contains(err.Error(), "wait failed") {
		t.Errorf("Run error = %v, want it to wrap the wait failure", err)
	}
	if errors.Is(err, ErrCancelled) || errors.Is(err, ErrTimeout) {
		t.Errorf("wait failure was reported as cancellation or timeout")
	}
	if got := f.client.countCall("interrupt"); got != 1 {
		t.Errorf("Interrupt calls = %d, want 1 cleanup interrupt", got)
	}
	if got := f.restarts.count(); got != 0 {
		t.Errorf("restart calls = %d, want 0 for a cleanup interrupt", got)
	}
	if snap := f.client.snapshot(); snap.interruptCtxErr != nil {
		t.Errorf("cleanup interrupt context error = %v, want a live background context", snap.interruptCtxErr)
	}
	if got := f.client.countCall("messages"); got != 0 {
		t.Errorf("Messages calls = %d, want 0 after a wait failure", got)
	}
	if !strings.Contains(readFileString(t, f.logPath), "wait") {
		t.Errorf("log does not record the wait failure")
	}
}

func TestAPIRunnerPromptFailureCleansUpSession(t *testing.T) {
	f := newAPITestFixture(t)
	f.client.onPrompt(func(context.Context, string, string) error {
		return errors.New("prompt rejected")
	})

	_, err := f.runner.Run(context.Background(), f.req, f.logPath)
	if err == nil || !strings.Contains(err.Error(), "prompt rejected") {
		t.Fatalf("Run error = %v, want the prompt failure", err)
	}
	if got := f.client.countCall("interrupt"); got != 1 {
		t.Errorf("Interrupt calls = %d, want 1 cleanup interrupt", got)
	}
	if got := f.restarts.count(); got != 0 {
		t.Errorf("restart calls = %d, want 0 for a cleanup interrupt", got)
	}
	if got := f.client.countCall("wait"); got != 0 {
		t.Errorf("Wait calls = %d, want 0 after a prompt failure", got)
	}
	if snap := f.client.snapshot(); snap.interruptCtxErr != nil {
		t.Errorf("cleanup interrupt context error = %v, want a live background context", snap.interruptCtxErr)
	}
	if log := readFileString(t, f.logPath); !strings.Contains(log, "cleaned up") {
		t.Errorf("log does not record the session cleanup")
	}
}

func TestAPIRunnerMessagesFailureCleansUpSession(t *testing.T) {
	f := newAPITestFixture(t)
	f.client.onMessages(func(context.Context, string) ([]opencode.Message, error) {
		return nil, errors.New("messages unavailable")
	})

	_, err := f.runner.Run(context.Background(), f.req, f.logPath)
	if err == nil || !strings.Contains(err.Error(), "messages unavailable") {
		t.Fatalf("Run error = %v, want the messages failure", err)
	}
	if got := f.client.countCall("interrupt"); got != 1 {
		t.Errorf("Interrupt calls = %d, want 1 cleanup interrupt", got)
	}
	if got := f.restarts.count(); got != 0 {
		t.Errorf("restart calls = %d, want 0 for a cleanup interrupt", got)
	}
	if snap := f.client.snapshot(); snap.interruptCtxErr != nil {
		t.Errorf("cleanup interrupt context error = %v, want a live background context", snap.interruptCtxErr)
	}
	if log := readFileString(t, f.logPath); !strings.Contains(log, "cleaned up") {
		t.Errorf("log does not record the session cleanup")
	}
}

func TestAPIRunnerCancelInterruptsWithoutRestart(t *testing.T) {
	f := newAPITestFixture(t)
	waitStarted := make(chan struct{})
	release := make(chan struct{})
	var releaseOnce sync.Once
	releaseWait := func() { releaseOnce.Do(func() { close(release) }) }
	t.Cleanup(releaseWait)

	f.client.onWait(func(ctx context.Context, _ string) error {
		close(waitStarted)
		select {
		case <-release:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	})
	f.client.onMessages(func(context.Context, string) ([]opencode.Message, error) {
		return apiInterruptedMessages(), nil
	})

	runCtx, cancelRun := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancelRun()

	type outcome struct {
		res RunResult
		err error
	}
	done := make(chan outcome, 1)
	go func() {
		res, err := f.runner.Run(runCtx, f.req, f.logPath)
		done <- outcome{res: res, err: err}
	}()

	awaitSignal(t, waitStarted, "Wait")

	if err := f.runner.Cancel(context.Background()); err != nil {
		t.Fatalf("Cancel returned error: %v", err)
	}

	snap := f.client.snapshot()
	if got := f.client.countCall("interrupt"); got != 1 {
		t.Errorf("Interrupt calls = %d, want 1", got)
	}
	if snap.interruptTarget != apiFakeSessionID {
		t.Errorf("Interrupt session = %q, want %q", snap.interruptTarget, apiFakeSessionID)
	}
	if got := f.restarts.count(); got != 0 {
		t.Errorf("restart calls = %d, want 0", got)
	}

	releaseWait()
	select {
	case out := <-done:
		if !errors.Is(out.err, ErrCancelled) {
			t.Fatalf("Run error = %v, want ErrCancelled", out.err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return after Cancel")
	}
}

func TestAPIRunnerCancelRestartsOnInterruptError(t *testing.T) {
	f := newAPITestFixture(t)
	f.client.onInterrupt(func(context.Context, string) (bool, error) {
		return false, errors.New("api server unavailable")
	})
	f.runner.mu.Lock()
	f.runner.sessionID = apiFakeSessionID
	f.runner.mu.Unlock()

	if err := f.runner.Cancel(context.Background()); err != nil {
		t.Fatalf("Cancel returned error: %v", err)
	}
	if got := f.client.countCall("interrupt"); got != 1 {
		t.Errorf("Interrupt calls = %d, want 1", got)
	}
	if got := f.restarts.count(); got != 1 {
		t.Errorf("restart calls = %d, want 1 after an interrupt error", got)
	}
}

func TestAPIRunnerRestartAfterInterruptTimeoutUsesLiveContext(t *testing.T) {
	f := newAPITestFixture(t)
	f.runner.interruptWait = 20 * time.Millisecond

	f.client.onWait(func(ctx context.Context, _ string) error {
		<-ctx.Done()
		return ctx.Err()
	})
	// An unresponsive interrupt consumes its whole budget before failing, which
	// is exactly when the restart must not reuse that expired context.
	f.client.onInterrupt(func(ctx context.Context, _ string) (bool, error) {
		<-ctx.Done()
		return false, ctx.Err()
	})

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()

	if _, err := f.runner.Run(ctx, f.req, f.logPath); !errors.Is(err, ErrTimeout) {
		t.Fatalf("Run error = %v, want ErrTimeout", err)
	}

	snap := f.restarts.snapshot()
	if snap.calls != 1 {
		t.Fatalf("restart calls = %d, want 1", snap.calls)
	}
	if snap.ctxErr != nil {
		t.Errorf("restart context error = %v, want a fresh live context", snap.ctxErr)
	}
	if !snap.hasDeadline {
		t.Error("restart context has no deadline, want a bounded background context")
	}
}

func TestAPIRunnerCancelRestartUsesLiveContext(t *testing.T) {
	f := newAPITestFixture(t)
	f.client.onInterrupt(func(context.Context, string) (bool, error) {
		return false, errors.New("api server unavailable")
	})
	f.runner.mu.Lock()
	f.runner.sessionID = apiFakeSessionID
	f.runner.mu.Unlock()

	// Task 11 may pass the already-cancelled job context to Cancel. The
	// recovery restart must not inherit it.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if err := f.runner.Cancel(ctx); err != nil {
		t.Fatalf("Cancel returned error: %v", err)
	}
	snap := f.restarts.snapshot()
	if snap.calls != 1 {
		t.Fatalf("restart calls = %d, want 1", snap.calls)
	}
	if snap.ctxErr != nil {
		t.Errorf("restart context error = %v, want a fresh live context", snap.ctxErr)
	}
	if !snap.hasDeadline {
		t.Error("restart context has no deadline, want a bounded background context")
	}
}

func TestAPIRunnerCancelFalseDoesNotRestart(t *testing.T) {
	f := newAPITestFixture(t)
	waitStarted := make(chan struct{})
	release := make(chan struct{})
	var releaseOnce sync.Once
	releaseWait := func() { releaseOnce.Do(func() { close(release) }) }
	t.Cleanup(releaseWait)

	f.client.onWait(func(ctx context.Context, _ string) error {
		close(waitStarted)
		select {
		case <-release:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	})
	f.client.onInterrupt(func(context.Context, string) (bool, error) {
		return false, nil
	})

	runCtx, cancelRun := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancelRun()

	done := make(chan error, 1)
	go func() {
		_, err := f.runner.Run(runCtx, f.req, f.logPath)
		done <- err
	}()

	awaitSignal(t, waitStarted, "Wait")

	if err := f.runner.Cancel(context.Background()); err != nil {
		t.Fatalf("Cancel returned error: %v", err)
	}
	if got := f.client.countCall("interrupt"); got != 1 {
		t.Errorf("Interrupt calls = %d, want 1", got)
	}
	if got := f.restarts.count(); got != 0 {
		t.Errorf("restart calls = %d, want 0 when the session was already idle", got)
	}
	if log := readFileString(t, f.logPath); !strings.Contains(log, "already idle") {
		t.Errorf("log does not record that the session was already idle")
	}

	releaseWait()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run returned error after an already-idle cancel: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return after the already-idle cancel")
	}
}

func TestAPIRunnerNeverCallsCLI(t *testing.T) {
	typ := reflect.TypeOf(APIRunner{})
	cliType := reflect.TypeOf((*CLIRunner)(nil))
	for i := 0; i < typ.NumField(); i++ {
		field := typ.Field(i)
		if field.Type == cliType {
			t.Errorf("APIRunner field %q has the CLI runner type", field.Name)
		}
		switch field.Name {
		case "bin", "cli", "cliRunner":
			t.Errorf("APIRunner field %q looks like a CLI dependency", field.Name)
		}
	}

	ctor := reflect.TypeOf(NewAPIRunner)
	if ctor.Kind() != reflect.Func {
		t.Fatalf("NewAPIRunner is %s, want a function", ctor.Kind())
	}
	if ctor.NumIn() != 3 {
		t.Fatalf("NewAPIRunner takes %d arguments, want 3", ctor.NumIn())
	}
	clientType := reflect.TypeOf((*APIClient)(nil)).Elem()
	if ctor.In(0) != clientType {
		t.Errorf("NewAPIRunner first argument is %s, want APIClient", ctor.In(0))
	}
}

func TestAPIRunnerTimeoutInterrupts(t *testing.T) {
	f := newAPITestFixture(t)
	f.client.onWait(func(ctx context.Context, _ string) error {
		<-ctx.Done()
		return ctx.Err()
	})

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()

	res, err := f.runner.Run(ctx, f.req, f.logPath)
	if !errors.Is(err, ErrTimeout) {
		t.Fatalf("Run error = %v, want ErrTimeout", err)
	}
	if res.ExitCode != -1 {
		t.Errorf("ExitCode = %d, want -1", res.ExitCode)
	}
	if got := f.client.countCall("interrupt"); got != 1 {
		t.Errorf("Interrupt calls = %d, want 1", got)
	}
	if got := f.restarts.count(); got != 0 {
		t.Errorf("restart calls = %d, want 0", got)
	}

	snap := f.client.snapshot()
	if snap.interruptCtxErr != nil {
		t.Errorf("Interrupt context error = %v, want a background context", snap.interruptCtxErr)
	}
	if !snap.interruptHasDL {
		t.Error("Interrupt context has no deadline, want the 5s background budget")
	} else if remaining := time.Until(snap.interruptDeadl); remaining <= 0 || remaining > 5*time.Second {
		t.Errorf("Interrupt deadline is %s away, want at most 5s", remaining)
	}
	if log := readFileString(t, f.logPath); !strings.Contains(log, "interrupt") {
		t.Errorf("log does not record the interrupt")
	}
}

func TestAPIRunnerFallbackSummary(t *testing.T) {
	f := newAPITestFixture(t)
	f.client.onMessages(func(context.Context, string) ([]opencode.Message, error) {
		return []opencode.Message{
			{Type: "assistant", Content: []opencode.MessageContent{{Type: "reasoning", Text: "no final text"}}},
			{Type: "idle", Outcome: "succeeded"},
		}, nil
	})

	res, err := f.runner.Run(context.Background(), f.req, f.logPath)
	if err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	if res.Summary != "Gist complete." {
		t.Errorf("Summary = %q, want the fallback", res.Summary)
	}
}

func TestAPIRunnerFailedOutcomeReturnsError(t *testing.T) {
	f := newAPITestFixture(t)
	f.client.onMessages(func(context.Context, string) ([]opencode.Message, error) {
		return []opencode.Message{
			{Type: "assistant", Content: []opencode.MessageContent{{Type: "text", Text: "partial gist"}}},
			{Type: "idle", Outcome: "failed"},
		}, nil
	})

	res, err := f.runner.Run(context.Background(), f.req, f.logPath)
	if err == nil {
		t.Fatal("Run returned a nil error for a failed outcome")
	}
	if errors.Is(err, ErrCancelled) || errors.Is(err, ErrTimeout) {
		t.Errorf("failed outcome was reported as cancellation or timeout: %v", err)
	}
	if res.Summary != "partial gist" {
		t.Errorf("Summary = %q, want the last assistant text", res.Summary)
	}
	if log := readFileString(t, f.logPath); !strings.Contains(log, "failed") {
		t.Errorf("log does not record the failed outcome")
	}
}

func TestAPIRunnerInterruptedOutcomeReturnsCancelled(t *testing.T) {
	f := newAPITestFixture(t)
	f.client.onMessages(func(context.Context, string) ([]opencode.Message, error) {
		return apiInterruptedMessages(), nil
	})

	_, err := f.runner.Run(context.Background(), f.req, f.logPath)
	if !errors.Is(err, ErrCancelled) {
		t.Fatalf("Run error = %v, want ErrCancelled", err)
	}
}

func TestAPIRunnerMissingOrUnknownOutcomeReturnsError(t *testing.T) {
	cases := []struct {
		name     string
		messages []opencode.Message
	}{
		{
			name: "missing idle",
			messages: []opencode.Message{
				{Type: "assistant", Content: []opencode.MessageContent{{Type: "text", Text: "summary"}}},
			},
		},
		{
			name: "empty outcome",
			messages: []opencode.Message{
				{Type: "assistant", Content: []opencode.MessageContent{{Type: "text", Text: "summary"}}},
				{Type: "idle"},
			},
		},
		{
			name: "unknown outcome",
			messages: []opencode.Message{
				{Type: "assistant", Content: []opencode.MessageContent{{Type: "text", Text: "summary"}}},
				{Type: "idle", Outcome: "cancelled"},
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newAPITestFixture(t)
			f.client.onMessages(func(context.Context, string) ([]opencode.Message, error) {
				return tc.messages, nil
			})

			res, err := f.runner.Run(context.Background(), f.req, f.logPath)
			if err == nil {
				t.Fatal("Run returned a nil error for a non-succeeded outcome")
			}
			if errors.Is(err, ErrCancelled) || errors.Is(err, ErrTimeout) {
				t.Errorf("outcome was reported as cancellation or timeout: %v", err)
			}
			if res.ExitCode != -1 {
				t.Errorf("ExitCode = %d, want -1", res.ExitCode)
			}
			if res.Summary != "summary" {
				t.Errorf("Summary = %q, want the last assistant text", res.Summary)
			}
		})
	}
}

func TestAPIRunnerRejectsAlreadyCancelledContext(t *testing.T) {
	f := newAPITestFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if _, err := f.runner.Run(ctx, f.req, f.logPath); !errors.Is(err, ErrCancelled) {
		t.Fatalf("Run error = %v, want ErrCancelled", err)
	}
	if got := f.client.countCall("info"); got != 0 {
		t.Errorf("Info calls = %d, want 0 for an already-cancelled context", got)
	}
}

func TestAPIRunnerWritesPrivateLogWithTimestamps(t *testing.T) {
	f := newAPITestFixture(t)
	if _, err := f.runner.Run(context.Background(), f.req, f.logPath); err != nil {
		t.Fatalf("Run returned error: %v", err)
	}

	info, err := os.Stat(f.logPath)
	if err != nil {
		t.Fatalf("stat log: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("log mode = %o, want 600", perm)
	}

	raw := readFileString(t, f.logPath)
	stamp := regexp.MustCompile(`^\d{4}/\d{2}/\d{2} \d{2}:\d{2}:\d{2} `)
	for _, line := range strings.Split(strings.TrimSuffix(raw, "\n"), "\n") {
		if line == "" {
			continue
		}
		if !stamp.MatchString(line) {
			t.Errorf("log line %q does not start with a timestamp", line)
		}
	}
	for _, want := range []string{"health", apiFakeSessionID, "prompt", "wait", "outcome"} {
		if !strings.Contains(raw, want) {
			t.Errorf("log does not contain %q", want)
		}
	}
}
