package repo

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

const (
	workSubdir     = ".work"
	trashSubdir    = ".trash"
	cloneDepth     = "50"
	terminateGrace = 10 * time.Second

	jobDirMode   = fs.FileMode(0o700)
	workDirMode  = fs.FileMode(0o700)
	trashDirMode = fs.FileMode(0o700)
)

// jobIDPattern keeps a job ID a single, non-hidden path component. The job
// manager generates IDs, but Prepare still validates them so a malformed ID can
// never escape WORK_DIR/.work.
var jobIDPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`)

// Prepared is a freshly cloned repository plus the sandbox that contains it.
type Prepared struct {
	JobDir  string // safe cwd for OpenCode; contains "repo"
	RepoDir string // "<JobDir>/repo"
}

// Cloner clones GitHub repositories into per-job sandboxes and installs them
// atomically under WorkDir. All git invocations use a fixed argv and a process
// group so cancellation cannot leave a clone running.
type Cloner struct {
	WorkDir string
	GitBin  string
	Timeout time.Duration

	// renameFn is a test seam for failures after the old repository has been
	// moved aside. It is nil in production, where os.Rename is used.
	renameFn func(oldPath, newPath string) error
}

// NewCloner returns a Cloner for workDir. An empty gitBin means "git".
func NewCloner(workDir, gitBin string, timeout time.Duration) *Cloner {
	if gitBin == "" {
		gitBin = "git"
	}
	return &Cloner{
		WorkDir: workDir,
		GitBin:  gitBin,
		Timeout: timeout,
	}
}

// Prepare clones t into WORK_DIR/.work/<jobID>/repo and removes every symlink
// from the result. The caller owns the returned sandbox and must eventually
// Install it or Abort it. On any failure Prepare removes the job directory.
func (c *Cloner) Prepare(ctx context.Context, t Target, jobID string, log io.Writer) (Prepared, error) {
	if ctx == nil {
		return Prepared{}, errors.New("repo: context is nil")
	}
	if err := validateCloneTarget(t); err != nil {
		return Prepared{}, err
	}
	if !jobIDPattern.MatchString(jobID) {
		return Prepared{}, errors.New("repo: job ID is not a safe path component")
	}

	_, workRoot, err := c.ensureRoots()
	if err != nil {
		return Prepared{}, err
	}

	jobDir := filepath.Join(workRoot, jobID)
	if err := os.Mkdir(jobDir, jobDirMode); err != nil {
		if errors.Is(err, fs.ErrExist) {
			return Prepared{}, errors.New("repo: job directory already exists")
		}
		return Prepared{}, fmt.Errorf("repo: create job directory: %w", err)
	}

	repoDir := filepath.Join(jobDir, "repo")
	cloneCtx := ctx
	if c.Timeout > 0 {
		var cancel context.CancelFunc
		cloneCtx, cancel = context.WithTimeout(ctx, c.Timeout)
		defer cancel()
	}
	err = c.clone(cloneCtx, t.URL, repoDir, log)
	if err == nil {
		err = removeSymlinks(repoDir, log)
	}
	if err != nil {
		if cleanupErr := os.RemoveAll(jobDir); cleanupErr != nil {
			return Prepared{}, fmt.Errorf("%w; also failed to remove job directory: %v", err, cleanupErr)
		}
		return Prepared{}, err
	}

	return Prepared{JobDir: jobDir, RepoDir: repoDir}, nil
}

// Install atomically replaces WORK_DIR/<id> with repoDir. repoDir must be a
// real directory inside WORK_DIR/.work. When an installed copy exists it is
// moved to WORK_DIR/.trash first and removed after the swap. If the swap fails
// the old copy is restored.
func (c *Cloner) Install(t Target, repoDir string) error {
	if !ValidID(t.ID) {
		return errors.New("repo: target ID is invalid")
	}
	workDir, workRoot, err := c.existingRoots()
	if err != nil {
		return err
	}
	finalDir := RepoDir(workDir, t.ID)
	if finalDir == "" {
		return errors.New("repo: target ID is invalid")
	}
	if err := validateStagedRepo(workRoot, repoDir); err != nil {
		return err
	}

	info, err := os.Lstat(finalDir)
	switch {
	case err == nil:
		if info.Mode()&fs.ModeSymlink != 0 || !info.IsDir() {
			return errors.New("repo: installed repository path is not a real directory")
		}
		return c.swap(finalDir, repoDir, filepath.Join(workDir, trashSubdir), t.ID)
	case errors.Is(err, fs.ErrNotExist):
		if err := c.rename(repoDir, finalDir); err != nil {
			return fmt.Errorf("repo: install repository: %w", err)
		}
		return nil
	default:
		return fmt.Errorf("repo: inspect installed repository: %w", err)
	}
}

// Abort removes a per-job sandbox. It only accepts a direct child of
// WORK_DIR/.work so it can never delete an installed repository or a path
// outside the sandbox root.
func (c *Cloner) Abort(jobDir string) error {
	_, workRoot, err := c.existingRoots()
	if err != nil {
		return err
	}
	if jobDir == "" || !filepath.IsAbs(jobDir) {
		return errors.New("repo: job directory must be an absolute path")
	}
	jobDir = filepath.Clean(jobDir)

	rel, err := filepath.Rel(workRoot, jobDir)
	if err != nil {
		return errors.New("repo: job directory is not inside the work sandbox")
	}
	if rel == "." || rel == ".." || filepath.IsAbs(rel) || strings.ContainsRune(rel, filepath.Separator) {
		return errors.New("repo: job directory is not a direct child of the work sandbox")
	}

	if err := os.RemoveAll(jobDir); err != nil {
		return fmt.Errorf("repo: remove job directory: %w", err)
	}
	return nil
}

// Recover repairs the workspace after an unclean shutdown. Every stale job
// directory under WORK_DIR/.work is removed. A trash entry is renamed back to
// its installed path when that path is missing; otherwise the stale trash entry
// is removed.
func (c *Cloner) Recover() error {
	workDir, err := c.workDirPath()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(workDir, workDirMode); err != nil {
		return fmt.Errorf("repo: create work directory: %w", err)
	}
	if err := requireRealDir(workDir, "work directory"); err != nil {
		return err
	}

	workRoot := filepath.Join(workDir, workSubdir)
	if err := ensureRealDir(workRoot, workDirMode); err != nil {
		return err
	}
	entries, err := os.ReadDir(workRoot)
	if err != nil {
		return fmt.Errorf("repo: read work sandbox: %w", err)
	}
	for _, entry := range entries {
		if err := os.RemoveAll(filepath.Join(workRoot, entry.Name())); err != nil {
			return fmt.Errorf("repo: remove stale job directory: %w", err)
		}
	}

	return c.recoverTrash(workDir)
}

func (c *Cloner) clone(ctx context.Context, cloneURL, repoDir string, log io.Writer) error {
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("repo: clone cancelled before start: %w", err)
	}

	cmd := exec.Command(c.gitBin(), "clone", "--depth", cloneDepth, "--", cloneURL, repoDir)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Env = cloneEnv(os.Environ())
	out := &syncWriter{w: logWriter(log)}
	cmd.Stdout = out
	cmd.Stderr = out

	if err := cmd.Start(); err != nil {
		return fmt.Errorf("repo: start git clone: %w", err)
	}

	done := make(chan error, 1)
	go func() {
		done <- cmd.Wait()
	}()

	select {
	case err := <-done:
		if err != nil {
			return fmt.Errorf("repo: git clone failed: %w", err)
		}
		return nil
	case <-ctx.Done():
		stopProcessGroup(cmd.Process.Pid, done)
		return fmt.Errorf("repo: git clone interrupted: %w", ctx.Err())
	}
}

// stopProcessGroup terminates git and every child it spawned. A process that
// ignores SIGTERM is killed after terminateGrace.
func stopProcessGroup(pid int, done <-chan error) {
	_ = syscall.Kill(-pid, syscall.SIGTERM)

	timer := time.NewTimer(terminateGrace)
	defer timer.Stop()
	select {
	case <-done:
	case <-timer.C:
		_ = syscall.Kill(-pid, syscall.SIGKILL)
		<-done
	}
}

// cloneEnv strips credentials and bot configuration from the environment that
// git inherits, then disables interactive credential prompts.
func cloneEnv(environ []string) []string {
	out := make([]string, 0, len(environ)+1)
	for _, entry := range environ {
		key, _, _ := strings.Cut(entry, "=")
		if strings.HasPrefix(key, "TELEGRAM_") {
			continue
		}
		switch key {
		case "OWNER_USER_ID",
			"OPENCODE_PASSWORD",
			"OPENCODE_SERVER_PASSWORD",
			"OPENCODE_API_PASSWORD",
			"GIT_TERMINAL_PROMPT":
			continue
		}
		out = append(out, entry)
	}
	return append(out, "GIT_TERMINAL_PROMPT=0")
}

// removeSymlinks deletes every symlink below repoDir without following any of
// them and writes the count to log.
func removeSymlinks(repoDir string, log io.Writer) error {
	info, err := os.Lstat(repoDir)
	if err != nil {
		return fmt.Errorf("repo: inspect fresh clone: %w", err)
	}
	if info.Mode()&fs.ModeSymlink != 0 || !info.IsDir() {
		return errors.New("repo: fresh clone is not a real directory")
	}

	removed := 0
	err = filepath.WalkDir(repoDir, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if info.Mode()&fs.ModeSymlink == 0 {
			return nil
		}
		if err := os.Remove(path); err != nil {
			return err
		}
		removed++
		return nil
	})
	if err != nil {
		return fmt.Errorf("repo: remove symlinks from fresh clone: %w", err)
	}

	fmt.Fprintf(logWriter(log), "removed %d symlinks\n", removed)
	return nil
}

// swap moves the installed copy to trash, installs repoDir, and then removes
// the trash. A failed install restores the previous copy.
func (c *Cloner) swap(finalDir, repoDir, trashDir, id string) error {
	if err := ensureRealDir(trashDir, trashDirMode); err != nil {
		return err
	}
	trashPath, err := uniqueTrashPath(trashDir, id)
	if err != nil {
		return err
	}

	if err := c.rename(finalDir, trashPath); err != nil {
		return fmt.Errorf("repo: move old repository aside: %w", err)
	}
	if err := c.rename(repoDir, finalDir); err != nil {
		if restoreErr := c.rename(trashPath, finalDir); restoreErr != nil {
			return fmt.Errorf("repo: install repository: %v; restore old repository: %w", err, restoreErr)
		}
		return fmt.Errorf("repo: install repository: %w", err)
	}

	// The swap is complete. A leftover trash entry is harmless because Recover
	// removes it when the final directory exists.
	_ = os.RemoveAll(trashPath)
	return nil
}

func (c *Cloner) recoverTrash(workDir string) error {
	trashDir := filepath.Join(workDir, trashSubdir)
	info, err := os.Lstat(trashDir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("repo: inspect trash directory: %w", err)
	}
	if info.Mode()&fs.ModeSymlink != 0 || !info.IsDir() {
		return errors.New("repo: trash path is not a real directory")
	}

	entries, err := os.ReadDir(trashDir)
	if err != nil {
		return fmt.Errorf("repo: read trash directory: %w", err)
	}
	type trashed struct {
		id    string
		stamp int64
		name  string
	}
	items := make([]trashed, 0, len(entries))
	for _, entry := range entries {
		id, stamp, ok := splitTrashName(entry.Name())
		if !ok {
			continue
		}
		items = append(items, trashed{id: id, stamp: stamp, name: entry.Name()})
	}
	sort.Slice(items, func(i, j int) bool {
		if items[i].id != items[j].id {
			return items[i].id < items[j].id
		}
		return items[i].stamp < items[j].stamp
	})

	for i := 0; i < len(items); {
		j := i
		for j < len(items) && items[j].id == items[i].id {
			j++
		}
		id := items[i].id
		finalDir := RepoDir(workDir, id)

		_, statErr := os.Lstat(finalDir)
		finalExists := statErr == nil
		if statErr != nil && !errors.Is(statErr, fs.ErrNotExist) {
			return fmt.Errorf("repo: inspect installed repository: %w", statErr)
		}

		if !finalExists {
			newest := items[j-1]
			path := filepath.Join(trashDir, newest.name)
			info, err := os.Lstat(path)
			if err != nil {
				return fmt.Errorf("repo: inspect trash entry: %w", err)
			}
			if info.Mode()&fs.ModeSymlink != 0 || !info.IsDir() {
				return errors.New("repo: trash entry is not a real directory")
			}
			if err := c.rename(path, finalDir); err != nil {
				return fmt.Errorf("repo: restore repository from trash: %w", err)
			}
			for k := i; k < j-1; k++ {
				if err := os.RemoveAll(filepath.Join(trashDir, items[k].name)); err != nil {
					return fmt.Errorf("repo: remove stale trash entry: %w", err)
				}
			}
		} else {
			for k := i; k < j; k++ {
				if err := os.RemoveAll(filepath.Join(trashDir, items[k].name)); err != nil {
					return fmt.Errorf("repo: remove stale trash entry: %w", err)
				}
			}
		}
		i = j
	}
	return nil
}

func uniqueTrashPath(trashDir, id string) (string, error) {
	stamp := time.Now().UnixNano()
	for {
		path := filepath.Join(trashDir, fmt.Sprintf("%s-%d", id, stamp))
		_, err := os.Lstat(path)
		if errors.Is(err, fs.ErrNotExist) {
			return path, nil
		}
		if err != nil {
			return "", fmt.Errorf("repo: inspect trash path: %w", err)
		}
		stamp++
	}
}

func splitTrashName(name string) (id string, stamp int64, ok bool) {
	i := strings.LastIndexByte(name, '-')
	if i <= 0 || i == len(name)-1 {
		return "", 0, false
	}
	id = name[:i]
	if !ValidID(id) {
		return "", 0, false
	}
	for _, r := range name[i+1:] {
		if r < '0' || r > '9' {
			return "", 0, false
		}
	}
	stamp, err := strconv.ParseInt(name[i+1:], 10, 64)
	if err != nil {
		return "", 0, false
	}
	return id, stamp, true
}

func validateCloneTarget(t Target) error {
	parsed, err := ParseGitHubURL(t.URL)
	if err != nil {
		return fmt.Errorf("repo: target URL is not a valid GitHub repository: %w", err)
	}
	if !ValidID(t.ID) || parsed.ID != t.ID {
		return errors.New("repo: target ID does not match its URL")
	}
	return nil
}

// validateStagedRepo verifies that repoDir is a real directory below workRoot
// with no symlinked path component.
func validateStagedRepo(workRoot, repoDir string) error {
	if repoDir == "" || !filepath.IsAbs(repoDir) {
		return errors.New("repo: staged repository must be an absolute path")
	}
	repoDir = filepath.Clean(repoDir)

	rel, err := filepath.Rel(workRoot, repoDir)
	if err != nil {
		return errors.New("repo: staged repository is not inside the work sandbox")
	}
	if rel == "." || rel == ".." || filepath.IsAbs(rel) || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return errors.New("repo: staged repository is not inside the work sandbox")
	}

	current := workRoot
	for _, part := range strings.Split(rel, string(filepath.Separator)) {
		if part == "" || part == "." || part == ".." {
			return errors.New("repo: staged repository path is not canonical")
		}
		current = filepath.Join(current, part)
		info, err := os.Lstat(current)
		if err != nil {
			return fmt.Errorf("repo: inspect staged repository: %w", err)
		}
		if info.Mode()&fs.ModeSymlink != 0 {
			return errors.New("repo: staged repository path must not contain symlinks")
		}
		if current != repoDir && !info.IsDir() {
			return errors.New("repo: staged repository path is not a directory")
		}
	}
	return nil
}

func (c *Cloner) workDirPath() (string, error) {
	if c == nil {
		return "", errors.New("repo: cloner is nil")
	}
	if c.WorkDir == "" || !filepath.IsAbs(c.WorkDir) {
		return "", errors.New("repo: work directory must be an absolute path")
	}
	workDir := filepath.Clean(c.WorkDir)
	if workDir == string(filepath.Separator) {
		return "", errors.New("repo: work directory must not be the filesystem root")
	}
	return workDir, nil
}

// ensureRoots creates WORK_DIR and WORK_DIR/.work when needed.
func (c *Cloner) ensureRoots() (string, string, error) {
	workDir, err := c.workDirPath()
	if err != nil {
		return "", "", err
	}
	if err := os.MkdirAll(workDir, workDirMode); err != nil {
		return "", "", fmt.Errorf("repo: create work directory: %w", err)
	}
	if err := requireRealDir(workDir, "work directory"); err != nil {
		return "", "", err
	}
	workRoot := filepath.Join(workDir, workSubdir)
	if err := ensureRealDir(workRoot, workDirMode); err != nil {
		return "", "", err
	}
	return workDir, workRoot, nil
}

// existingRoots requires WORK_DIR and WORK_DIR/.work to already exist.
func (c *Cloner) existingRoots() (string, string, error) {
	workDir, err := c.workDirPath()
	if err != nil {
		return "", "", err
	}
	if err := requireRealDir(workDir, "work directory"); err != nil {
		return "", "", err
	}
	workRoot := filepath.Join(workDir, workSubdir)
	if err := requireRealDir(workRoot, "work sandbox directory"); err != nil {
		return "", "", err
	}
	return workDir, workRoot, nil
}

func ensureRealDir(path string, mode fs.FileMode) error {
	err := os.Mkdir(path, mode)
	if err == nil {
		return nil
	}
	if !errors.Is(err, fs.ErrExist) {
		return fmt.Errorf("repo: create directory: %w", err)
	}
	return requireRealDir(path, "directory")
}

func requireRealDir(path, what string) error {
	info, err := os.Lstat(path)
	if err != nil {
		return fmt.Errorf("repo: inspect %s: %w", what, err)
	}
	if info.Mode()&fs.ModeSymlink != 0 || !info.IsDir() {
		return fmt.Errorf("repo: %s is not a real directory", what)
	}
	return nil
}

func (c *Cloner) gitBin() string {
	if c.GitBin == "" {
		return "git"
	}
	return c.GitBin
}

func (c *Cloner) rename(oldPath, newPath string) error {
	if c.renameFn != nil {
		return c.renameFn(oldPath, newPath)
	}
	return os.Rename(oldPath, newPath)
}

func logWriter(log io.Writer) io.Writer {
	if log == nil {
		return io.Discard
	}
	return log
}

// syncWriter serializes stdout and stderr copying from the git process.
type syncWriter struct {
	mu sync.Mutex
	w  io.Writer
}

func (s *syncWriter) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.w.Write(p)
}
