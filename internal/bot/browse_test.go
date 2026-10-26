package bot

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"gistbot/internal/jobs"
	"gistbot/internal/state"
)

// installRepo creates an installed repository directory with gist documents
// under the fixture's WORK_DIR and returns its canonical ID.
func installRepo(t *testing.T, f *botFixture, owner, name string,
	docs map[string]string, modTime time.Time) string {
	t.Helper()

	id := "github__" + strings.ToLower(owner) + "__" + strings.ToLower(name)
	dir := filepath.Join(f.cfg.WorkDir, id)
	for doc, body := range docs {
		path := filepath.Join(dir, "docs", "gist", doc+".md")
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatalf("create docs directory: %v", err)
		}
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatalf("write %s: %v", path, err)
		}
		if !modTime.IsZero() {
			if err := os.Chtimes(path, modTime, modTime); err != nil {
				t.Fatalf("set mtime on %s: %v", path, err)
			}
		}
	}
	return id
}

// writeJobLog writes a private job log under the fixture's LOG_DIR.
func writeJobLog(t *testing.T, f *botFixture, jobID, body string) {
	t.Helper()

	if err := os.MkdirAll(f.cfg.LogDir, 0o700); err != nil {
		t.Fatalf("create log directory: %v", err)
	}
	path := filepath.Join(f.cfg.LogDir, jobID+".log")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write job log: %v", err)
	}
}

// addJobRecord appends one job history record to the fixture's state store.
func addJobRecord(t *testing.T, f *botFixture, id, repoID string, started time.Time) {
	t.Helper()

	record := state.JobRecord{
		ID:         id,
		RepoID:     repoID,
		URL:        "https://github.com/" + displayRepo(repoID),
		Runner:     "cli",
		Model:      "flash",
		Status:     state.StatusSucceeded,
		StartedAt:  started,
		FinishedAt: started.Add(time.Minute),
	}
	if err := f.store.UpsertJob(record); err != nil {
		t.Fatalf("UpsertJob(%q): %v", id, err)
	}
}

func TestListShowsDisplayNameAndDateNewestFirst(t *testing.T) {
	f := newBotFixture(t)

	older := time.Date(2026, time.January, 2, 3, 4, 0, 0, time.Local)
	newer := time.Date(2026, time.February, 3, 4, 5, 0, 0, time.Local)
	installRepo(t, f, "Alpha", "One", map[string]string{
		"codemap": "ALPHA CODEMAP",
		"routing": "ALPHA ROUTING",
	}, older)
	installRepo(t, f, "Beta", "Two", map[string]string{
		"observation": "BETA OBSERVATION",
	}, newer)

	f.handle(privateUpdate("/list"))

	texts := f.sender.messageTexts()
	if len(texts) != 1 {
		t.Fatalf("messages = %d, want 1", len(texts))
	}
	text := texts[0]

	for _, want := range []string{
		"<pre>",
		"beta/two — 2026-02-03 04:05 — observation",
		"alpha/one — 2026-01-02 03:04 — codemap,routing",
	} {
		requireContains(t, text, want)
	}

	newest := strings.Index(text, "beta/two")
	oldest := strings.Index(text, "alpha/one")
	if newest < 0 || oldest < 0 || newest > oldest {
		t.Errorf("repositories are not newest first:\n%s", text)
	}
	requireNotContains(t, text, f.cfg.WorkDir)
}

func TestShowAllDocsSendsThreeFiles(t *testing.T) {
	cases := []struct {
		label string
		arg   string
	}{
		{label: "display name", arg: "example/project"},
		{label: "canonical id", arg: "github__example__project"},
	}

	for _, tc := range cases {
		t.Run(tc.label, func(t *testing.T) {
			f := newBotFixture(t)
			installRepo(t, f, "example", "project", map[string]string{
				"codemap":     "CODEMAP",
				"routing":     "ROUTING",
				"observation": "OBSERVATION",
			}, time.Time{})

			f.handle(privateUpdate("/show " + tc.arg))

			sent := f.sender.sentDocuments()
			if len(sent) != 3 {
				t.Fatalf("documents = %d, want 3", len(sent))
			}
			wantNames := []string{"codemap.md", "routing.md", "observation.md"}
			wantBodies := []string{"CODEMAP", "ROUTING", "OBSERVATION"}
			for i, doc := range sent {
				if doc.chatID != testChatID {
					t.Errorf("document[%d] chatID = %d, want %d", i, doc.chatID, testChatID)
				}
				if doc.filename != wantNames[i] {
					t.Errorf("document[%d] filename = %q, want %q", i, doc.filename, wantNames[i])
				}
				if string(doc.data) != wantBodies[i] {
					t.Errorf("document[%d] body = %q, want %q", i, doc.data, wantBodies[i])
				}
			}
		})
	}
}

