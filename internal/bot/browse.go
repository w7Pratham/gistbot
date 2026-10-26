package bot

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"gistbot/internal/jobs"
	"gistbot/internal/repo"
	"gistbot/internal/state"
)

const (
	// listTimeLayout is the timestamp format used by /list.
	listTimeLayout = "2006-01-02 15:04"
	// maxLogCardLines is how many log lines /log shows in the chat card.
	maxLogCardLines = 10
)

// jobIDPattern mirrors the job manager's path-safety rule. A job ID is read
// from persisted state or the running job, never from a user, but it is still
// validated before it becomes a file name under LOG_DIR.
var jobIDPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`)

// handleList replies with a compact header and one line per installed
// repository, newest documents first.
func (b *Bot) handleList(ctx context.Context, chatID int64) {
	entries, err := repo.List(b.cfg.WorkDir)
	if err != nil {
		b.logWarn("list repositories", "error", err)
		b.reply(ctx, chatID, "Could not list repositories.")
		return
	}
	if len(entries) == 0 {
		b.reply(ctx, chatID, "No repositories installed.")
		return
	}

	b.send(ctx, chatID, listText(entries))
}

// handleShow resends installed gist documents. With no argument it uses the
// newest installed repository. Otherwise the repository must be a displayed
// owner/repo or a canonical repository ID and must already be installed; the
// optional document is one of the canonical names with an optional .md suffix.
// No caller-supplied value ever becomes a path.
func (b *Bot) handleShow(ctx context.Context, chatID int64, args []string) {
	if len(args) > 2 {
		b.reply(ctx, chatID, "Usage: /show [owner/repo|repo-id] [codemap|routing|observation]")
		return
	}

	var entry repo.Entry
	if len(args) == 0 {
		entries, err := repo.List(b.cfg.WorkDir)
		if err != nil {
			b.logWarn("list repositories", "error", err)
			b.reply(ctx, chatID, "Could not list repositories.")
			return
		}
		if len(entries) == 0 {
			b.reply(ctx, chatID, "No repositories installed.")
			return
		}
		entry = entries[0]
	} else {
		found := false
		var err error
		entry, found, err = b.installedRepo(args[0])
		if err != nil {
			b.logWarn("list repositories", "error", err)
			b.reply(ctx, chatID, "Could not list repositories.")
			return
		}
		if !found {
			b.reply(ctx, chatID, "Repository is not installed.")
			return
		}
	}

	wanted := ""
	if len(args) == 2 {
		name, ok := parseDocName(args[1])
		if !ok {
			b.reply(ctx, chatID, "Document must be codemap, routing, or observation.")
			return
		}
		wanted = name
	}

	dir := repo.RepoDir(b.cfg.WorkDir, entry.ID)
	if dir == "" {
		b.reply(ctx, chatID, "Repository is not installed.")
		return
	}
	docs, err := repo.DiscoverDocs(dir)
	if err != nil {
		b.logWarn("discover gist documents", "repo", entry.ID, "error", err)
		b.reply(ctx, chatID, "Could not read the installed documents.")
		return
	}

	files := make(map[string]string, len(docOrder))
	for name, path := range docs.Files {
		if wanted != "" && name != wanted {
			continue
		}
		if !safeDocFile(dir, path) {
			continue
		}
		files[name] = path
	}

	if len(files) == 0 {
		if wanted != "" {
			b.reply(ctx, chatID, fmt.Sprintf("%s is not available for %s.", wanted, entry.Display))
			return
		}
		b.reply(ctx, chatID, "No gist documents are installed for "+entry.Display+".")
		return
	}

	b.sendDocAlbum(ctx, chatID, repo.Docs{Files: files})
}

// handleLog replies with a compact card holding the tail of the current job
// log plus the full .log attachment. The optional argument names a repository,
// never a path.
func (b *Bot) handleLog(ctx context.Context, chatID int64, args []string) {
	if len(args) > 1 {
		b.reply(ctx, chatID, "Usage: /log [owner/repo|repo-id]")
		return
	}

	record, found := state.JobRecord{}, false
	if len(args) == 0 {
		if view := b.runningJob(); view != nil {
			record, found = state.JobRecord{ID: view.ID, RepoID: view.RepoID}, true
		} else {
			record, found = b.newestJob("")
		}
	} else {
		repoID, ok := canonicalRepoID(args[0])
		if !ok {
			b.reply(ctx, chatID, "Only an installed repository name or ID is accepted.")
			return
		}
		if view := b.runningJob(); view != nil && view.RepoID == repoID {
			record, found = state.JobRecord{ID: view.ID, RepoID: view.RepoID}, true
		} else {
			record, found = b.newestJob(repoID)
		}
		if !found {
			b.reply(ctx, chatID, "No jobs recorded for "+displayRepo(repoID)+".")
			return
		}
	}

	if !found {
		b.reply(ctx, chatID, "No jobs recorded.")
		return
	}

	path, ok := b.jobLogPath(record.ID)
	if !ok {
		b.reply(ctx, chatID, "The job log is not available.")
		return
	}
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() {
		b.reply(ctx, chatID, "The job log is not available.")
		return
	}
	tail, err := tailFile(path, maxLogCardLines)
	if err != nil {
		b.logWarn("read job log", "job", record.ID, "error", err)
		b.reply(ctx, chatID, "The job log is not available.")
		return
	}

	b.send(ctx, chatID, logCardHTML(record, tail))
	b.sendLogFile(ctx, chatID, jobs.Result{
		Request: jobs.Request{ID: record.ID, ChatID: chatID},
		LogPath: path,
	})
}

// installedRepo resolves a displayed owner/repo or canonical ID against the
// repositories currently installed under WORK_DIR.
func (b *Bot) installedRepo(input string) (repo.Entry, bool, error) {
	id, ok := canonicalRepoID(input)
	if !ok {
		return repo.Entry{}, false, nil
	}
	entries, err := repo.List(b.cfg.WorkDir)
	if err != nil {
		return repo.Entry{}, false, err
	}
	for _, entry := range entries {
		if entry.ID == id {
			return entry, true, nil
		}
	}
	return repo.Entry{}, false, nil
}

// canonicalRepoID converts an owner/repo or an existing canonical ID to the
// canonical ID, rejecting anything that is not a single repository reference.
func canonicalRepoID(input string) (string, bool) {
	input = strings.TrimSpace(input)
	if input == "" {
		return "", false
	}
	if repo.ValidID(input) {
		return input, true
	}

	target, err := repo.ParseGitHubURL("https://github.com/" + input)
	if err != nil || !repo.ValidID(target.ID) {
		return "", false
	}
	return target.ID, true
}

// parseDocName accepts one canonical document name with an optional .md
// suffix. Anything path-like is rejected.
func parseDocName(raw string) (string, bool) {
	name := strings.TrimSuffix(strings.ToLower(strings.TrimSpace(raw)), ".md")
	switch name {
	case "codemap", "routing", "observation":
		return name, true
	default:
		return "", false
	}
}

// safeDocFile reports whether path is a regular, non-symlink file inside dir.
// DiscoverDocs already enforces this; the check is repeated immediately before
// the file is read so a swapped path can never escape the repository.
func safeDocFile(dir, path string) bool {
	if !filepath.IsAbs(path) {
		return false
	}
	rel, err := filepath.Rel(dir, path)
	if err != nil || rel == "." || filepath.IsAbs(rel) {
		return false
	}
	if rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return false
	}

	info, err := os.Lstat(path)
	if err != nil {
		return false
	}
	return info.Mode().IsRegular()
}

// runningJob returns the job manager's current job, if any.
func (b *Bot) runningJob() *jobs.JobView {
	if b.jobs == nil {
		return nil
	}
	return b.jobs.Status().Running
}

// newestJob returns the newest recorded job, optionally restricted to repoID.
// The job history is ordered by creation, so the later record wins ties.
func (b *Bot) newestJob(repoID string) (state.JobRecord, bool) {
	if b.state == nil {
		return state.JobRecord{}, false
	}

	records := b.state.Snapshot().Jobs
	best := state.JobRecord{}
	bestTime := time.Time{}
	found := false
	for _, record := range records {
		if repoID != "" && record.RepoID != repoID {
			continue
		}
		stamp := jobTimestamp(record)
		if !found || newerJobStamp(stamp, bestTime) {
			best = record
			bestTime = stamp
			found = true
		}
	}
	return best, found
}

// newerJobStamp reports whether candidate is newer than the current best time.
// A record with no timestamp is still queued (the manager stamps StartedAt
// before running), so it outranks an older completed record; equal timestamps
// fall back to history order, whose later entries are newer.
func newerJobStamp(candidate, best time.Time) bool {
	switch {
	case candidate.IsZero():
		return true
	case best.IsZero():
		return false
	default:
		return !candidate.Before(best)
	}
}

// jobTimestamp is the best available start time for ordering job history.
func jobTimestamp(record state.JobRecord) time.Time {
	if !record.StartedAt.IsZero() {
		return record.StartedAt
	}
	if !record.FinishedAt.IsZero() {
		return record.FinishedAt
	}
	return time.Time{}
}

// jobLogPath builds the log path for a validated job ID. User input is never
// part of the path.
func (b *Bot) jobLogPath(jobID string) (string, bool) {
	if !jobIDPattern.MatchString(jobID) {
		return "", false
	}
	if b.cfg.LogDir == "" || !filepath.IsAbs(b.cfg.LogDir) {
		return "", false
	}
	dir := filepath.Clean(b.cfg.LogDir)
	path := filepath.Join(dir, jobID+".log")
	if filepath.Dir(path) != dir {
		return "", false
	}
	return path, true
}
