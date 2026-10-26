package repo

import (
	"bytes"
	"context"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

func fakeGitPath(t *testing.T) string {
	t.Helper()
	path, err := filepath.Abs(filepath.Join("testdata", "fake-git.sh"))
	if err != nil {
		t.Fatalf("resolve fake git path: %v", err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat fake git: %v", err)
	}
	if info.Mode().Perm()&0o111 == 0 {
		t.Fatalf("fake git fixture is not executable")
	}
	return path
}

func mustTarget(t *testing.T, raw string) Target {
	t.Helper()
	target, err := ParseGitHubURL(raw)
	if err != nil {
		t.Fatalf("parse target %q: %v", raw, err)
	}
	return target
}

func requireNotExist(t *testing.T, path string) {
	t.Helper()
	if _, err := os.Lstat(path); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("%s still exists (err=%v)", path, err)
	}
}

func readLines(t *testing.T, path string) []string {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return strings.Split(strings.TrimSuffix(string(raw), "\n"), "\n")
}

func parseEnvFile(t *testing.T, path string) map[string]string {
	t.Helper()
	env := make(map[string]string)
	for _, line := range readLines(t, path) {
		key, value, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		env[key] = value
	}
	return env
}

func processGone(t *testing.T, pid int) bool {
	t.Helper()
	err := syscall.Kill(pid, 0)
	if errors.Is(err, syscall.ESRCH) {
		return true
	}
	// A zombie still answers kill(pid, 0) until it is reaped.
	raw, readErr := os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), "stat"))
	if readErr == nil {
		fields := strings.Fields(string(raw))
		if len(fields) > 2 && fields[2] == "Z" {
			return true
		}
	}
	return false
}

func TestPrepareClonesIntoJobSandbox(t *testing.T) {
	workDir := t.TempDir()
	argsFile := filepath.Join(t.TempDir(), "args")
	t.Setenv("FAKE_GIT_ARGS", argsFile)

	target := mustTarget(t, "https://github.com/Owner/Repo")
	cloner := NewCloner(workDir, fakeGitPath(t), 5*time.Second)

	var log bytes.Buffer
	prepared, err := cloner.Prepare(context.Background(), target, "job-1", &log)
	if err != nil {
		t.Fatalf("Prepare returned error: %v", err)
	}

	wantJobDir := filepath.Join(workDir, ".work", "job-1")
	wantRepoDir := filepath.Join(wantJobDir, "repo")
	if prepared.JobDir != wantJobDir {
		t.Errorf("JobDir = %q, want %q", prepared.JobDir, wantJobDir)
	}
	if prepared.RepoDir != wantRepoDir {
		t.Errorf("RepoDir = %q, want %q", prepared.RepoDir, wantRepoDir)
	}

	raw, err := os.ReadFile(filepath.Join(prepared.RepoDir, "README.md"))
	if err != nil {
		t.Fatalf("read cloned README: %v", err)
	}
	if got, want := string(raw), "readme\n"; got != want {
		t.Errorf("README = %q, want %q", got, want)
	}

	info, err := os.Stat(prepared.JobDir)
	if err != nil {
		t.Fatalf("stat job dir: %v", err)
	}
	if got, want := info.Mode().Perm(), fs.FileMode(0o700); got != want {
		t.Errorf("job dir mode = %o, want %o", got, want)
	}

	wantArgs := []string{"clone", "--depth", "50", "--", target.URL, prepared.RepoDir}
	if got := readLines(t, argsFile); !reflect.DeepEqual(got, wantArgs) {
		t.Errorf("git args = %v, want %v", got, wantArgs)
	}
}

func TestPrepareRemovesSymlinks(t *testing.T) {
	workDir := t.TempDir()
	outside := t.TempDir()
	secret := filepath.Join(outside, "secret.txt")
	writeRepoFile(t, secret, "outside secret")

	t.Setenv("FAKE_GIT_LINK", secret)
	t.Setenv("FAKE_GIT_LINK_DIR", outside)

	cloner := NewCloner(workDir, fakeGitPath(t), 5*time.Second)
	var log bytes.Buffer
	prepared, err := cloner.Prepare(context.Background(), mustTarget(t, "https://github.com/owner/repo"), "job-links", &log)
	if err != nil {
		t.Fatalf("Prepare returned error: %v", err)
	}

	requireNotExist(t, filepath.Join(prepared.RepoDir, "link"))
	requireNotExist(t, filepath.Join(prepared.RepoDir, "linkdir"))

	raw, err := os.ReadFile(secret)
	if err != nil {
		t.Fatalf("outside secret was removed: %v", err)
	}
	if got, want := string(raw), "outside secret"; got != want {
		t.Errorf("outside secret = %q, want %q", got, want)
	}

	if got := log.String(); !strings.Contains(got, "removed 2 symlinks") {
		t.Errorf("job log = %q, want symlink count", got)
	}
}

