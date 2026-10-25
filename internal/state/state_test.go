package state

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func testDefaults() Data {
	return Data{Runner: "cli", Model: "flash"}
}

func openStore(t *testing.T, path string, defaults Data) *Store {
	t.Helper()
	store, err := Open(path, defaults)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	return store
}

func TestOpenCreatesDefaultsAndPrivateFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "gistbot", "state.json")
	store := openStore(t, path, testDefaults())

	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("Stat state file: %v", err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Errorf("state file mode = %o, want 600", got)
	}

	dirInfo, err := os.Stat(filepath.Dir(path))
	if err != nil {
		t.Fatalf("Stat state directory: %v", err)
	}
	if got := dirInfo.Mode().Perm(); got != 0o700 {
		t.Errorf("state directory mode = %o, want 700", got)
	}

	snapshot := store.Snapshot()
	if snapshot.Runner != "cli" {
		t.Errorf("Runner = %q, want cli", snapshot.Runner)
	}
	if snapshot.Model != "flash" {
		t.Errorf("Model = %q, want flash", snapshot.Model)
	}
	if snapshot.UpdateOffset != 0 {
		t.Errorf("UpdateOffset = %d, want 0", snapshot.UpdateOffset)
	}
	if len(snapshot.Jobs) != 0 {
		t.Errorf("len(Jobs) = %d, want 0", len(snapshot.Jobs))
	}
}

func TestSetOffsetPersistsAcrossOpen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	store := openStore(t, path, testDefaults())

	if err := store.SetOffset(1234); err != nil {
		t.Fatalf("SetOffset: %v", err)
	}

	reopened := openStore(t, path, Data{})
	if got := reopened.Snapshot().UpdateOffset; got != 1234 {
		t.Errorf("UpdateOffset after reopen = %d, want 1234", got)
	}
}

func TestRunnerModelAndFreeOverridePersist(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	store := openStore(t, path, testDefaults())

	if err := store.SetRunner("api"); err != nil {
		t.Fatalf("SetRunner: %v", err)
	}
	if err := store.SetModel("pro"); err != nil {
		t.Fatalf("SetModel: %v", err)
	}
	if err := store.SetFreeModel("opencode/kimi-k2-free#fast"); err != nil {
		t.Fatalf("SetFreeModel: %v", err)
	}

	reopened := openStore(t, path, testDefaults())
	snapshot := reopened.Snapshot()
	if snapshot.Runner != "api" {
		t.Errorf("Runner after reopen = %q, want api", snapshot.Runner)
	}
	if snapshot.Model != "pro" {
		t.Errorf("Model after reopen = %q, want pro", snapshot.Model)
	}
	if snapshot.FreeModel != "opencode/kimi-k2-free#fast" {
		t.Errorf("FreeModel after reopen = %q", snapshot.FreeModel)
	}

	if err := reopened.ClearFreeModel(); err != nil {
		t.Fatalf("ClearFreeModel: %v", err)
	}
	cleared := openStore(t, path, testDefaults()).Snapshot()
	if cleared.FreeModel != "" {
		t.Errorf("FreeModel after clear = %q, want empty", cleared.FreeModel)
	}
	if cleared.Runner != "api" || cleared.Model != "pro" {
		t.Errorf("clearing free override changed runner/model: %q/%q", cleared.Runner, cleared.Model)
	}
}

