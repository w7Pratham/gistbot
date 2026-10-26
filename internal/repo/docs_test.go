package repo

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"
)

func writeRepoFile(t *testing.T, path, contents string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("create directory for %s: %v", path, err)
	}
	if err := os.WriteFile(path, []byte(contents), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func requireSymlink(t *testing.T, target, link string) {
	t.Helper()
	if err := os.Symlink(target, link); err != nil {
		t.Fatalf("symlink %s -> %s: %v", link, target, err)
	}
}

func TestDiscoverDocsPrefersDocsGistPerFile(t *testing.T) {
	repoDir := t.TempDir()
	writeRepoFile(t, filepath.Join(repoDir, "docs", "codemap.md"), "docs codemap")
	writeRepoFile(t, filepath.Join(repoDir, "docs", "routing.md"), "docs routing")
	writeRepoFile(t, filepath.Join(repoDir, "docs", "observation.md"), "docs observation")
	writeRepoFile(t, filepath.Join(repoDir, "docs", "gist", "codemap.md"), "gist codemap")
	writeRepoFile(t, filepath.Join(repoDir, "docs", "gist", "routing.md"), "gist routing")

	docs, err := DiscoverDocs(repoDir)
	if err != nil {
		t.Fatalf("DiscoverDocs returned error: %v", err)
	}

	want := map[string]string{
		"codemap":     filepath.Join(repoDir, "docs", "gist", "codemap.md"),
		"routing":     filepath.Join(repoDir, "docs", "gist", "routing.md"),
		"observation": filepath.Join(repoDir, "docs", "observation.md"),
	}
	if !reflect.DeepEqual(docs.Files, want) {
		t.Errorf("Files = %#v, want %#v", docs.Files, want)
	}
	if len(docs.Missing) != 0 {
		t.Errorf("Missing = %v, want none", docs.Missing)
	}

	wantContents := map[string]string{
		"codemap":     "gist codemap",
		"routing":     "gist routing",
		"observation": "docs observation",
	}
	for name, path := range want {
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read discovered %s: %v", name, err)
		}
		if got, wantBody := string(raw), wantContents[name]; got != wantBody {
			t.Errorf("%s content = %q, want %q", name, got, wantBody)
		}
	}
}

