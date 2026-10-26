package jobs

import (
	"context"
	"errors"
	"fmt"
	"log"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"gistbot/internal/opencode"
)

const (
	// apiHealthInterval is how often Info is retried after a restart.
	apiHealthInterval = 500 * time.Millisecond
	// apiHealthTimeout is how long a restarted API server has to become
	// healthy before the job fails.
	apiHealthTimeout = 30 * time.Second
	// apiInterruptGrace bounds the background interrupt issued after a job is
	// cancelled, times out, or fails after its session was created.
	apiInterruptGrace = 5 * time.Second
	// apiRestartGrace bounds a restart triggered by an interrupt failure. It is
	// deliberately separate from the interrupt budget: an interrupt that hangs
	// until its deadline must not leave the restart with an expired context.
	apiRestartGrace = 5 * time.Second
	// apiAgent is the fixed OpenCode agent used for gist jobs.
	apiAgent = "repo-scout"
	// apiFallbackSummary is sent when the session produced no assistant text.
	apiFallbackSummary = "Gist complete."
)

// APIClient is the narrow OpenCode API surface the API runner needs. The
// production implementation is *opencode.Client.
type APIClient interface {
	Info(ctx context.Context) error
	CreateSession(ctx context.Context, location, agent, providerID, modelID, variant string) (string, error)
	Prompt(ctx context.Context, sessionID, text string) error
	Wait(ctx context.Context, sessionID string) error
	Interrupt(ctx context.Context, sessionID string) (bool, error)
	Messages(ctx context.Context, sessionID string) ([]opencode.Message, error)
}

// APIRunner executes a job through the OpenCode HTTP API. It never falls back
// to the CLI. A runner handles one job at a time; Cancel may be called
// concurrently with Run.
type APIRunner struct {
	client  APIClient
	unit    string
	restart func(context.Context) error

	// healthWait is how long a restarted API server has to become healthy.
	// It is a field so tests can shorten the production 30s window; the poll
	// interval stays at apiHealthInterval.
	healthWait time.Duration

	// interruptWait and restartWait bound background interrupts and the
	// recovery restart. They are fields so tests can shorten the budgets.
	interruptWait time.Duration
	restartWait   time.Duration

	mu              sync.Mutex
	sessionID       string
	logger          *log.Logger
	cancelRequested bool
}

// NewAPIRunner returns a runner that talks to client. unit is the systemd unit
// restarted when the API is unhealthy; restart may be nil, in which case a
// health or interrupt failure is reported instead of recovered.
func NewAPIRunner(client APIClient, unit string, restart func(context.Context) error) *APIRunner {
	return &APIRunner{
		client:        client,
		unit:          unit,
		restart:       restart,
		healthWait:    apiHealthTimeout,
		interruptWait: apiInterruptGrace,
		restartWait:   apiRestartGrace,
	}
}