func TestUpsertJobKeepsLastTwenty(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	store := openStore(t, path, testDefaults())

	for i := 0; i < 25; i++ {
		job := JobRecord{
			ID:     fmt.Sprintf("job-%02d", i),
			RepoID: "owner/repo",
			URL:    "https://github.com/owner/repo",
			Runner: "cli",
			Model:  "flash",
			Status: StatusQueued,
			Docs:   []string{fmt.Sprintf("doc-%d", i)},
		}
		if err := store.UpsertJob(job); err != nil {
			t.Fatalf("UpsertJob %d: %v", i, err)
		}
	}

	snapshot := store.Snapshot()
	if len(snapshot.Jobs) != 20 {
		t.Fatalf("len(Jobs) = %d, want 20", len(snapshot.Jobs))
	}
	if snapshot.Jobs[0].ID != "job-05" {
		t.Errorf("oldest job = %q, want job-05", snapshot.Jobs[0].ID)
	}
	if snapshot.Jobs[19].ID != "job-24" {
		t.Errorf("newest job = %q, want job-24", snapshot.Jobs[19].ID)
	}

	replacement := JobRecord{
		ID:     "job-10",
		RepoID: "owner/repo",
		URL:    "https://github.com/owner/repo",
		Runner: "api",
		Model:  "pro",
		Status: StatusFailed,
		Error:  "boom",
		Docs:   []string{"replacement"},
	}
	if err := store.UpsertJob(replacement); err != nil {
		t.Fatalf("UpsertJob replacement: %v", err)
	}

	snapshot = store.Snapshot()
	if len(snapshot.Jobs) != 20 {
		t.Fatalf("len(Jobs) after replacement = %d, want 20", len(snapshot.Jobs))
	}
	found := false
	for _, job := range snapshot.Jobs {
		if job.ID != "job-10" {
			continue
		}
		found = true
		if job.Status != StatusFailed || job.Error != "boom" || job.Runner != "api" || job.Model != "pro" {
			t.Errorf("replaced job = %+v", job)
		}
		if len(job.Docs) != 1 || job.Docs[0] != "replacement" {
			t.Errorf("replaced job docs = %v", job.Docs)
		}
	}
	if !found {
		t.Error("replaced job job-10 not found")
	}

	// The caller must not be able to mutate stored data through its argument.
	replacement.Docs[0] = "mutated by caller"
	snapshot = store.Snapshot()
	for _, job := range snapshot.Jobs {
		if job.ID == "job-10" && job.Docs[0] != "replacement" {
			t.Errorf("stored docs mutated through caller slice: %v", job.Docs)
		}
	}

	persisted := openStore(t, path, Data{}).Snapshot()
	if len(persisted.Jobs) != 20 {
		t.Fatalf("len(Jobs) after reopen = %d, want 20", len(persisted.Jobs))
	}
}

func TestUpdateJob(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	store := openStore(t, path, testDefaults())

	for _, id := range []string{"job-a", "job-b"} {
		if err := store.UpsertJob(JobRecord{ID: id, Status: StatusQueued, Docs: []string{"original"}}); err != nil {
			t.Fatalf("UpsertJob %s: %v", id, err)
		}
	}

	started := time.Date(2026, 10, 25, 10, 0, 0, 0, time.UTC)
	finished := started.Add(2 * time.Minute)

	var during Data
	var leakedDocs []string
	done := make(chan error, 1)
	go func() {
		done <- store.UpdateJob("job-a", func(job *JobRecord) {
			// Snapshot from inside the callback must not deadlock.
			during = store.Snapshot()
			job.Status = StatusSucceeded
			job.StartedAt = started
			job.FinishedAt = finished
			job.Error = ""
			job.Docs = append(job.Docs, "added")
			leakedDocs = job.Docs
		})
	}()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("UpdateJob: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("UpdateJob deadlocked when the callback called Snapshot")
	}

	if during.UpdateOffset != 0 {
		t.Errorf("snapshot taken inside callback looks wrong: %+v", during)
	}

	// Mutating the callback's slice after return must not change stored data.
	leakedDocs[0] = "mutated after return"

	snapshot := store.Snapshot()
	if len(snapshot.Jobs) != 2 {
		t.Fatalf("len(Jobs) = %d, want 2", len(snapshot.Jobs))
	}
	updated := snapshot.Jobs[0]
	if updated.ID != "job-a" {
		t.Fatalf("first job ID = %q, want job-a", updated.ID)
	}
	if updated.Status != StatusSucceeded {
		t.Errorf("Status = %q, want succeeded", updated.Status)
	}
	if !updated.StartedAt.Equal(started) {
		t.Errorf("StartedAt = %v, want %v", updated.StartedAt, started)
	}
	if !updated.FinishedAt.Equal(finished) {
		t.Errorf("FinishedAt = %v, want %v", updated.FinishedAt, finished)
	}
	if updated.Docs[0] != "original" {
		t.Errorf("Docs[0] = %q, want original", updated.Docs[0])
	}
	if len(updated.Docs) != 2 || updated.Docs[1] != "added" {
		t.Errorf("Docs = %v, want [original added]", updated.Docs)
	}
	if snapshot.Jobs[1].Status != StatusQueued {
		t.Errorf("unrelated job changed: %+v", snapshot.Jobs[1])
	}

	persisted := openStore(t, path, Data{}).Snapshot()
	if persisted.Jobs[0].Status != StatusSucceeded {
		t.Errorf("persisted Status = %q, want succeeded", persisted.Jobs[0].Status)
	}
	if persisted.Jobs[0].Docs[0] != "original" {
		t.Errorf("persisted Docs[0] = %q, want original", persisted.Jobs[0].Docs[0])
	}

	if err := store.UpdateJob("missing", func(*JobRecord) {}); err == nil {
		t.Error("UpdateJob on missing job returned nil error")
	} else if errors.Is(err, os.ErrNotExist) {
		t.Errorf("UpdateJob missing returned filesystem error: %v", err)
	}
	if err := store.UpdateJob("job-a", nil); err == nil {
		t.Error("UpdateJob with nil callback returned nil error")
	}
}