func TestPrepareFiltersSecretEnvironment(t *testing.T) {
	workDir := t.TempDir()
	envFile := filepath.Join(t.TempDir(), "env")
	t.Setenv("FAKE_GIT_ENV", envFile)

	secrets := map[string]string{
		"TELEGRAM_BOT_TOKEN":       "telegram-secret-value",
		"TELEGRAM_API_URL":         "https://telegram.invalid/secret",
		"OWNER_USER_ID":            "424242",
		"OPENCODE_PASSWORD":        "opencode-secret-value",
		"OPENCODE_SERVER_PASSWORD": "opencode-server-secret-value",
		"OPENCODE_API_PASSWORD":    "opencode-api-secret-value",
	}
	for key, value := range secrets {
		t.Setenv(key, value)
	}

	var log bytes.Buffer
	cloner := NewCloner(workDir, fakeGitPath(t), 5*time.Second)
	prepared, err := cloner.Prepare(context.Background(), mustTarget(t, "https://github.com/owner/repo"), "job-env", &log)
	if err != nil {
		t.Fatalf("Prepare returned error: %v", err)
	}
	if prepared.RepoDir == "" {
		t.Fatal("Prepare returned an empty RepoDir")
	}

	env := parseEnvFile(t, envFile)
	for key := range secrets {
		if _, ok := env[key]; ok {
			t.Errorf("git environment still contains %s", key)
		}
	}
	if got := env["GIT_TERMINAL_PROMPT"]; got != "0" {
		t.Errorf("GIT_TERMINAL_PROMPT = %q, want 0", got)
	}
	for _, secret := range secrets {
		if strings.Contains(log.String(), secret) {
			t.Errorf("job log contains a secret value")
		}
	}
}