// Run executes one job: it checks API health, creates a session in req.RunDir,
// prompts it, waits for idle, and turns the final session messages into a
// summary and an outcome. Cancellation and deadlines interrupt the session and
// return ErrCancelled or ErrTimeout.
func (r *APIRunner) Run(ctx context.Context, req Request, logPath string) (RunResult, error) {
	if ctx == nil {
		return RunResult{ExitCode: -1}, errors.New("jobs: API runner context is nil")
	}
	if ctx.Err() != nil {
		return RunResult{ExitCode: -1}, contextFailure(ctx)
	}
	if err := validateAPIRequest(r.client, req, logPath); err != nil {
		return RunResult{ExitCode: -1}, err
	}

	logFile, err := openJobLog(logPath)
	if err != nil {
		return RunResult{ExitCode: -1}, err
	}
	defer logFile.Close()
	logger := log.New(logFile, "", log.LstdFlags)

	r.mu.Lock()
	r.logger = logger
	r.sessionID = ""
	r.cancelRequested = false
	r.mu.Unlock()
	defer func() {
		r.mu.Lock()
		r.logger = nil
		r.sessionID = ""
		r.cancelRequested = false
		r.mu.Unlock()
	}()

	apiLogf(logger, "api runner: starting job %s", req.ID)

	if err := r.ensureHealthy(ctx, logger); err != nil {
		if ctx.Err() != nil {
			apiLogf(logger, "error: health check stopped: %v", err)
			return RunResult{ExitCode: -1}, contextFailure(ctx)
		}
		apiLogf(logger, "error: %v", err)
		return RunResult{ExitCode: -1}, err
	}

	sessionID, err := r.client.CreateSession(ctx, req.RunDir, apiAgent,
		req.Model.ProviderID, req.Model.ModelID, req.Model.Variant)
	if err != nil {
		apiLogf(logger, "error: create session: %v", err)
		if ctx.Err() != nil {
			return RunResult{ExitCode: -1}, contextFailure(ctx)
		}
		return RunResult{ExitCode: -1}, fmt.Errorf("jobs: create API session: %w", err)
	}
	r.mu.Lock()
	r.sessionID = sessionID
	r.mu.Unlock()
	apiLogf(logger, "session: created %s in %s", sessionID, req.RunDir)

	if err := r.client.Prompt(ctx, sessionID, req.Prompt); err != nil {
		apiLogf(logger, "error: prompt: %v", err)
		if ctx.Err() != nil {
			r.interruptAfterCancellation(sessionID, logger)
			return RunResult{ExitCode: -1}, contextFailure(ctx)
		}
		r.cleanupSession(sessionID, logger)
		return RunResult{ExitCode: -1}, fmt.Errorf("jobs: prompt API session: %w", err)
	}
	apiLogf(logger, "prompt: accepted for session %s", sessionID)

	waitErr := r.client.Wait(ctx, sessionID)
	if ctxErr := ctx.Err(); ctxErr != nil {
		apiLogf(logger, "wait: interrupted by %v", ctxErr)
		r.interruptAfterCancellation(sessionID, logger)
		return RunResult{ExitCode: -1}, contextFailure(ctx)
	}
	if waitErr != nil {
		apiLogf(logger, "wait: failed: %v", waitErr)
		r.cleanupSession(sessionID, logger)
		if r.cancelWasRequested() {
			return RunResult{ExitCode: -1}, ErrCancelled
		}
		return RunResult{ExitCode: -1}, fmt.Errorf("jobs: wait for API session: %w", waitErr)
	}
	apiLogf(logger, "wait: completed for session %s", sessionID)

	messages, err := r.client.Messages(ctx, sessionID)
	if err != nil {
		apiLogf(logger, "error: messages: %v", err)
		if ctx.Err() != nil {
			r.interruptAfterCancellation(sessionID, logger)
			return RunResult{ExitCode: -1}, contextFailure(ctx)
		}
		r.cleanupSession(sessionID, logger)
		return RunResult{ExitCode: -1}, fmt.Errorf("jobs: read API session messages: %w", err)
	}

	summary := lastAssistantText(messages)
	if summary == "" {
		summary = apiFallbackSummary
	}
	outcome := lastIdleOutcome(messages)
	apiLogf(logger, "outcome: session %s finished with %q", sessionID, outcome)

	switch outcome {
	case "succeeded":
		return RunResult{ExitCode: 0, Summary: summary}, nil
	case "failed":
		return RunResult{ExitCode: -1, Summary: summary},
			fmt.Errorf("jobs: API session %s ended with outcome failed", sessionID)
	case "interrupted":
		return RunResult{ExitCode: -1, Summary: summary}, ErrCancelled
	case "":
		return RunResult{ExitCode: -1, Summary: summary},
			fmt.Errorf("jobs: API session %s has no idle outcome", sessionID)
	default:
		return RunResult{ExitCode: -1, Summary: summary},
			fmt.Errorf("jobs: API session %s ended with unknown outcome %q", sessionID, outcome)
	}
}

// Cancel interrupts the active API session. A failed interrupt triggers one
// restart; an interrupt that reports no active execution means the session was
// already idle, so no restart is needed.
func (r *APIRunner) Cancel(ctx context.Context) error {
	if ctx == nil {
		ctx = context.Background()
	}

	r.mu.Lock()
	r.cancelRequested = true
	sessionID := r.sessionID
	logger := r.logger
	r.mu.Unlock()

	if sessionID == "" {
		apiLogf(logger, "cancel: no active API session")
		return nil
	}
	apiLogf(logger, "cancel: interrupting session %s", sessionID)
	return r.interrupt(ctx, sessionID, logger)
}

