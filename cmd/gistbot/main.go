// Command gistbot runs Zero Two, the owner-only Telegram front end for the
// repo-gist workflow. It loads validated configuration, wires the Telegram,
// repository, job, and OpenCode components, and shuts down gracefully on
// SIGINT or SIGTERM.
package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"os/signal"
	"syscall"
	"time"

	"gistbot/internal/bot"
	"gistbot/internal/config"
	"gistbot/internal/jobs"
	"gistbot/internal/opencode"
	"gistbot/internal/repo"
	"gistbot/internal/state"
	"gistbot/internal/telegram"
)

// shutdownTimeout bounds how long manager.Stop may wait for a cancelled job
// and its notification after the poller exits.
const shutdownTimeout = 15 * time.Second

func main() {
	logger := slog.New(slog.NewTextHandler(os.Stderr, nil))

	cfg, err := config.Load(os.Getenv)
	if err != nil {
		logger.Error("configuration is invalid", "error", err)
		os.Exit(1)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if err := runAndLog(ctx, cfg, logger); err != nil {
		os.Exit(1)
	}
}

// runAndLog runs the bot and records any failure with the supplied logger.
// main and the failure-path tests share this path so secret redaction on
// startup errors is exercised exactly as it is in production.
func runAndLog(ctx context.Context, cfg config.Config, logger *slog.Logger) error {
	if logger == nil {
		logger = slog.Default()
	}
	if err := run(ctx, cfg, logger); err != nil {
		logger.Error("gistbot stopped with an error", "error", err)
		return err
	}
	return nil
}

// run wires and runs the bot until ctx is cancelled. It receives an
// already-validated configuration and never reads the environment itself.
func run(ctx context.Context, cfg config.Config, logger *slog.Logger) error {
	if logger == nil {
		logger = slog.Default()
	}
	if ctx == nil {
		ctx = context.Background()
	}

	store, err := state.Open(cfg.StateFile, state.Data{
		Runner: cfg.DefaultRunner,
		Model:  cfg.DefaultModel,
	})
	if err != nil {
		return fmt.Errorf("open state: %w", err)
	}
	if err := store.MarkInterrupted(time.Now()); err != nil {
		return fmt.Errorf("mark interrupted jobs: %w", err)
	}

	cloner := repo.NewCloner(cfg.WorkDir, "git", cfg.CloneTimeout)
	if err := cloner.Recover(); err != nil {
		return fmt.Errorf("recover work directory: %w", err)
	}

	telegramClient := telegram.New(cfg.TelegramToken, telegram.WithBaseURL(cfg.TelegramAPI))
	if err := telegramClient.DeleteWebhook(ctx); err != nil {
		return fmt.Errorf("delete telegram webhook: %w", err)
	}
	if err := telegramClient.SetMyCommands(ctx, bot.CommandMenu()); err != nil {
		// The menu is a convenience; polling must still start.
		logger.Warn("register telegram command menu", "error", err)
	}

	apiClient := opencode.NewClient(cfg.OpenCodeAPI, cfg.OpenCodeAPIPassword)
	apiRunner := jobs.NewAPIRunner(apiClient, cfg.OpenCodeUnit, func(restartCtx context.Context) error {
		// Fixed argv, executed directly: no shell and no user-controlled
		// arguments ever reach systemctl.
		return exec.CommandContext(restartCtx, "systemctl", "--user", "restart", cfg.OpenCodeUnit).Run()
	})

	runners := map[jobs.RunnerKind]jobs.Runner{
		jobs.RunnerCLI: jobs.NewCLIRunner(cfg.OpenCodeBin),
		jobs.RunnerAPI: apiRunner,
	}

	// The manager needs a notifier and the bot needs the manager. The binding
	// is completed before Start launches the worker, so a notification can
	// never observe a nil bot.
	var handler *bot.Bot
	notifier := notifierFunc{
		notify: func(notifyCtx context.Context, result jobs.Result) {
			if handler != nil {
				handler.Notify(notifyCtx, result)
			}
		},
		notifyStarted: func(notifyCtx context.Context, req jobs.Request) {
			if handler != nil {
				handler.NotifyStarted(notifyCtx, req)
			}
		},
	}
	manager := jobs.NewManager(cfg, store, cloner, runners, notifier, logger)

	handler = bot.New(cfg, store, manager, telegramClient, cfg.OwnerUserID,
		func(listCtx context.Context) ([]string, error) {
			return opencode.ListModels(listCtx, cfg.OpenCodeBin)
		}, logger)

	poller := telegram.NewPoller(telegramClient, store, cfg.OwnerUserID, handler, logger)

	manager.Start(ctx)
	pollErr := poller.Run(ctx)

	stopCtx, stopCancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer stopCancel()
	manager.Stop(stopCtx)

	if pollErr != nil {
		return fmt.Errorf("poll telegram: %w", pollErr)
	}
	return nil
}

// notifierFunc adapts functions to jobs.Notifier and jobs.StartNotifier
// without exposing the manager's construction order to callers.
type notifierFunc struct {
	notify        func(context.Context, jobs.Result)
	notifyStarted func(context.Context, jobs.Request)
}

// Notify implements jobs.Notifier.
func (f notifierFunc) Notify(ctx context.Context, result jobs.Result) {
	if f.notify != nil {
		f.notify(ctx, result)
	}
}

// NotifyStarted implements jobs.StartNotifier.
func (f notifierFunc) NotifyStarted(ctx context.Context, req jobs.Request) {
	if f.notifyStarted != nil {
		f.notifyStarted(ctx, req)
	}
}
