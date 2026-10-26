// Package jobs defines the requests, results, and runner contracts shared by
// Zero Two's CLI and API runners and its single-worker job manager.
package jobs

import (
	"context"
	"errors"
	"time"

	"gistbot/internal/config"
	"gistbot/internal/repo"
	"gistbot/internal/state"
)

var (
	// ErrCancelled reports a job that stopped because Cancel was requested.
	ErrCancelled = errors.New("job cancelled")
	// ErrTimeout reports a job that stopped because its deadline expired.
	ErrTimeout = errors.New("job timed out")
)

// RunnerKind identifies how a job talks to OpenCode.
type RunnerKind string

const (
	RunnerCLI RunnerKind = "cli"
	RunnerAPI RunnerKind = "api"
)

// Request is one queued gist job. The fields above RunDir are supplied by the
// bot; RunDir, TargetDir, and Prompt are filled in by the manager immediately
// before the runner starts.
type Request struct {
	ID         string
	Target     repo.Target
	Focus      string
	ChatID     int64
	Runner     RunnerKind
	Model      config.ModelRef
	ModelAlias string
	EnqueuedAt time.Time

	RunDir    string // set by Manager
	TargetDir string // set by Manager
	Prompt    string // set by Manager
}

// RunResult is what a runner observed. Summary is the cleaned tail of the
// child's output; ExitCode is -1 when the child never exited normally.
type RunResult struct {
	Summary  string
	ExitCode int
}

// Runner executes one job and can cancel the job it is currently running.
type Runner interface {
	Run(ctx context.Context, req Request, logPath string) (RunResult, error)
	Cancel(ctx context.Context) error
}

// Result is the manager's final record of a finished job.
type Result struct {
	Request    Request
	Status     state.JobStatus
	Docs       repo.Docs
	Summary    string
	Err        error
	StartedAt  time.Time
	FinishedAt time.Time
	LogPath    string
}

// Notifier delivers a finished job to the user.
type Notifier interface {
	Notify(ctx context.Context, res Result)
}

// StartNotifier is an optional extension of Notifier. When the manager's
// Notifier also implements it, NotifyStarted runs once per job after the job is
// recorded as running and before repository preparation begins. Implementations
// must be best-effort: a failure to deliver the start event never affects the
// job.
type StartNotifier interface {
	NotifyStarted(ctx context.Context, req Request)
}

// JobView is a read-only snapshot of a job for /status.
type JobView struct {
	ID        string
	RepoID    string
	Runner    string
	Model     string
	StartedAt time.Time
	Elapsed   time.Duration
}

// Status is the manager's current running and queued work.
type Status struct {
	Running *JobView
	Queued  []JobView
}
