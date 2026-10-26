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

	runnerCLI  = "cli"
	runnerAPI  = "api"
	aliasFree  = "free"
	aliasFlash = "flash"
	aliasPro   = "pro"
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

	if position <= 0 {
		b.sendJobCard(ctx, chatID, req, runningCardHTML(req, now))
		return
	}
	b.sendJobCard(ctx, chatID, req, queuedCardHTML(req, position))
}

// sendJobCard sends a job's initial card and remembers its message ID. A failed
// send is logged and does not affect the queued job; the final notification
// then falls back to a new card.
func (b *Bot) sendJobCard(ctx context.Context, chatID int64, req jobs.Request, html string) {
	if b.sender == nil {
		return
	}
	messageID, err := b.sender.SendMessageID(ctx, chatID, html)
	if err != nil {
		b.logWarn("send job card", "job", req.ID, "error", err)
		return
	}
	b.storeCard(req.ID, messageID)
}

func (b *Bot) handleStatus(ctx context.Context, chatID int64) {
	if b.jobs == nil {
		b.reply(ctx, chatID, "Jobs are not available.")
		return
	}
	text := b.settingsText() + "\n\n" + statusText(b.jobs.Status())
	b.replyPre(ctx, chatID, text)
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

// handleModel shows the current selection or persists a new model alias. The
// free alias additionally accepts an installed provider/model[#variant]
// override or "reset" to fall back to the configured free model.
func (b *Bot) handleModel(ctx context.Context, chatID int64, args []string) {
	if len(args) == 0 {
		b.reply(ctx, chatID, strings.Join(b.modelLines(), "\n"))
		return
	}
	if b.state == nil {
		b.reply(ctx, chatID, "State is not available.")
		return
	}

	alias := strings.ToLower(strings.TrimSpace(args[0]))
	switch alias {
	case aliasFree:
		switch {
		case len(args) == 1:
			b.persistAlias(ctx, chatID, aliasFree)
		case len(args) > 2:
			b.reply(ctx, chatID, "Usage: /model free [provider/model[#variant]|reset]")
		case strings.EqualFold(strings.TrimSpace(args[1]), "reset"):
			if err := b.state.SetModel(aliasFree); err != nil {
				b.logWarn("persist model selection", "error", err)
				b.reply(ctx, chatID, "Could not save the model selection.")
				return
			}
			if err := b.state.ClearFreeModel(); err != nil {
				b.logWarn("clear free model override", "error", err)
				b.reply(ctx, chatID, "Could not save the model selection.")
				return
			}
			b.reply(ctx, chatID, strings.Join(b.modelLines(), "\n"))
		default:
			value := strings.TrimSpace(args[1])
			if !b.validateFreeModel(ctx, chatID, value) {
				return
			}
			if err := b.state.SetModel(aliasFree); err != nil {
				b.logWarn("persist model selection", "error", err)
				b.reply(ctx, chatID, "Could not save the model selection.")
				return
			}
			if err := b.state.SetFreeModel(value); err != nil {
				b.logWarn("persist free model override", "error", err)
				b.reply(ctx, chatID, "Could not save the model selection.")
				return
			}
			b.reply(ctx, chatID, strings.Join(b.modelLines(), "\n"))
		}
	case aliasFlash, aliasPro:
		if len(args) > 1 {
			b.reply(ctx, chatID, "Usage: /model "+alias)
			return
		}
		b.persistAlias(ctx, chatID, alias)
	default:
		b.reply(ctx, chatID, "Unknown model. Use free, flash, or pro.")
	}
}

// persistAlias stores the selected alias. An existing free override is kept;
// only "/model free reset" clears it.
func (b *Bot) persistAlias(ctx context.Context, chatID int64, alias string) {
	if err := b.state.SetModel(alias); err != nil {
		b.logWarn("persist model selection", "error", err)
		b.reply(ctx, chatID, "Could not save the model selection.")
		return
	}
	b.reply(ctx, chatID, strings.Join(b.modelLines(), "\n"))
}

// validateFreeModel checks that raw is provider/model[#variant] and that the
// base provider/model exactly matches one of the models installed in OpenCode.
func (b *Bot) validateFreeModel(ctx context.Context, chatID int64, raw string) bool {
	ref, err := config.ParseModelRef(raw)
	if err != nil {
		b.reply(ctx, chatID, "Model reference is invalid. Use provider/model with an optional #variant.")
		return false
	}
	if b.listModels == nil {
		b.reply(ctx, chatID, "Could not list installed models.")
		return false
	}
	installed, err := b.listModels(ctx)
	if err != nil {
		b.logWarn("list installed models", "error", err)
		b.reply(ctx, chatID, "Could not list installed models.")
		return false
	}

	base := ref.ProviderID + "/" + ref.ModelID
	for _, id := range installed {
		if id == base {
			return true
		}
	}
	b.reply(ctx, chatID, "Unknown model. Use an installed provider/model.")
	return false
}

// modelLines renders the alias, resolved reference, free default, and current
// override for /model.
func (b *Bot) modelLines() []string {
	data := state.Data{}
	if b.state != nil {
		data = b.state.Snapshot()
	}
	alias := data.Model
	if alias == "" {
		alias = b.cfg.DefaultModel
	}
	override := strings.TrimSpace(data.FreeModel)

	model := alias
	if ref, err := b.cfg.ResolveModel(alias, override); err == nil {
		model = aliasWithRef(alias, ref)
	}

	freeDefault := "not configured"
	if ref, ok := b.cfg.Models[aliasFree]; ok && (ref.ProviderID != "" || ref.ModelID != "") {
		freeDefault = ref.String()
	}

	shownOverride := "none"
	if override != "" {
		shownOverride = override
	}

	return []string{
		"Model: " + model,
		"Free default: " + freeDefault,
		"Free override: " + shownOverride,
		"Aliases: free, flash, pro",
	}
}

// handleRunner shows the selected runner or persists cli/api.
func (b *Bot) handleRunner(ctx context.Context, chatID int64, args []string) {
	if len(args) == 0 {
		b.reply(ctx, chatID, "Runner: "+b.activeRunner())
		return
	}
	if len(args) > 1 {
		b.reply(ctx, chatID, "Usage: /runner [cli|api]")
		return
	}

	runner := strings.ToLower(strings.TrimSpace(args[0]))
	switch runner {
	case runnerCLI, runnerAPI:
		if b.state == nil {
			b.reply(ctx, chatID, "State is not available.")
			return
		}
		if err := b.state.SetRunner(runner); err != nil {
			b.logWarn("persist runner selection", "error", err)
			b.reply(ctx, chatID, "Could not save the runner selection.")
			return
		}
		b.reply(ctx, chatID, "Runner: "+runner)
	default:
		b.reply(ctx, chatID, "Unknown runner. Use cli or api.")
	}
}

// settingsText renders the active runner, model, and free override for /status.
func (b *Bot) settingsText() string {
	data := state.Data{}
	if b.state != nil {
		data = b.state.Snapshot()
	}
	alias := data.Model
	if alias == "" {
		alias = b.cfg.DefaultModel
	}
	override := strings.TrimSpace(data.FreeModel)

	model := alias
	if ref, err := b.cfg.ResolveModel(alias, override); err == nil {
		model = aliasWithRef(alias, ref)
	}

	shownOverride := "none"
	if override != "" {
		shownOverride = override
	}

	return strings.Join([]string{
		"Runner: " + b.activeRunner(),
		"Model: " + model,
		"Free override: " + shownOverride,
	}, "\n")
}

// activeRunner returns the persisted runner or the configured default.
func (b *Bot) activeRunner() string {
	runner := ""
	if b.state != nil {
		runner = b.state.Snapshot().Runner
	}
	if runner == "" {
		runner = b.cfg.DefaultRunner
	}
	if runner == "" {
		return "unknown"
	}
	return runner
}

// aliasWithRef renders an alias with its resolved reference.
func aliasWithRef(alias string, ref config.ModelRef) string {
	if ref.ProviderID == "" && ref.ModelID == "" {
		return alias
	}
	if alias == "" {
		return ref.String()
	}
	return alias + " (" + ref.String() + ")"
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
