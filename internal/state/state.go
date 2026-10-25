// Package state provides Zero Two's durable, atomically written state file.
//
// The store keeps the Telegram update offset, the selected runner and model,
// an optional free-model override, and the most recent job records. Every
// mutation is persisted with a write-to-temp, fsync, rename, fsync-directory
// sequence so that readers never observe a partially written file.
package state

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sync"
	"time"
)

const (
	maxJobs = 20

	interruptedMessage = "bot restarted"

	stateFileMode = fs.FileMode(0o600)
	stateDirMode  = fs.FileMode(0o700)
)

// JobStatus describes the lifecycle state of a job.
type JobStatus string

const (
	StatusQueued      JobStatus = "queued"
	StatusRunning     JobStatus = "running"
	StatusSucceeded   JobStatus = "succeeded"
	StatusFailed      JobStatus = "failed"
	StatusCancelled   JobStatus = "cancelled"
	StatusTimedOut    JobStatus = "timed_out"
	StatusInterrupted JobStatus = "interrupted"
)

// JobRecord is the durable summary of one job.
type JobRecord struct {
	ID         string    `json:"id"`
	RepoID     string    `json:"repo_id"`
	URL        string    `json:"url"`
	Runner     string    `json:"runner"`
	Model      string    `json:"model"`
	Status     JobStatus `json:"status"`
	StartedAt  time.Time `json:"started_at,omitempty"`
	FinishedAt time.Time `json:"finished_at,omitempty"`
	Error      string    `json:"error,omitempty"`
	Docs       []string  `json:"docs,omitempty"`
}

// Data is the full persisted state.
type Data struct {
	UpdateOffset int64       `json:"update_offset"`
	Runner       string      `json:"runner"`
	Model        string      `json:"model"`
	FreeModel    string      `json:"free_model,omitempty"`
	Jobs         []JobRecord `json:"jobs"`
}

// Store guards a Data value and persists each mutation atomically. It is safe
// for concurrent use.
type Store struct {
	mu   sync.Mutex
	path string
	data Data
}

// Open loads the state at path. A missing file is created from defaults. A
// corrupt file returns an error and is left untouched. Empty Runner and Model
// values are filled from defaults.
func Open(path string, defaults Data) (*Store, error) {
	if path == "" {
		return nil, errors.New("state: path is empty")
	}

	store := &Store{
		path: filepath.Clean(path),
		data: cloneData(defaults),
	}

	raw, err := os.ReadFile(store.path)
	switch {
	case err == nil:
		var loaded Data
		if err := json.Unmarshal(raw, &loaded); err != nil {
			return nil, fmt.Errorf("state: decode %s: %w", store.path, err)
		}
		store.data = loaded
	case errors.Is(err, fs.ErrNotExist):
		if err := store.saveData(store.data); err != nil {
			return nil, err
		}
		return store, nil
	default:
		return nil, fmt.Errorf("state: read %s: %w", store.path, err)
	}

	if store.data.Runner == "" {
		store.data.Runner = defaults.Runner
	}
	if store.data.Model == "" {
		store.data.Model = defaults.Model
	}

	return store, nil
}

// Snapshot returns a deep copy of the current state.
func (s *Store) Snapshot() Data {
	s.mu.Lock()
	defer s.mu.Unlock()
	return cloneData(s.data)
}

// SetOffset stores the Telegram update offset.
func (s *Store) SetOffset(offset int64) error {
	return s.update(func(data *Data) {
		data.UpdateOffset = offset
	})
}

// SetRunner stores the selected runner.
func (s *Store) SetRunner(runner string) error {
	return s.update(func(data *Data) {
		data.Runner = runner
	})
}

// SetModel stores the selected model alias or reference.
func (s *Store) SetModel(alias string) error {
	return s.update(func(data *Data) {
		data.Model = alias
	})
}

// SetFreeModel stores a free-model override.
func (s *Store) SetFreeModel(model string) error {
	return s.update(func(data *Data) {
		data.FreeModel = model
	})
}

// ClearFreeModel removes the free-model override.
func (s *Store) ClearFreeModel() error {
	return s.update(func(data *Data) {
		data.FreeModel = ""
	})
}

// UpsertJob replaces the job with the same ID or appends a new job, then keeps
// only the newest maxJobs records.
func (s *Store) UpsertJob(job JobRecord) error {
	stored := cloneJob(job)
	return s.update(func(data *Data) {
		for i := range data.Jobs {
			if data.Jobs[i].ID == stored.ID {
				data.Jobs[i] = cloneJob(stored)
				data.Jobs = trimJobs(data.Jobs)
				return
			}
		}
		data.Jobs = append(data.Jobs, cloneJob(stored))
		data.Jobs = trimJobs(data.Jobs)
	})
}

