package repo

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// docNames is the canonical order of gist documents.
var docNames = []string{"codemap", "routing", "observation"}

// Docs is the result of scanning a repository for gist documentation.
type Docs struct {
	Files   map[string]string // "codemap" -> absolute path
	Missing []string
}

// DiscoverDocs finds codemap, routing, and observation under repoDir. Each
// document is looked up in docs/gist/<name>.md first and then
// docs/<name>.md, so a repository that was written by the gist skill can mix
// migrated files with legacy ones. Only regular, non-symlink files are
// returned; a symlinked docs or docs/gist directory is never followed.
func DiscoverDocs(repoDir string) (Docs, error) {
	if repoDir == "" || !filepath.IsAbs(repoDir) {
		return Docs{}, errors.New("repo: repository directory must be an absolute path")
	}
	repoDir = filepath.Clean(repoDir)

	info, err := os.Lstat(repoDir)
	if err != nil {
		return Docs{}, fmt.Errorf("repo: inspect repository directory: %w", err)
	}
	if info.Mode()&fs.ModeSymlink != 0 || !info.IsDir() {
		return Docs{}, errors.New("repo: repository path is not a real directory")
	}

	root, err := os.OpenRoot(repoDir)
	if err != nil {
		return Docs{}, fmt.Errorf("repo: open repository directory: %w", err)
	}
	defer root.Close()

	docsDir, err := realDir(root, "docs")
	if err != nil {
		return Docs{}, err
	}
	gistDir := false
	if docsDir {
		gistDir, err = realDir(root, filepath.Join("docs", "gist"))
		if err != nil {
			return Docs{}, err
		}
	}

	docs := Docs{Files: make(map[string]string, len(docNames))}
	for _, name := range docNames {
		filename := name + ".md"
		found := ""

		if gistDir {
			rel := filepath.Join("docs", "gist", filename)
			ok, err := realFile(root, rel)
			if err != nil {
				return Docs{}, err
			}
			if ok {
				found = filepath.Join(repoDir, rel)
			}
		}
		if found == "" && docsDir {
			rel := filepath.Join("docs", filename)
			ok, err := realFile(root, rel)
			if err != nil {
				return Docs{}, err
			}
			if ok {
				found = filepath.Join(repoDir, rel)
			}
		}

		if found == "" {
			docs.Missing = append(docs.Missing, name)
			continue
		}
		docs.Files[name] = found
	}

	return docs, nil
}

// Entry summarizes one installed repository for /list.
type Entry struct {
	ID      string
	Display string // "owner/repo"
	Docs    []string
	ModTime time.Time
}

// List returns installed repositories directly under workDir. Hidden names,
// symlinks, non-directories, names that are not canonical repository IDs, and
// directories without a discovered document are skipped. Entries are ordered
// by newest document modification time, newest first, with the ID as a
// deterministic tie-breaker.
func List(workDir string) ([]Entry, error) {
	if workDir == "" || !filepath.IsAbs(workDir) {
		return nil, errors.New("repo: work directory must be an absolute path")
	}
	workDir = filepath.Clean(workDir)

	children, err := os.ReadDir(workDir)
	if err != nil {
		return nil, fmt.Errorf("repo: read work directory: %w", err)
	}

	entries := make([]Entry, 0, len(children))
	for _, child := range children {
		id := child.Name()
		if strings.HasPrefix(id, ".") || !ValidID(id) {
			continue
		}

		info, err := child.Info()
		if err != nil {
			return nil, fmt.Errorf("repo: inspect %s: %w", id, err)
		}
		if info.Mode()&fs.ModeSymlink != 0 || !info.IsDir() {
			continue
		}

		docs, err := DiscoverDocs(filepath.Join(workDir, id))
		if err != nil {
			return nil, err
		}
		present, newest, err := summarizeDocs(docs.Files)
		if err != nil {
			return nil, err
		}
		if len(present) == 0 {
			continue
		}

		owner, name, ok := splitID(id)
		if !ok {
			continue
		}
		entries = append(entries, Entry{
			ID:      id,
			Display: owner + "/" + name,
			Docs:    present,
			ModTime: newest,
		})
	}

	sort.Slice(entries, func(i, j int) bool {
		if !entries[i].ModTime.Equal(entries[j].ModTime) {
			return entries[i].ModTime.After(entries[j].ModTime)
		}
		return entries[i].ID < entries[j].ID
	})

	return entries, nil
}

func realDir(root *os.Root, name string) (bool, error) {
	info, err := root.Lstat(name)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return false, nil
		}
		return false, fmt.Errorf("repo: inspect %s: %w", name, err)
	}
	return info.Mode()&fs.ModeSymlink == 0 && info.IsDir(), nil
}

func realFile(root *os.Root, name string) (bool, error) {
	info, err := root.Lstat(name)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return false, nil
		}
		return false, fmt.Errorf("repo: inspect %s: %w", name, err)
	}
	return info.Mode().IsRegular(), nil
}

// summarizeDocs returns the available doc names in canonical order and the
// newest modification time among them.
func summarizeDocs(files map[string]string) ([]string, time.Time, error) {
	var present []string
	var newest time.Time

	for _, name := range docNames {
		path, ok := files[name]
		if !ok {
			continue
		}
		info, err := os.Lstat(path)
		if err != nil {
			return nil, time.Time{}, fmt.Errorf("repo: inspect document %s: %w", path, err)
		}
		if !info.Mode().IsRegular() {
			continue
		}
		present = append(present, name)
		if info.ModTime().After(newest) {
			newest = info.ModTime()
		}
	}

	return present, newest, nil
}