func TestPrepareTimesOutAndLeavesNoJobDir(t *testing.T) {
	workDir := t.TempDir()
	childFile := filepath.Join(t.TempDir(), "child.pid")
	t.Setenv("FAKE_GIT_MODE", "sleep")
	t.Setenv("FAKE_GIT_CHILD", childFile)

	cloner := NewCloner(workDir, fakeGitPath(t), 250*time.Millisecond)
	started := time.Now()
	_, err := cloner.Prepare(context.Background(), mustTarget(t, "https://github.com/owner/repo"), "job-timeout", io.Discard)
	if err == nil {
		t.Fatal("Prepare returned no error")
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("Prepare error = %v, want deadline exceeded", err)
	}
	if elapsed := time.Since(started); elapsed > 5*time.Second {
		t.Errorf("Prepare took %v, want prompt cancellation", elapsed)
	}

	requireNotExist(t, filepath.Join(workDir, ".work", "job-timeout"))

	raw, err := os.ReadFile(childFile)
	if err != nil {
		t.Fatalf("fake git child pid was not recorded: %v", err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(raw)))
	if err != nil {
		t.Fatalf("parse child pid: %v", err)
	}

	deadline := time.Now().Add(5 * time.Second)
	for {
		if processGone(t, pid) {
			break
		}
		if time.Now().After(deadline) {
			_ = syscall.Kill(pid, syscall.SIGKILL)
			t.Fatalf("git child process %d survived cancellation", pid)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestPrepareRejectsInvalidInput(t *testing.T) {
	workDir := t.TempDir()
	git := fakeGitPath(t)
	target := mustTarget(t, "https://github.com/owner/repo")

	cases := []struct {
		name   string
		target Target
		jobID  string
	}{
		{
			name:   "non github target",
			target: Target{URL: "https://gitlab.com/owner/repo", ID: target.ID},
			jobID:  "job-1",
		},
		{
			name:   "target id mismatch",
			target: Target{URL: target.URL, ID: "github__other__repo"},
			jobID:  "job-1",
		},
		{name: "empty job id", target: target, jobID: ""},
		{name: "traversal job id", target: target, jobID: "../escape"},
		{name: "separator job id", target: target, jobID: "job/1"},
		{name: "hidden job id", target: target, jobID: ".hidden"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cloner := NewCloner(workDir, git, time.Second)
			_, err := cloner.Prepare(context.Background(), tc.target, tc.jobID, io.Discard)
			if err == nil {
				t.Fatalf("Prepare returned no error")
			}
		})
	}

	cloner := NewCloner("relative/path", git, time.Second)
	if _, err := cloner.Prepare(context.Background(), target, "job-1", io.Discard); err == nil {
		t.Error("Prepare with a relative work dir returned no error")
	}
}

func TestInstallReplacesOldRepo(t *testing.T) {
	workDir := t.TempDir()
	target := mustTarget(t, "https://github.com/owner/repo")
	finalDir := filepath.Join(workDir, target.ID)
	writeRepoFile(t, filepath.Join(finalDir, "docs", "codemap.md"), "old codemap")
	writeRepoFile(t, filepath.Join(finalDir, "old.txt"), "old")

	repoDir := filepath.Join(workDir, ".work", "job-1", "repo")
	writeRepoFile(t, filepath.Join(repoDir, "new.txt"), "new")

	cloner := NewCloner(workDir, fakeGitPath(t), time.Second)
	if err := cloner.Install(target, repoDir); err != nil {
		t.Fatalf("Install returned error: %v", err)
	}

	raw, err := os.ReadFile(filepath.Join(finalDir, "new.txt"))
	if err != nil {
		t.Fatalf("read installed repo: %v", err)
	}
	if got, want := string(raw), "new"; got != want {
		t.Errorf("new.txt = %q, want %q", got, want)
	}
	requireNotExist(t, filepath.Join(finalDir, "old.txt"))
	requireNotExist(t, repoDir)

	children, err := os.ReadDir(filepath.Join(workDir, ".trash"))
	if err == nil && len(children) != 0 {
		t.Errorf("trash still has %d entries, want none", len(children))
	} else if err != nil && !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("read trash: %v", err)
	}
}

func TestInstallRestoresOldOnRenameFailure(t *testing.T) {
	workDir := t.TempDir()
	target := mustTarget(t, "https://github.com/owner/repo")
	finalDir := filepath.Join(workDir, target.ID)
	writeRepoFile(t, filepath.Join(finalDir, "old.txt"), "old")

	repoDir := filepath.Join(workDir, ".work", "job-1", "repo")
	writeRepoFile(t, filepath.Join(repoDir, "new.txt"), "new")

	cloner := NewCloner(workDir, fakeGitPath(t), time.Second)
	calls := 0
	cloner.renameFn = func(oldPath, newPath string) error {
		calls++
		if calls == 2 {
			return errors.New("injected rename failure")
		}
		return os.Rename(oldPath, newPath)
	}

	if err := cloner.Install(target, repoDir); err == nil {
		t.Fatal("Install returned no error")
	}

	raw, err := os.ReadFile(filepath.Join(finalDir, "old.txt"))
	if err != nil {
		t.Fatalf("old repo was not restored: %v", err)
	}
	if got, want := string(raw), "old"; got != want {
		t.Errorf("old.txt = %q, want %q", got, want)
	}
	requireNotExist(t, filepath.Join(finalDir, "new.txt"))
	if _, err := os.Stat(filepath.Join(repoDir, "new.txt")); err != nil {
		t.Errorf("staged repo was not preserved: %v", err)
	}

	children, err := os.ReadDir(filepath.Join(workDir, ".trash"))
	if err == nil && len(children) != 0 {
		t.Errorf("trash still has %d entries after restore, want none", len(children))
	}
}

func TestInstallRejectsSymlinkFinal(t *testing.T) {
	workDir := t.TempDir()
	target := mustTarget(t, "https://github.com/owner/repo")

	outside := t.TempDir()
	writeRepoFile(t, filepath.Join(outside, "marker.txt"), "outside")
	finalDir := filepath.Join(workDir, target.ID)
	requireSymlink(t, outside, finalDir)

	repoDir := filepath.Join(workDir, ".work", "job-1", "repo")
	writeRepoFile(t, filepath.Join(repoDir, "new.txt"), "new")

	cloner := NewCloner(workDir, fakeGitPath(t), time.Second)
	if err := cloner.Install(target, repoDir); err == nil {
		t.Fatal("Install accepted a symlinked final path")
	}

	info, err := os.Lstat(finalDir)
	if err != nil {
		t.Fatalf("lstat final: %v", err)
	}
	if info.Mode()&fs.ModeSymlink == 0 {
		t.Error("final symlink was replaced")
	}
	requireNotExist(t, filepath.Join(outside, "new.txt"))
	if _, err := os.Stat(filepath.Join(outside, "marker.txt")); err != nil {
		t.Errorf("outside directory was modified: %v", err)
	}
	if _, err := os.Stat(filepath.Join(repoDir, "new.txt")); err != nil {
		t.Errorf("staged repo was not preserved: %v", err)
	}
}

func TestInstallRejectsUntrustedSource(t *testing.T) {
	workDir := t.TempDir()
	target := mustTarget(t, "https://github.com/owner/repo")

	outside := t.TempDir()
	writeRepoFile(t, filepath.Join(outside, "new.txt"), "new")

	cloner := NewCloner(workDir, fakeGitPath(t), time.Second)
	if err := cloner.Install(target, outside); err == nil {
		t.Error("Install accepted a source outside .work")
	}
	if err := cloner.Install(target, filepath.Join(workDir, ".work")); err == nil {
		t.Error("Install accepted .work itself as a source")
	}
	if err := cloner.Install(target, "relative/repo"); err == nil {
		t.Error("Install accepted a relative source")
	}
	if _, err := os.Stat(filepath.Join(outside, "new.txt")); err != nil {
		t.Errorf("outside source was modified: %v", err)
	}
}

func TestAbortRemovesJobDirOnly(t *testing.T) {
	workDir := t.TempDir()
	cloner := NewCloner(workDir, fakeGitPath(t), time.Second)

	jobDir := filepath.Join(workDir, ".work", "job-1")
	writeRepoFile(t, filepath.Join(jobDir, "repo", "README.md"), "readme")
	if err := cloner.Abort(jobDir); err != nil {
		t.Fatalf("Abort returned error: %v", err)
	}
	requireNotExist(t, jobDir)

	outside := filepath.Join(t.TempDir(), "job-1")
	writeRepoFile(t, filepath.Join(outside, "keep.txt"), "keep")
	for _, path := range []string{outside, workDir, filepath.Join(workDir, ".work")} {
		if err := cloner.Abort(path); err == nil {
			t.Errorf("Abort(%q) returned no error", path)
		}
	}
	if _, err := os.Stat(filepath.Join(outside, "keep.txt")); err != nil {
		t.Errorf("Abort removed a directory outside .work: %v", err)
	}

	installed := filepath.Join(workDir, "github__owner__repo")
	writeRepoFile(t, filepath.Join(installed, "keep.txt"), "keep")
	if err := cloner.Abort(installed); err == nil {
		t.Error("Abort accepted an installed repository path")
	}
	if _, err := os.Stat(filepath.Join(installed, "keep.txt")); err != nil {
		t.Errorf("Abort removed an installed repository: %v", err)
	}

	if err := cloner.Abort(filepath.Join(workDir, ".work", "missing")); err != nil {
		t.Errorf("Abort of a missing job dir returned error: %v", err)
	}
}

func TestRecoverRestoresTrashWhenFinalMissing(t *testing.T) {
	workDir := t.TempDir()
	cloner := NewCloner(workDir, fakeGitPath(t), time.Second)

	staleJob := filepath.Join(workDir, ".work", "job-stale")
	writeRepoFile(t, filepath.Join(staleJob, "repo", "README.md"), "stale")

	trashDir := filepath.Join(workDir, ".trash")
	trashPath := filepath.Join(trashDir, "github__owner__repo-1700000000000000000")
	writeRepoFile(t, filepath.Join(trashPath, "docs", "codemap.md"), "old codemap")

	if err := cloner.Recover(); err != nil {
		t.Fatalf("Recover returned error: %v", err)
	}

	requireNotExist(t, staleJob)
	requireNotExist(t, trashPath)

	finalDir := filepath.Join(workDir, "github__owner__repo")
	raw, err := os.ReadFile(filepath.Join(finalDir, "docs", "codemap.md"))
	if err != nil {
		t.Fatalf("trash was not restored: %v", err)
	}
	if got, want := string(raw), "old codemap"; got != want {
		t.Errorf("restored codemap = %q, want %q", got, want)
	}
}

func TestRecoverRemovesTrashWhenFinalExists(t *testing.T) {
	workDir := t.TempDir()
	cloner := NewCloner(workDir, fakeGitPath(t), time.Second)

	finalDir := filepath.Join(workDir, "github__owner__repo")
	writeRepoFile(t, filepath.Join(finalDir, "current.txt"), "current")

	trashDir := filepath.Join(workDir, ".trash")
	trashPath := filepath.Join(trashDir, "github__owner__repo-1700000000000000000")
	writeRepoFile(t, filepath.Join(trashPath, "old.txt"), "old")

	if err := cloner.Recover(); err != nil {
		t.Fatalf("Recover returned error: %v", err)
	}

	requireNotExist(t, trashPath)
	raw, err := os.ReadFile(filepath.Join(finalDir, "current.txt"))
	if err != nil {
		t.Fatalf("final repo was modified: %v", err)
	}
	if got, want := string(raw), "current"; got != want {
		t.Errorf("current.txt = %q, want %q", got, want)
	}
}