func TestMarkInterrupted(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	store := openStore(t, path, testDefaults())

	now := time.Date(2026, 10, 25, 12, 30, 0, 0, time.UTC)
	earlier := now.Add(-time.Hour)
	jobs := []JobRecord{
		{ID: "queued", Status: StatusQueued, StartedAt: earlier},
		{ID: "running", Status: StatusRunning, StartedAt: earlier},
		{ID: "succeeded", Status: StatusSucceeded, StartedAt: earlier, FinishedAt: earlier, Error: "kept"},
		{ID: "failed", Status: StatusFailed, StartedAt: earlier, FinishedAt: earlier, Error: "prior failure"},
	}
	for _, job := range jobs {
		if err := store.UpsertJob(job); err != nil {
			t.Fatalf("UpsertJob %s: %v", job.ID, err)
		}
	}

	if err := store.MarkInterrupted(now); err != nil {
		t.Fatalf("MarkInterrupted: %v", err)
	}

	snapshot := openStore(t, path, Data{}).Snapshot()
	byID := make(map[string]JobRecord, len(snapshot.Jobs))
	for _, job := range snapshot.Jobs {
		byID[job.ID] = job
	}

	for _, id := range []string{"queued", "running"} {
		job := byID[id]
		if job.Status != StatusInterrupted {
			t.Errorf("%s Status = %q, want interrupted", id, job.Status)
		}
		if !job.FinishedAt.Equal(now) {
			t.Errorf("%s FinishedAt = %v, want %v", id, job.FinishedAt, now)
		}
		if job.Error != "bot restarted" {
			t.Errorf("%s Error = %q, want %q", id, job.Error, "bot restarted")
		}
		if !job.StartedAt.Equal(earlier) {
			t.Errorf("%s StartedAt = %v, want %v", id, job.StartedAt, earlier)
		}
	}

	succeeded := byID["succeeded"]
	if succeeded.Status != StatusSucceeded || !succeeded.FinishedAt.Equal(earlier) || succeeded.Error != "kept" {
		t.Errorf("succeeded job changed: %+v", succeeded)
	}
	failed := byID["failed"]
	if failed.Status != StatusFailed || !failed.FinishedAt.Equal(earlier) || failed.Error != "prior failure" {
		t.Errorf("failed job changed: %+v", failed)
	}
}

func TestCorruptStateReturnsError(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	original := []byte(`{"update_offset": 7, "runner": `)
	if err := os.WriteFile(path, original, 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	if _, err := Open(path, testDefaults()); err == nil {
		t.Fatal("Open of corrupt state returned nil error")
	}

	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile after failed Open: %v", err)
	}
	if string(after) != string(original) {
		t.Errorf("corrupt state file was overwritten: got %q, want %q", after, original)
	}
}