func TestDiscoverDocsIgnoresSymlinks(t *testing.T) {
	t.Run("symlinked files are skipped with per-file fallback", func(t *testing.T) {
		repoDir := t.TempDir()
		outside := t.TempDir()
		secret := filepath.Join(outside, "secret.md")
		writeRepoFile(t, secret, "outside secret")

		writeRepoFile(t, filepath.Join(repoDir, "docs", "codemap.md"), "docs codemap")
		if err := os.MkdirAll(filepath.Join(repoDir, "docs", "gist"), 0o755); err != nil {
			t.Fatal(err)
		}
		requireSymlink(t, secret, filepath.Join(repoDir, "docs", "gist", "codemap.md"))
		requireSymlink(t, secret, filepath.Join(repoDir, "docs", "routing.md"))

		docs, err := DiscoverDocs(repoDir)
		if err != nil {
			t.Fatalf("DiscoverDocs returned error: %v", err)
		}

		want := map[string]string{
			"codemap": filepath.Join(repoDir, "docs", "codemap.md"),
		}
		if !reflect.DeepEqual(docs.Files, want) {
			t.Errorf("Files = %#v, want %#v", docs.Files, want)
		}
		if wantMissing := []string{"routing", "observation"}; !reflect.DeepEqual(docs.Missing, wantMissing) {
			t.Errorf("Missing = %v, want %v", docs.Missing, wantMissing)
		}
	})

	t.Run("symlinked gist directory is not followed", func(t *testing.T) {
		repoDir := t.TempDir()
		outside := t.TempDir()
		writeRepoFile(t, filepath.Join(outside, "codemap.md"), "outside codemap")
		writeRepoFile(t, filepath.Join(outside, "routing.md"), "outside routing")
		writeRepoFile(t, filepath.Join(outside, "observation.md"), "outside observation")

		if err := os.MkdirAll(filepath.Join(repoDir, "docs"), 0o755); err != nil {
			t.Fatal(err)
		}
		requireSymlink(t, outside, filepath.Join(repoDir, "docs", "gist"))

		docs, err := DiscoverDocs(repoDir)
		if err != nil {
			t.Fatalf("DiscoverDocs returned error: %v", err)
		}
		if len(docs.Files) != 0 {
			t.Errorf("Files = %#v, want none", docs.Files)
		}
		wantMissing := []string{"codemap", "routing", "observation"}
		if !reflect.DeepEqual(docs.Missing, wantMissing) {
			t.Errorf("Missing = %v, want %v", docs.Missing, wantMissing)
		}
	})

	t.Run("symlinked docs directory is not followed", func(t *testing.T) {
		repoDir := t.TempDir()
		outside := t.TempDir()
		writeRepoFile(t, filepath.Join(outside, "codemap.md"), "outside codemap")
		writeRepoFile(t, filepath.Join(outside, "gist", "routing.md"), "outside routing")

		requireSymlink(t, outside, filepath.Join(repoDir, "docs"))

		docs, err := DiscoverDocs(repoDir)
		if err != nil {
			t.Fatalf("DiscoverDocs returned error: %v", err)
		}
		if len(docs.Files) != 0 {
			t.Errorf("Files = %#v, want none", docs.Files)
		}
		wantMissing := []string{"codemap", "routing", "observation"}
		if !reflect.DeepEqual(docs.Missing, wantMissing) {
			t.Errorf("Missing = %v, want %v", docs.Missing, wantMissing)
		}
	})

	t.Run("directories named like docs are not files", func(t *testing.T) {
		repoDir := t.TempDir()
		if err := os.MkdirAll(filepath.Join(repoDir, "docs", "routing.md"), 0o755); err != nil {
			t.Fatal(err)
		}

		docs, err := DiscoverDocs(repoDir)
		if err != nil {
			t.Fatalf("DiscoverDocs returned error: %v", err)
		}
		if len(docs.Files) != 0 {
			t.Errorf("Files = %#v, want none", docs.Files)
		}
		wantMissing := []string{"codemap", "routing", "observation"}
		if !reflect.DeepEqual(docs.Missing, wantMissing) {
			t.Errorf("Missing = %v, want %v", docs.Missing, wantMissing)
		}
	})
}

func TestDiscoverDocsReportsMissing(t *testing.T) {
	repoDir := t.TempDir()
	path := filepath.Join(repoDir, "docs", "codemap.md")
	writeRepoFile(t, path, "codemap")

	docs, err := DiscoverDocs(repoDir)
	if err != nil {
		t.Fatalf("DiscoverDocs returned error: %v", err)
	}
	want := map[string]string{"codemap": path}
	if !reflect.DeepEqual(docs.Files, want) {
		t.Errorf("Files = %#v, want %#v", docs.Files, want)
	}
	if wantMissing := []string{"routing", "observation"}; !reflect.DeepEqual(docs.Missing, wantMissing) {
		t.Errorf("Missing = %v, want %v", docs.Missing, wantMissing)
	}

	empty := t.TempDir()
	docs, err = DiscoverDocs(empty)
	if err != nil {
		t.Fatalf("DiscoverDocs on empty repo returned error: %v", err)
	}
	if len(docs.Files) != 0 {
		t.Errorf("Files = %#v, want none", docs.Files)
	}
	if wantMissing := []string{"codemap", "routing", "observation"}; !reflect.DeepEqual(docs.Missing, wantMissing) {
		t.Errorf("Missing = %v, want %v", docs.Missing, wantMissing)
	}
}

func TestDiscoverDocsRejectsInvalidRepoDir(t *testing.T) {
	for _, repoDir := range []string{"", "relative/repo"} {
		if _, err := DiscoverDocs(repoDir); err == nil {
			t.Errorf("DiscoverDocs(%q) returned no error", repoDir)
		}
	}

	missing := filepath.Join(t.TempDir(), "missing")
	if _, err := DiscoverDocs(missing); err == nil {
		t.Errorf("DiscoverDocs(%q) returned no error", missing)
	}
}