// UpdateJob applies fn to a copy of the job with id and persists the result.
// The callback runs without the store lock held, so it may call other Store
// methods. Mutations made through the callback's slice after it returns do not
// affect stored data.
func (s *Store) UpdateJob(id string, fn func(*JobRecord)) error {
	if fn == nil {
		return errors.New("state: update job callback is nil")
	}

	s.mu.Lock()
	index := jobIndex(s.data.Jobs, id)
	if index < 0 {
		s.mu.Unlock()
		return fmt.Errorf("state: job %q not found", id)
	}
	job := cloneJob(s.data.Jobs[index])
	s.mu.Unlock()

	fn(&job)
	job.ID = id

	s.mu.Lock()
	defer s.mu.Unlock()

	index = jobIndex(s.data.Jobs, id)
	if index < 0 {
		return fmt.Errorf("state: job %q not found", id)
	}

	next := cloneData(s.data)
	next.Jobs[index] = cloneJob(job)
	if err := s.saveData(next); err != nil {
		return err
	}
	s.data = next
	return nil
}

// MarkInterrupted marks every queued or running job as interrupted by a bot
// restart and records now as its finish time.
func (s *Store) MarkInterrupted(now time.Time) error {
	return s.update(func(data *Data) {
		for i := range data.Jobs {
			job := &data.Jobs[i]
			if job.Status != StatusQueued && job.Status != StatusRunning {
				continue
			}
			job.Status = StatusInterrupted
			job.FinishedAt = now
			job.Error = interruptedMessage
		}
	})
}

// update applies fn to a deep copy of the data, persists it, and only then
// publishes it in memory so a failed write leaves the store unchanged.
func (s *Store) update(fn func(*Data)) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	next := cloneData(s.data)
	fn(&next)
	if err := s.saveData(next); err != nil {
		return err
	}
	s.data = next
	return nil
}

// saveData atomically replaces the state file with data.
func (s *Store) saveData(data Data) error {
	dir := filepath.Dir(s.path)
	if err := os.MkdirAll(dir, stateDirMode); err != nil {
		return fmt.Errorf("state: create directory %s: %w", dir, err)
	}

	payload, err := json.MarshalIndent(data, "", "  ")
	if err != nil {
		return fmt.Errorf("state: encode: %w", err)
	}
	payload = append(payload, '\n')

	temp, err := os.CreateTemp(dir, ".state-*.tmp")
	if err != nil {
		return fmt.Errorf("state: create temporary file: %w", err)
	}
	tempPath := temp.Name()
	removeTemp := func() {
		_ = temp.Close()
		_ = os.Remove(tempPath)
	}

	if _, err := temp.Write(payload); err != nil {
		removeTemp()
		return fmt.Errorf("state: write temporary file: %w", err)
	}
	if err := temp.Sync(); err != nil {
		removeTemp()
		return fmt.Errorf("state: sync temporary file: %w", err)
	}
	if err := temp.Chmod(stateFileMode); err != nil {
		removeTemp()
		return fmt.Errorf("state: set permissions: %w", err)
	}
	if err := temp.Close(); err != nil {
		_ = os.Remove(tempPath)
		return fmt.Errorf("state: close temporary file: %w", err)
	}
	if err := os.Rename(tempPath, s.path); err != nil {
		_ = os.Remove(tempPath)
		return fmt.Errorf("state: replace %s: %w", s.path, err)
	}

	dirFile, err := os.Open(dir)
	if err != nil {
		return fmt.Errorf("state: open directory %s: %w", dir, err)
	}
	defer dirFile.Close()
	if err := dirFile.Sync(); err != nil {
		return fmt.Errorf("state: sync directory %s: %w", dir, err)
	}
	return nil
}

func jobIndex(jobs []JobRecord, id string) int {
	for i := range jobs {
		if jobs[i].ID == id {
			return i
		}
	}
	return -1
}

func trimJobs(jobs []JobRecord) []JobRecord {
	if len(jobs) <= maxJobs {
		return jobs
	}
	kept := make([]JobRecord, maxJobs)
	copy(kept, jobs[len(jobs)-maxJobs:])
	return kept
}

func cloneData(data Data) Data {
	cloned := data
	if data.Jobs != nil {
		cloned.Jobs = make([]JobRecord, len(data.Jobs))
		for i, job := range data.Jobs {
			cloned.Jobs[i] = cloneJob(job)
		}
	}
	return cloned
}

func cloneJob(job JobRecord) JobRecord {
	cloned := job
	if job.Docs != nil {
		cloned.Docs = make([]string, len(job.Docs))
		copy(cloned.Docs, job.Docs)
	}
	return cloned
}