func TestConcurrentUpdatesRace(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	store := openStore(t, path, testDefaults())

	for i := 0; i < 5; i++ {
		if err := store.UpsertJob(JobRecord{ID: fmt.Sprintf("job-%d", i), Status: StatusQueued}); err != nil {
			t.Fatalf("UpsertJob: %v", err)
		}
	}

	var (
		wg       sync.WaitGroup
		errMu    sync.Mutex
		firstErr error
	)
	recordErr := func(err error) {
		if err == nil {
			return
		}
		errMu.Lock()
		defer errMu.Unlock()
		if firstErr == nil {
			firstErr = err
		}
	}

	const workers = 8
	const iterations = 10
	for worker := 0; worker < workers; worker++ {
		wg.Add(1)
		go func(worker int) {
			defer wg.Done()
			for i := 0; i < iterations; i++ {
				id := fmt.Sprintf("job-%d", (worker+i)%5)
				recordErr(store.SetOffset(int64(worker*iterations + i)))
				recordErr(store.SetRunner(fmt.Sprintf("runner-%d", worker%2)))
				recordErr(store.SetModel(fmt.Sprintf("model-%d", i%3)))
				recordErr(store.SetFreeModel(fmt.Sprintf("opencode/free-%d", worker)))
				recordErr(store.UpsertJob(JobRecord{
					ID:     id,
					Status: StatusRunning,
					Docs:   []string{fmt.Sprintf("worker-%d", worker)},
				}))
				recordErr(store.UpdateJob(id, func(job *JobRecord) {
					job.Error = fmt.Sprintf("worker-%d", worker)
					if len(job.Docs) > 0 {
						job.Docs[0] = fmt.Sprintf("updated-%d", worker)
					}
				}))
				snapshot := store.Snapshot()
				for _, job := range snapshot.Jobs {
					if len(job.Docs) > 0 {
						job.Docs[0] = "local mutation"
					}
				}
			}
		}(worker)
	}
	wg.Wait()

	if firstErr != nil {
		t.Fatalf("concurrent update error: %v", firstErr)
	}

	snapshot := store.Snapshot()
	if len(snapshot.Jobs) != 5 {
		t.Fatalf("len(Jobs) = %d, want 5", len(snapshot.Jobs))
	}
	for _, job := range snapshot.Jobs {
		if len(job.Docs) > 0 && job.Docs[0] == "local mutation" {
			t.Errorf("snapshot mutation leaked into store for %s", job.ID)
		}
	}

	persisted := openStore(t, path, Data{}).Snapshot()
	if len(persisted.Jobs) != 5 {
		t.Fatalf("len(persisted Jobs) = %d, want 5", len(persisted.Jobs))
	}
}

func TestSnapshotReturnsDeepCopy(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	store := openStore(t, path, testDefaults())

	if err := store.UpsertJob(JobRecord{ID: "job", Status: StatusQueued, Docs: []string{"one", "two"}}); err != nil {
		t.Fatalf("UpsertJob: %v", err)
	}

	first := store.Snapshot()
	first.Runner = "changed"
	first.Jobs[0].Status = StatusFailed
	first.Jobs[0].Docs[0] = "changed"

	second := store.Snapshot()
	if second.Runner != "cli" {
		t.Errorf("Runner = %q, want cli", second.Runner)
	}
	if second.Jobs[0].Status != StatusQueued {
		t.Errorf("Status = %q, want queued", second.Jobs[0].Status)
	}
	if second.Jobs[0].Docs[0] != "one" {
		t.Errorf("Docs[0] = %q, want one", second.Jobs[0].Docs[0])
	}
}

func TestOpenFillsEmptyRunnerAndModelFromDefaults(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	raw := []byte(`{"update_offset": 42, "runner": "", "model": "", "jobs": []}`)
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	store := openStore(t, path, Data{Runner: "api", Model: "pro"})
	snapshot := store.Snapshot()
	if snapshot.Runner != "api" {
		t.Errorf("Runner = %q, want api", snapshot.Runner)
	}
	if snapshot.Model != "pro" {
		t.Errorf("Model = %q, want pro", snapshot.Model)
	}
	if snapshot.UpdateOffset != 42 {
		t.Errorf("UpdateOffset = %d, want 42", snapshot.UpdateOffset)
	}
}