func TestShowOneDocSendsOneFile(t *testing.T) {
	cases := []struct {
		arg      string
		filename string
		body     string
	}{
		{arg: "codemap", filename: "codemap.md", body: "CODEMAP"},
		{arg: "routing.md", filename: "routing.md", body: "ROUTING"},
		{arg: "observation", filename: "observation.md", body: "OBSERVATION"},
	}

	for _, tc := range cases {
		t.Run(tc.arg, func(t *testing.T) {
			f := newBotFixture(t)
			installRepo(t, f, "example", "project", map[string]string{
				"codemap":     "CODEMAP",
				"routing":     "ROUTING",
				"observation": "OBSERVATION",
			}, time.Time{})

			f.handle(privateUpdate("/show example/project " + tc.arg))

			sent := f.sender.sentDocuments()
			if len(sent) != 1 {
				t.Fatalf("documents = %d, want 1", len(sent))
			}
			if sent[0].filename != tc.filename {
				t.Errorf("filename = %q, want %q", sent[0].filename, tc.filename)
			}
			if string(sent[0].data) != tc.body {
				t.Errorf("body = %q, want %q", sent[0].data, tc.body)
			}
		})
	}
}

func TestShowRejectsTraversal(t *testing.T) {
	f := newBotFixture(t)
	installRepo(t, f, "example", "project", map[string]string{
		"codemap": "CODEMAP",
	}, time.Time{})

	inputs := []string{
		"/show ../../etc/passwd",
		"/show example/project ../../etc/passwd",
		"/show github__example__project ../codemap",
		"/show /etc/passwd",
	}

	for _, input := range inputs {
		f.handle(privateUpdate(input))
	}

	if sent := f.sender.sentDocuments(); len(sent) != 0 {
		t.Fatalf("documents = %d, want 0", len(sent))
	}
	texts := f.sender.messageTexts()
	if len(texts) != len(inputs) {
		t.Fatalf("messages = %d, want %d", len(texts), len(inputs))
	}
	for i, text := range texts {
		requireNotContains(t, text, "root:")
		requireNotContains(t, text, f.cfg.WorkDir)
		if !strings.Contains(text, "installed") && !strings.Contains(text, "Document") &&
			!strings.Contains(text, "Repo") {
			t.Errorf("message[%d] does not explain rejection: %q", i, text)
		}
	}
}

func TestShowRejectsUnknownRepoAndUnknownDoc(t *testing.T) {
	f := newBotFixture(t)
	installRepo(t, f, "example", "project", map[string]string{
		"codemap": "CODEMAP",
	}, time.Time{})

	t.Run("unknown display repo", func(t *testing.T) {
		f.handle(privateUpdate("/show example/missing"))
		if sent := f.sender.sentDocuments(); len(sent) != 0 {
			t.Fatalf("documents = %d, want 0", len(sent))
		}
		texts := f.sender.messageTexts()
		if len(texts) != 1 {
			t.Fatalf("messages = %d, want 1", len(texts))
		}
		requireContains(t, texts[0], "not installed")
	})

	t.Run("unknown canonical repo", func(t *testing.T) {
		f.handle(privateUpdate("/show github__example__missing"))
		if sent := f.sender.sentDocuments(); len(sent) != 0 {
			t.Fatalf("documents = %d, want 0", len(sent))
		}
	})

	t.Run("unknown document", func(t *testing.T) {
		f.handle(privateUpdate("/show example/project secrets"))
		if sent := f.sender.sentDocuments(); len(sent) != 0 {
			t.Fatalf("documents = %d, want 0", len(sent))
		}
		texts := f.sender.messageTexts()
		if len(texts) < 2 {
			t.Fatalf("messages = %d, want at least 2", len(texts))
		}
		last := texts[len(texts)-1]
		requireContains(t, last, "codemap")
		requireContains(t, last, "routing")
		requireContains(t, last, "observation")
	})

	t.Run("missing document", func(t *testing.T) {
		f.handle(privateUpdate("/show example/project routing"))
		if sent := f.sender.sentDocuments(); len(sent) != 0 {
			t.Fatalf("documents = %d, want 0", len(sent))
		}
	})
}

