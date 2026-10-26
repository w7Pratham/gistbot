package bot

import (
	"bufio"
	"errors"
	"io"
	"os"
	"regexp"
	"sort"
	"strings"
	"time"

	"gistbot/internal/config"
	"gistbot/internal/jobs"
	"gistbot/internal/repo"
	"gistbot/internal/state"
	"gistbot/internal/telegram"
)

const (
	// maxLogLineRunes bounds one displayed log line so pathological output
	// cannot crowd out the rest of the tail.
	maxLogLineRunes = 500
	// maxLogLineBytes bounds how much of one line is buffered while reading.
	maxLogLineBytes = 4096
	// logReadBuffer is the streaming buffer size for tailFile.
	logReadBuffer = 64 * 1024
)

// docOrder is the canonical order used when presenting gist documents.
var docOrder = []string{"codemap", "routing", "observation"}

var (
	// ansiSequence matches CSI, OSC, and single-character ANSI escapes.
	ansiSequence = regexp.MustCompile(`\x1b\[[0-9;?]*[ -/]*[@-~]|\x1b\][^\x07\x1b]*(?:\x07|\x1b\\)|\x1b[@-_]`)
	// logControl matches control bytes other than newline and tab.
	logControl = regexp.MustCompile(`[\x00-\x08\x0b-\x1f\x7f]`)
)

// successHTML renders the summary for a successful job. Missing documents are
// reported on their own line; the caller sends the documents afterwards.
func successHTML(res jobs.Result) string {
	lines := []string{
		"Gist complete",
		targetDisplay(res.Request.Target),
		"model: " + modelDisplay(res.Request.ModelAlias, res.Request.Model),
		"runner: " + string(res.Request.Runner),
		"duration: " + formatDuration(durationOf(res)),
		"docs: " + docsLabel(presentDocNames(res.Docs.Files)),
	}
	if missing := missingDocNames(res.Docs); len(missing) > 0 {
		lines = append(lines, "Missing: "+strings.Join(missing, ", "))
	}

	return preBlock(telegram.EscapeAndTruncate(strings.Join(lines, "\n"), maxMessageUTF16-preOverhead))
}

// failureHTML renders a short status and, when available, the escaped tail of
// the job log.
func failureHTML(res jobs.Result, tail string) string {
	label := "Job"
	if res.Request.ID != "" {
		label += " " + res.Request.ID
	}

	lines := []string{
		label + " " + statusWord(res.Status),
		targetDisplay(res.Request.Target),
		"model: " + modelDisplay(res.Request.ModelAlias, res.Request.Model),
		"runner: " + string(res.Request.Runner),
		"duration: " + formatDuration(durationOf(res)),
	}
	if res.Err != nil {
		lines = append(lines, "error: "+res.Err.Error())
	}

	html := preBlock(telegram.EscapeAndTruncate(strings.Join(lines, "\n"), failureSummaryBudget))
	if escaped := escapeTail(tail, logTailBudget); escaped != "" {
		html += "\n" + preBlock(escaped)
	}
	return html
}

// tailFile returns the last maxLines lines of path with ANSI escapes and
// control bytes removed. It streams the file so an arbitrarily large job log
// uses bounded memory. Lines longer than maxLogLineRunes are cut with an
// ellipsis.
func tailFile(path string, maxLines int) (string, error) {
	if maxLines <= 0 {
		return "", nil
	}
	if path == "" {
		return "", errors.New("bot: log path is empty")
	}

	file, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer file.Close()

	reader := bufio.NewReaderSize(file, logReadBuffer)
	tail := make([]string, 0, maxLines)
	var line strings.Builder

	for {
		chunk, readErr := reader.ReadSlice('\n')
		if len(chunk) > 0 {
			endsLine := chunk[len(chunk)-1] == '\n'
			body := chunk
			if endsLine {
				body = body[:len(body)-1]
			}
			if line.Len() < maxLogLineBytes {
				remaining := maxLogLineBytes - line.Len()
				if len(body) > remaining {
					body = body[:remaining]
				}
				line.Write(body)
			}
			if endsLine {
				tail = appendTailLine(tail, sanitizeLogLine(line.String()), maxLines)
				line.Reset()
			}
		}

		if readErr == nil {
			continue
		}
		if errors.Is(readErr, bufio.ErrBufferFull) {
			continue
		}
		if errors.Is(readErr, io.EOF) {
			if line.Len() > 0 {
				tail = appendTailLine(tail, sanitizeLogLine(line.String()), maxLines)
			}
			break
		}
		return "", readErr
	}

	return strings.Join(tail, "\n"), nil
}

// appendTailLine keeps only the newest maxLines lines.
func appendTailLine(lines []string, line string, maxLines int) []string {
	if len(lines) == maxLines {
		copy(lines, lines[1:])
		lines = lines[:maxLines-1]
	}
	return append(lines, line)
}

