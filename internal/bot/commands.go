package bot

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"gistbot/internal/config"
	"gistbot/internal/jobs"
	"gistbot/internal/repo"
	"gistbot/internal/state"
)

const (
	// maxFocusRunes bounds the free-form focus appended to a gist job.
	maxFocusRunes = 200

	runnerCLI = "cli"
	runnerAPI = "api"
	aliasFree = "free"
)

// selection is the persisted model and runner resolved for one new job.
type selection struct {
	alias  string
	model  config.ModelRef
	runner jobs.RunnerKind
}

// parseCommand extracts a command name and arguments from message text. It
// strips a leading slash and an optional @BotName suffix, and lowercases only
// the command name; arguments keep their original spelling.
func parseCommand(text string) (string, []string, bool) {
	text = strings.TrimSpace(text)
	if !strings.HasPrefix(text, "/") {
		return "", nil, false
	}
	fields := strings.Fields(text)
	if len(fields) == 0 {
		return "", nil, false
	}

	name := strings.TrimPrefix(fields[0], "/")
	if i := strings.IndexByte(name, '@'); i >= 0 {
		name = name[:i]
	}
	if name == "" {
		return "", nil, false
	}
	return strings.ToLower(name), fields[1:], true
}

func (b *Bot) handleHelp(ctx context.Context, chatID int64) {
	b.replyPre(ctx, chatID, b.helpText())
}

func (b *Bot) handleGist(ctx context.Context, chatID int64, args []string) {
	if len(args) == 0 {
		b.reply(ctx, chatID, "Usage: /gist https://github.com/owner/repo [focus]")
		return
	}

	target, err := repo.ParseGitHubURL(args[0])
	if err != nil {
		b.reply(ctx, chatID, "Only https://github.com/owner/repo URLs are supported.")
		return
	}

	focus, err := sanitizeFocus(strings.Join(args[1:], " "))
	if err != nil {
		b.reply(ctx, chatID, "Focus is too long (max 200 characters).")
		return
	}

	if b.jobs == nil {
		b.reply(ctx, chatID, "Jobs are not available.")
		return
	}

	selected, err := b.currentSelection()
	if err != nil {
		b.logWarn("resolve job selection", "repo", target.ID, "error", err)
		b.reply(ctx, chatID, "The selected model or runner is not configured.")
		return
	}

	now := b.clock()
	req := jobs.Request{
		ID:         b.nextJobID(now),
		Target:     target,
		Focus:      focus,
		ChatID:     chatID,
		Runner:     selected.runner,
		Model:      selected.model,
		ModelAlias: selected.alias,
		EnqueuedAt: now,
	}

	position, err := b.jobs.Enqueue(req)
	if err != nil {
		switch {
		case errors.Is(err, jobs.ErrQueueFull):
			b.reply(ctx, chatID, "Queue is full. Try again after the current job finishes.")
		default:
			b.logWarn("enqueue job", "repo", target.ID, "error", err)
			b.reply(ctx, chatID, "Could not queue the job.")
		}
		return
	}

	label := target.Owner + "/" + target.Name
	model := modelDisplay(selected.alias, selected.model)
	if position <= 0 {
		b.reply(ctx, chatID, fmt.Sprintf("Started %s · %s · %s", label, model, selected.runner))
		return
	}
	b.reply(ctx, chatID, fmt.Sprintf("Queued #%d · %s · %s · %s", position, label, model, selected.runner))
}

func (b *Bot) handleStatus(ctx context.Context, chatID int64) {
	if b.jobs == nil {
		b.reply(ctx, chatID, "Jobs are not available.")
		return
	}
	b.replyPre(ctx, chatID, statusText(b.jobs.Status()))
}

func (b *Bot) handleCancel(ctx context.Context, chatID int64) {
	if b.jobs == nil {
		b.reply(ctx, chatID, "Jobs are not available.")
		return
	}
	if b.jobs.Cancel(ctx) {
		b.reply(ctx, chatID, "Cancel requested. The worker reports when the job stops.")
		return
	}
	b.reply(ctx, chatID, "No running job.")
}