// ensureHealthy calls Info once and, on failure, restarts the API unit once and
// polls until the server answers or the health window expires.
func (r *APIRunner) ensureHealthy(ctx context.Context, logger *log.Logger) error {
	err := r.client.Info(ctx)
	if err == nil {
		apiLogf(logger, "health: ok")
		return nil
	}
	apiLogf(logger, "health: failed: %v", err)
	if ctx.Err() != nil {
		return ctx.Err()
	}

	if r.restart == nil {
		return errors.New("jobs: OpenCode API is unavailable and no restart is configured")
	}
	apiLogf(logger, "health: restarting %s", r.unit)
	if err := r.restart(ctx); err != nil {
		return fmt.Errorf("jobs: restart OpenCode API %s: %w", r.unit, err)
	}

	deadline := time.Now().Add(r.healthWait)
	for {
		err := r.client.Info(ctx)
		if err == nil {
			apiLogf(logger, "health: ok after restart")
			return nil
		}
		apiLogf(logger, "health: still unhealthy: %v", err)
		if ctx.Err() != nil {
			return ctx.Err()
		}

		remaining := time.Until(deadline)
		if remaining <= 0 {
			return fmt.Errorf("jobs: OpenCode API %s is still unhealthy after restart", r.unit)
		}
		wait := apiHealthInterval
		if wait > remaining {
			wait = remaining
		}
		timer := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
}

// interrupt stops the session and recovers the API if the interrupt call
// itself failed. The recovery restart uses its own fresh background context so
// an interrupt that consumed its whole deadline cannot poison the restart.
func (r *APIRunner) interrupt(ctx context.Context, sessionID string, logger *log.Logger) error {
	interrupted, err := r.client.Interrupt(ctx, sessionID)
	if err != nil {
		apiLogf(logger, "interrupt: failed for session %s: %v", sessionID, err)
		if r.restart == nil {
			return fmt.Errorf("jobs: interrupt API session: %w", err)
		}
		apiLogf(logger, "restart: requested after interrupt failure")
		return r.restartAfterInterrupt(logger)
	}
	if !interrupted {
		apiLogf(logger, "interrupt: session %s was already idle; no restart", sessionID)
		return nil
	}
	apiLogf(logger, "interrupt: session %s interrupted", sessionID)
	return nil
}

// restartAfterInterrupt restarts the API with a fresh bounded background
// context. The context of the failed interrupt or cancel is never reused.
func (r *APIRunner) restartAfterInterrupt(logger *log.Logger) error {
	ctx, cancel := context.WithTimeout(context.Background(), r.restartWait)
	defer cancel()
	if err := r.restart(ctx); err != nil {
		apiLogf(logger, "restart: failed: %v", err)
		return fmt.Errorf("jobs: restart OpenCode API %s: %w", r.unit, err)
	}
	apiLogf(logger, "restart: complete")
	return nil
}

// cleanupSession interrupts a session whose job failed after creation. The
// interrupt uses a fresh background budget and never restarts the API; an
// interrupt error is logged so the job can still report its original failure.
func (r *APIRunner) cleanupSession(sessionID string, logger *log.Logger) {
	ctx, cancel := context.WithTimeout(context.Background(), r.interruptWait)
	defer cancel()
	interrupted, err := r.client.Interrupt(ctx, sessionID)
	if err != nil {
		apiLogf(logger, "interrupt: cleanup failed for session %s: %v", sessionID, err)
		return
	}
	if !interrupted {
		apiLogf(logger, "interrupt: cleanup found session %s already idle", sessionID)
		return
	}
	apiLogf(logger, "interrupt: cleaned up session %s", sessionID)
}

// interruptAfterCancellation interrupts with a background budget so a
// cancelled job context cannot prevent cleanup. The cancellation error is what
// Run reports, so a recovery error is only logged.
func (r *APIRunner) interruptAfterCancellation(sessionID string, logger *log.Logger) {
	ctx, cancel := context.WithTimeout(context.Background(), r.interruptWait)
	defer cancel()
	if err := r.interrupt(ctx, sessionID, logger); err != nil {
		apiLogf(logger, "error: interrupt cleanup: %v", err)
	}
}

func (r *APIRunner) cancelWasRequested() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.cancelRequested
}

// lastAssistantText returns the newest assistant text. Messages arrive newest
// first, so the first assistant message with a text part is also the last one
// in time. Within that message the last non-empty text part wins.
func lastAssistantText(messages []opencode.Message) string {
	for _, message := range messages {
		if message.Type != "assistant" {
			continue
		}
		for i := len(message.Content) - 1; i >= 0; i-- {
			part := message.Content[i]
			if part.Type == "text" && strings.TrimSpace(part.Text) != "" {
				return strings.TrimSpace(part.Text)
			}
		}
	}
	return ""
}

// lastIdleOutcome returns the newest idle outcome. Messages arrive newest first,
// so the first idle message is the last one in time.
func lastIdleOutcome(messages []opencode.Message) string {
	for _, message := range messages {
		if message.Type == "idle" {
			return message.Outcome
		}
	}
	return ""
}

func validateAPIRequest(client APIClient, req Request, logPath string) error {
	if client == nil {
		return errors.New("jobs: API client is required")
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

// apiLogf writes to the job log unless the runner has no log yet.
func apiLogf(logger *log.Logger, format string, args ...any) {
	if logger != nil {
		logger.Printf(format, args...)
	}
}