// sanitizeLogLine removes ANSI escapes and control bytes and bounds the line
// length.
func sanitizeLogLine(line string) string {
	line = ansiSequence.ReplaceAllString(line, "")
	line = logControl.ReplaceAllString(line, "")

	runes := []rune(strings.TrimSuffix(line, "\r"))
	if len(runes) > maxLogLineRunes {
		return string(runes[:maxLogLineRunes]) + "…"
	}
	return string(runes)
}

// escapeTail escapes the newest log lines within budget UTF-16 code units.
// It walks backwards so truncation drops the oldest lines instead of the most
// recent output.
func escapeTail(raw string, budget int) string {
	if raw == "" || budget <= 0 {
		return ""
	}

	lines := strings.Split(raw, "\n")
	kept := make([]string, 0, len(lines))
	used := 0
	for i := len(lines) - 1; i >= 0; i-- {
		remaining := budget - used
		if remaining <= 0 {
			break
		}
		escaped := telegram.EscapeAndTruncate(lines[i], remaining)
		kept = append(kept, escaped)
		used += utf16Count(escaped) + 1 // account for the newline
	}

	for i, j := 0, len(kept)-1; i < j; i, j = i+1, j-1 {
		kept[i], kept[j] = kept[j], kept[i]
	}
	return strings.Join(kept, "\n")
}

// presentDocNames returns the documents that exist, in canonical order.
func presentDocNames(files map[string]string) []string {
	names := make([]string, 0, len(docOrder))
	for _, name := range docOrder {
		if _, ok := files[name]; ok {
			names = append(names, name)
		}
	}
	return names
}

// missingDocNames returns absent documents in canonical order, followed by any
// extra names reported by discovery.
func missingDocNames(docs repo.Docs) []string {
	seen := make(map[string]bool, len(docOrder))
	missing := make([]string, 0, len(docOrder))
	for _, name := range docOrder {
		if _, ok := docs.Files[name]; ok {
			continue
		}
		missing = append(missing, name)
		seen[name] = true
	}

	extra := make([]string, 0, len(docs.Missing))
	for _, name := range docs.Missing {
		if seen[name] {
			continue
		}
		extra = append(extra, name)
		seen[name] = true
	}
	sort.Strings(extra)

	return append(missing, extra...)
}

// docsLabel renders the present document list for the summary.
func docsLabel(names []string) string {
	if len(names) == 0 {
		return "none"
	}
	return strings.Join(names, ", ")
}

// readDoc reads at most maxDocBytes of one document.
func readDoc(path string) ([]byte, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()

	return io.ReadAll(io.LimitReader(file, maxDocBytes))
}

// displayRepo turns a canonical repository ID into owner/repo for display.
func displayRepo(id string) string {
	rest, ok := strings.CutPrefix(id, "github__")
	if !ok || rest == "" {
		if id == "" {
			return "unknown"
		}
		return id
	}
	owner, name, ok := strings.Cut(rest, "__")
	if !ok || owner == "" || name == "" {
		return id
	}
	return owner + "/" + name
}

// targetDisplay renders a parsed repository target.
func targetDisplay(target repo.Target) string {
	if target.Owner != "" && target.Name != "" {
		return target.Owner + "/" + target.Name
	}
	if target.URL != "" {
		return target.URL
	}
	if target.ID != "" {
		return target.ID
	}
	return "unknown"
}

// modelDisplay shows the alias, exposing the resolved reference for the free
// alias because the override is not visible from the alias alone.
func modelDisplay(alias string, ref config.ModelRef) string {
	reference := ""
	if ref.ProviderID != "" || ref.ModelID != "" {
		reference = ref.String()
	}
	switch {
	case alias == "":
		if reference == "" {
			return "unknown"
		}
		return reference
	case alias == aliasFree && reference != "":
		return alias + " (" + reference + ")"
	default:
		return alias
	}
}

// statusWord renders a terminal job status for a notification.
func statusWord(status state.JobStatus) string {
	switch status {
	case state.StatusFailed:
		return "failed"
	case state.StatusTimedOut:
		return "timed out"
	case state.StatusCancelled:
		return "cancelled"
	case state.StatusInterrupted:
		return "interrupted"
	case "":
		return "finished"
	default:
		return string(status)
	}
}

// formatDuration renders a duration at second precision.
func formatDuration(d time.Duration) string {
	if d < 0 {
		d = 0
	}
	d = d.Round(time.Second)
	if d == 0 {
		return "0s"
	}
	return d.String()
}

// durationOf reports how long a finished job ran.
func durationOf(res jobs.Result) time.Duration {
	if res.FinishedAt.Before(res.StartedAt) {
		return 0
	}
	return res.FinishedAt.Sub(res.StartedAt)
}

// preBlock wraps already-escaped content in a <pre> block.
func preBlock(escaped string) string {
	return "<pre>" + escaped + "</pre>"
}

// utf16Count counts UTF-16 code units, matching Telegram's message limit.
func utf16Count(s string) int {
	units := 0
	for _, r := range s {
		if r > 0xFFFF {
			units += 2
			continue
		}
		units++
	}
	return units
}