func TestListSkipsHiddenAndSymlinks(t *testing.T) {
	workDir := t.TempDir()

	alpha := filepath.Join(workDir, "github__owner__alpha")
	writeRepoFile(t, filepath.Join(alpha, "docs", "codemap.md"), "alpha")

	writeRepoFile(t, filepath.Join(workDir, ".github__owner__hidden", "docs", "codemap.md"), "hidden")
	writeRepoFile(t, filepath.Join(workDir, ".cache", "docs", "codemap.md"), "cache")
	writeRepoFile(t, filepath.Join(workDir, "not-a-repo", "docs", "codemap.md"), "invalid id")

	empty := filepath.Join(workDir, "github__owner__empty")
	if err := os.MkdirAll(empty, 0o755); err != nil {
		t.Fatal(err)
	}

	outside := t.TempDir()
	writeRepoFile(t, filepath.Join(outside, "docs", "codemap.md"), "outside")
	requireSymlink(t, outside, filepath.Join(workDir, "github__owner__outside"))
	requireSymlink(t, alpha, filepath.Join(workDir, "github__owner__link"))

	writeRepoFile(t, filepath.Join(workDir, "github__owner__file"), "not a directory")

	entries, err := List(workDir)
	if err != nil {
		t.Fatalf("List returned error: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("List returned %d entries, want 1: %#v", len(entries), entries)
	}
	got := entries[0]
	if got.ID != "github__owner__alpha" {
		t.Errorf("ID = %q, want github__owner__alpha", got.ID)
	}
	if got.Display != "owner/alpha" {
		t.Errorf("Display = %q, want owner/alpha", got.Display)
	}
	if want := []string{"codemap"}; !reflect.DeepEqual(got.Docs, want) {
		t.Errorf("Docs = %v, want %v", got.Docs, want)
	}
}

func TestListSortsNewestFirst(t *testing.T) {
	workDir := t.TempDir()
	base := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)

	alphaMap := filepath.Join(workDir, "github__owner__alpha", "docs", "codemap.md")
	writeRepoFile(t, alphaMap, "alpha codemap")

	betaMap := filepath.Join(workDir, "github__owner__beta", "docs", "codemap.md")
	betaRouting := filepath.Join(workDir, "github__owner__beta", "docs", "routing.md")
	writeRepoFile(t, betaMap, "beta codemap")
	writeRepoFile(t, betaRouting, "beta routing")

	gammaObservation := filepath.Join(workDir, "github__owner__gamma", "docs", "gist", "observation.md")
	writeRepoFile(t, gammaObservation, "gamma observation")

	times := map[string]time.Time{
		alphaMap:         base.Add(-3 * time.Hour),
		betaMap:          base.Add(-4 * time.Hour),
		betaRouting:      base.Add(-1 * time.Hour),
		gammaObservation: base,
	}
	for path, stamp := range times {
		if err := os.Chtimes(path, stamp, stamp); err != nil {
			t.Fatalf("set mtime on %s: %v", path, err)
		}
	}

	entries, err := List(workDir)
	if err != nil {
		t.Fatalf("List returned error: %v", err)
	}
	if len(entries) != 3 {
		t.Fatalf("List returned %d entries, want 3: %#v", len(entries), entries)
	}

	wantIDs := []string{
		"github__owner__gamma",
		"github__owner__beta",
		"github__owner__alpha",
	}
	wantDocs := [][]string{
		{"observation"},
		{"codemap", "routing"},
		{"codemap"},
	}
	wantModTimes := []time.Time{
		base,
		base.Add(-1 * time.Hour),
		base.Add(-3 * time.Hour),
	}
	wantDisplays := []string{"owner/gamma", "owner/beta", "owner/alpha"}

	for i, entry := range entries {
		if entry.ID != wantIDs[i] {
			t.Errorf("entry %d ID = %q, want %q", i, entry.ID, wantIDs[i])
		}
		if entry.Display != wantDisplays[i] {
			t.Errorf("entry %d Display = %q, want %q", i, entry.Display, wantDisplays[i])
		}
		if !reflect.DeepEqual(entry.Docs, wantDocs[i]) {
			t.Errorf("entry %d Docs = %v, want %v", i, entry.Docs, wantDocs[i])
		}
		if !entry.ModTime.Equal(wantModTimes[i]) {
			t.Errorf("entry %d ModTime = %v, want %v", i, entry.ModTime, wantModTimes[i])
		}
	}
}