// currentSelection resolves the model alias and runner persisted in state,
// falling back to the configured defaults. The free-model override only
// applies to the free alias.
func (b *Bot) currentSelection() (selection, error) {
	data := state.Data{}
	if b.state != nil {
		data = b.state.Snapshot()
	}

	alias := data.Model
	if alias == "" {
		alias = b.cfg.DefaultModel
	}
	runner := data.Runner
	if runner == "" {
		runner = b.cfg.DefaultRunner
	}

	var kind jobs.RunnerKind
	switch runner {
	case runnerCLI:
		kind = jobs.RunnerCLI
	case runnerAPI:
		kind = jobs.RunnerAPI
	default:
		return selection{}, errors.New("runner is not configured")
	}

	ref, err := b.cfg.ResolveModel(alias, data.FreeModel)
	if err != nil {
		return selection{}, err
	}

	return selection{
		alias:  alias,
		model:  ref,
		runner: kind,
	}, nil
}

// helpText lists the commands and the current model, free override, runner, and
// job state. Commands introduced in later tasks are listed here already.
func (b *Bot) helpText() string {
	data := state.Data{}
	if b.state != nil {
		data = b.state.Snapshot()
	}

	alias := data.Model
	if alias == "" {
		alias = b.cfg.DefaultModel
	}
	runner := data.Runner
	if runner == "" {
		runner = b.cfg.DefaultRunner
	}

	model := alias
	if ref, err := b.cfg.ResolveModel(alias, data.FreeModel); err == nil &&
		(ref.ProviderID != "" || ref.ModelID != "") {
		model = alias + " (" + ref.String() + ")"
	}

	free := "none"
	if override := strings.TrimSpace(data.FreeModel); override != "" {
		free = override
	}

	return strings.Join([]string{
		"Zero Two",
		"",
		"Commands",
		"/gist <github-url> [focus] — write gist docs for a repository",
		"/status — show the running job and queued jobs",
		"/cancel — stop the running job",
		"/list — list installed repositories",
		"/show <repo> [doc] — resend installed gist docs",
		"/log [repo] — show the latest job log",
		"/model [alias] — show or select a model",
		"/runner [cli|api] — show or select a runner",
		"/help — show this help",
		"",
		"Model: " + model,
		"Free override: " + free,
		"Runner: " + runner,
		"Job: " + b.jobStateText(),
	}, "\n")
}

// jobStateText summarizes the manager state for /help.
func (b *Bot) jobStateText() string {
	if b.jobs == nil {
		return "unknown"
	}
	status := b.jobs.Status()
	if status.Running != nil {
		return "running " + displayRepo(status.Running.RepoID)
	}
	if len(status.Queued) > 0 {
		return fmt.Sprintf("idle (%d queued)", len(status.Queued))
	}
	return "idle"
}

// statusText renders the running job and the queued positions for /status.
func statusText(status jobs.Status) string {
	var lines []string
	if status.Running != nil {
		lines = append(lines, "Running", jobLine(*status.Running, true))
	} else {
		lines = append(lines, "No job running.")
	}

	if len(status.Queued) > 0 {
		lines = append(lines, "", "Queued")
		for i, job := range status.Queued {
			lines = append(lines, fmt.Sprintf("#%d %s", i+1, jobLine(job, false)))
		}
	} else if status.Running == nil {
		lines = []string{"No jobs running."}
	}

	return strings.Join(lines, "\n")
}

// jobLine renders the fields shared by running and queued job views.
func jobLine(view jobs.JobView, withElapsed bool) string {
	parts := []string{displayRepo(view.RepoID)}
	if view.Model != "" {
		parts = append(parts, view.Model)
	}
	if view.Runner != "" {
		parts = append(parts, view.Runner)
	}
	if withElapsed {
		parts = append(parts, formatDuration(view.Elapsed))
	}
	return strings.Join(parts, " · ")
}

// sanitizeFocus strips control and format characters, collapses whitespace, and
// rejects a focus longer than maxFocusRunes runes.
func sanitizeFocus(raw string) (string, error) {
	var built strings.Builder
	for _, r := range raw {
		switch {
		case unicode.IsSpace(r):
			built.WriteByte(' ')
		case unicode.IsControl(r), unicode.Is(unicode.Cf, r):
			continue
		default:
			built.WriteRune(r)
		}
	}

	focus := strings.Join(strings.Fields(built.String()), " ")
	if utf8.RuneCountInString(focus) > maxFocusRunes {
		return "", errors.New("focus is too long")
	}
	return focus, nil
}

// nextJobID builds a unique, safe path component. The manager validates the ID
// before any path is constructed from it.
func (b *Bot) nextJobID(now time.Time) string {
	return fmt.Sprintf("job-%d-%d", now.UnixNano(), b.seq.Add(1))
}
