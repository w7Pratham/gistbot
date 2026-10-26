package bot

import (
	"bufio"
	"errors"
	"fmt"
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
	// logTruncatedMarker prefixes a capped .log attachment.
	logTruncatedMarker = "[log truncated: showing the most recent output]\n"
)

// docOrder is the canonical order used when presenting gist documents.
var docOrder = []string{"codemap", "routing", "observation"}

var (
	// ansiSequence matches CSI, OSC, and single-character ANSI escapes.
	ansiSequence = regexp.MustCompile(`\x1b\[[0-9;?]*[ -/]*[@-~]|\x1b\][^\x07\x1b]*(?:\x07|\x1b\\)|\x1b[@-_]`)
	// logControl matches control bytes other than newline and tab.
	logControl = regexp.MustCompile(`[\x00-\x08\x0b-\x1f\x7f]`)
)

// queuedCardHTML renders the initial card for a job waiting behind others.
func queuedCardHTML(req jobs.Request, position int) string {
	return strings.Join([]string{
		cardTitle(fmt.Sprintf("🕓 Queued #%d", position), targetDisplay(req.Target)),
		cardLine(requestMeta(req)),
	}, "\n")
}

// runningCardHTML renders the card for a job the worker has started.
func runningCardHTML(req jobs.Request, started time.Time) string {
	return strings.Join([]string{
		cardTitle("⚡ Running", targetDisplay(req.Target)),
		cardLine(requestMeta(req) + " · started " + started.Format("15:04")),
	}, "\n")
}

// successCardHTML renders the terminal card for a successful job. Missing
// documents are reported on their own line; the caller sends the docs after.
func successCardHTML(res jobs.Result) string {
	names := presentDocNames(res.Docs.Files)
	lines := []string{
		cardTitle("✅ Gist ready", targetDisplay(res.Request.Target)),
		cardLine(requestMeta(res.Request) + " · " + cardDuration(durationOf(res)) + " · " + docCountLabel(len(names))),
	}
	if missing := missingDocNames(res.Docs); len(missing) > 0 {
		lines = append(lines, cardField("Missing: "+strings.Join(missing, ", ")))
	}
	return strings.Join(lines, "\n")
}

// failureCardHTML renders the terminal card for a failed, cancelled, or timed
// out job and appends the escaped newest log lines inside a <pre> block.
func failureCardHTML(res jobs.Result, tail string) string {
	lines := []string{
		cardTitle(failureTitle(res.Status), targetDisplay(res.Request.Target)),
		cardField(failureReason(res)),
		cardLine(requestMeta(res.Request) + " · " + cardDuration(durationOf(res))),
	}
	return appendLogTail(strings.Join(lines, "\n"), tail)
}

// cardTitle renders the bold status and monospaced repository line.
func cardTitle(status, target string) string {
	return "<b>" + cardEscape(status) + "</b> · <code>" + cardEscape(target) + "</code>"
}

// cardLine renders one monospaced metadata line.
func cardLine(text string) string {
	return "<code>" + cardEscape(text) + "</code>"
}

// cardField renders one escaped plain-text line.
func cardField(text string) string {
	return cardEscape(text)
}

// cardEscape HTML-escapes and bounds one variable card field.
func cardEscape(text string) string {
	return telegram.EscapeAndTruncate(text, cardFieldBudget)
}

// appendLogTail appends as many escaped newest log lines as the 4,096 UTF-16
// message budget leaves room for.
func appendLogTail(header, tail string) string {
	if tail == "" {
		return header
	}
	remaining := maxMessageUTF16 - utf16Count(header) - preOverhead - len("\n\n")
	if remaining <= 0 {
		return header
	}
	if remaining > logTailBudget {
		remaining = logTailBudget
	}
	escaped := escapeTail(tail, remaining)
	if escaped == "" {
		return header
	}
	return header + "\n\n" + preBlock(escaped)
}

// requestMeta renders the model and runner used by a job.
func requestMeta(req jobs.Request) string {
	runner := string(req.Runner)
	if runner == "" {
		runner = "unknown"
	}
	return modelDisplay(req.ModelAlias, req.Model) + " · " + runner
}

// docCountLabel renders the number of delivered gist documents.
func docCountLabel(count int) string {
	if count == 1 {
		return "1 doc"
	}
	return fmt.Sprintf("%d docs", count)
}

// failureTitle renders the bold status label for a terminal failure.
func failureTitle(status state.JobStatus) string {
	switch status {
	case state.StatusCancelled:
		return "🚫 Cancelled"
	case state.StatusTimedOut:
		return "⌛ Timed out"
	default:
		return "❌ Gist failed"
	}
}

// failureReason prefers the concrete error and falls back to the status word.
func failureReason(res jobs.Result) string {
	if res.Err != nil {
		return res.Err.Error()
	}
	return statusWord(res.Status)
}

// cardDuration renders a duration in the card style: "1m 38s", "20m 00s".
func cardDuration(d time.Duration) string {
	if d < 0 {
		d = 0
	}
	d = d.Round(time.Second)
	hours := int(d / time.Hour)
	minutes := int(d%time.Hour) / int(time.Minute)
	seconds := int(d%time.Minute) / int(time.Second)
	switch {
	case hours > 0:
		return fmt.Sprintf("%dh %02dm %02ds", hours, minutes, seconds)
	case minutes > 0:
		return fmt.Sprintf("%dm %02ds", minutes, seconds)
	default:
		return fmt.Sprintf("%ds", seconds)
	}
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

// readDoc reads at most maxDocBytes of one document.
func readDoc(path string) ([]byte, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()

	return io.ReadAll(io.LimitReader(file, maxDocBytes))
}

// readLogAttachment reads the newest bytes of a job log. The returned data
// never exceeds maxLogAttachmentBytes; when the log is larger, truncated is
// true and the caller prefixes the marker whose length was reserved here.
func readLogAttachment(path string) (data []byte, truncated bool, err error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, false, err
	}
	defer file.Close()

	info, err := file.Stat()
	if err != nil {
		return nil, false, err
	}

	limit := int64(maxLogAttachmentBytes)
	truncated = info.Size() > limit
	if truncated {
		limit -= int64(len(logTruncatedMarker))
		if limit < 0 {
			limit = 0
		}
	}
	if info.Size() > limit {
		if _, err := file.Seek(info.Size()-limit, io.SeekStart); err != nil {
			return nil, false, err
		}
	}

	data, err = io.ReadAll(io.LimitReader(file, limit))
	if err != nil {
		return nil, false, err
	}
	return data, truncated, nil
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