func TestShowRejectsSymlinkDoc(t *testing.T) {
	f := newBotFixture(t)
	id := installRepo(t, f, "example", "project", map[string]string{
		"codemap": "CODEMAP",
	}, time.Time{})

	secret := filepath.Join(t.TempDir(), "secret.txt")
	if err := os.WriteFile(secret, []byte("SECRET"), 0o600); err != nil {
		t.Fatalf("write secret: %v", err)
	}
	link := filepath.Join(f.cfg.WorkDir, id, "docs", "gist", "routing.md")
	if err := os.Symlink(secret, link); err != nil {
		t.Fatalf("create symlink: %v", err)
	}

	t.Run("named symlink doc", func(t *testing.T) {
		f.handle(privateUpdate("/show example/project routing"))
		if sent := f.sender.sentDocuments(); len(sent) != 0 {
			t.Fatalf("documents = %d, want 0", len(sent))
		}
		texts := f.sender.messageTexts()
		if len(texts) != 1 {
			t.Fatalf("messages = %d, want 1", len(texts))
		}
		requireContains(t, texts[0], "not available")
	})

	t.Run("all docs skips symlink", func(t *testing.T) {
		f.handle(privateUpdate("/show example/project"))
		sent := f.sender.sentDocuments()
		if len(sent) != 1 {
			t.Fatalf("documents = %d, want 1", len(sent))
		}
		if sent[0].filename != "codemap.md" {
			t.Errorf("filename = %q, want codemap.md", sent[0].filename)
		}
		for _, doc := range sent {
			requireNotContains(t, string(doc.data), "SECRET")
		}
	})
}

func TestLogCurrentJobTail40(t *testing.T) {
	f := newBotFixture(t)
	f.jobs.status = jobs.Status{Running: &jobs.JobView{
		ID:     "job-current",
		RepoID: "github__example__project",
	}}

	var log strings.Builder
	for i := 1; i <= 60; i++ {
		fmt.Fprintf(&log, "line-%02d\n", i)
	}
	writeJobLog(t, f, "job-current", log.String())

	f.handle(privateUpdate("/log"))

	texts := f.sender.messageTexts()
	if len(texts) != 1 {
		t.Fatalf("messages = %d, want 1", len(texts))
	}
	text := texts[0]
	if !strings.HasPrefix(text, "<pre>") || !strings.HasSuffix(text, "</pre>") {
		t.Errorf("log is not wrapped in <pre>: %q", text)
	}
	for _, want := range []string{"line-21", "line-60"} {
		requireContains(t, text, want)
	}
	for _, unwanted := range []string{"line-20", "line-01"} {
		requireNotContains(t, text, unwanted)
	}
	if got := utf16Count(text); got > maxMessageUTF16 {
		t.Errorf("log is %d UTF-16 units, want <= %d", got, maxMessageUTF16)
	}
}

func TestLogByRepoUsesLatestJob(t *testing.T) {
	f := newBotFixture(t)
	base := time.Date(2026, time.January, 1, 0, 0, 0, 0, time.UTC)

	// Insert the newest example/project job before the older one so selection
	// cannot rely on slice order.
	addJobRecord(t, f, "job-newest", "github__example__project", base.Add(2*time.Hour))
	addJobRecord(t, f, "job-older", "github__example__project", base)
	addJobRecord(t, f, "job-other", "github__other__repo", base.Add(10*time.Hour))
	writeJobLog(t, f, "job-newest", "newest-log\n")
	writeJobLog(t, f, "job-older", "older-log\n")
	writeJobLog(t, f, "job-other", "other-log\n")

	// A running job for another repository must not be used for an explicit
	// repository request.
	f.jobs.status = jobs.Status{Running: &jobs.JobView{
		ID:     "job-other",
		RepoID: "github__other__repo",
	}}

	f.handle(privateUpdate("/log example/project"))
	f.handle(privateUpdate("/log github__example__project"))
	f.handle(privateUpdate("/log"))

	texts := f.sender.messageTexts()
	if len(texts) != 3 {
		t.Fatalf("messages = %d, want 3", len(texts))
	}
	for i := 0; i < 2; i++ {
		requireContains(t, texts[i], "newest-log")
		requireNotContains(t, texts[i], "older-log")
		requireNotContains(t, texts[i], "other-log")
	}
	requireContains(t, texts[2], "other-log")
	requireNotContains(t, texts[2], "newest-log")
}

func TestLogIsEscapedAndUnder4096(t *testing.T) {
	f := newBotFixture(t)
	f.jobs.status = jobs.Status{Running: &jobs.JobView{
		ID:     "job-huge",
		RepoID: "github__example__project",
	}}

	var log strings.Builder
	for i := 0; i < 60; i++ {
		fmt.Fprintf(&log, "%03d %s\n", i, strings.Repeat("x", 400))
	}
	log.WriteString(`<script>alert("x")</script> & ` + "\n")
	log.WriteString("LAST-LINE\n")
	writeJobLog(t, f, "job-huge", log.String())

	f.handle(privateUpdate("/log"))

	texts := f.sender.messageTexts()
	if len(texts) != 1 {
		t.Fatalf("messages = %d, want 1", len(texts))
	}
	text := texts[0]

	if got := utf16Count(text); got > maxMessageUTF16 {
		t.Errorf("log is %d UTF-16 units, want <= %d", got, maxMessageUTF16)
	}
	requireNotContains(t, text, "<script>")
	requireContains(t, text, "&lt;script&gt;")
	requireContains(t, text, "&amp;")
	// Truncation drops the oldest lines, not the newest output.
	requireContains(t, text, "LAST-LINE")
}
