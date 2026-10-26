package telegram

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"gistbot/internal/state"
)

const testOwnerID int64 = 4242

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// pollCall records one GetUpdates invocation.
type pollCall struct {
	offset  int64
	timeout time.Duration
}

// fakeUpdates replays batches in order. Once the batches are exhausted it
// blocks like a real long poll until ctx is cancelled, so tests always shut a
// poller down explicitly instead of letting it spin.
type fakeUpdates struct {
	mu      sync.Mutex
	calls   []pollCall
	batches [][]Update
	err     error
}

func (f *fakeUpdates) GetUpdates(ctx context.Context, offset int64, timeout time.Duration) ([]Update, error) {
	f.mu.Lock()
	f.calls = append(f.calls, pollCall{offset: offset, timeout: timeout})
	if len(f.batches) > 0 {
		batch := f.batches[0]
		f.batches = f.batches[1:]
		f.mu.Unlock()
		return batch, nil
	}
	err := f.err
	f.mu.Unlock()

	if err != nil {
		return nil, err
	}

	<-ctx.Done()
	return nil, ctx.Err()
}

func (f *fakeUpdates) callSnapshot() []pollCall {
	f.mu.Lock()
	defer f.mu.Unlock()

	calls := make([]pollCall, len(f.calls))
	copy(calls, f.calls)
	return calls
}

func (f *fakeUpdates) waitCalls(t *testing.T, want int) {
	t.Helper()

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if len(f.callSnapshot()) >= want {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("GetUpdates called %d times, want at least %d", len(f.callSnapshot()), want)
}

type recordingHandler struct {
	mu      sync.Mutex
	updates []Update
}

func (h *recordingHandler) Handle(_ context.Context, update Update) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.updates = append(h.updates, update)
}

func (h *recordingHandler) snapshot() []Update {
	h.mu.Lock()
	defer h.mu.Unlock()

	updates := make([]Update, len(h.updates))
	copy(updates, h.updates)
	return updates
}

type handlerFunc func(context.Context, Update)

func (f handlerFunc) Handle(ctx context.Context, update Update) {
	f(ctx, update)
}

func newPollerStore(t *testing.T) *state.Store {
	t.Helper()

	store, err := state.Open(filepath.Join(t.TempDir(), "state.json"), state.Data{Runner: "cli", Model: "flash"})
	if err != nil {
		t.Fatalf("state.Open: %v", err)
	}
	return store
}

// brokenStore returns an open store whose state directory has been replaced by
// a regular file, so every mutation fails.
func brokenStore(t *testing.T) *state.Store {
	t.Helper()

	root := t.TempDir()
	dir := filepath.Join(root, "state")
	store, err := state.Open(filepath.Join(dir, "state.json"), state.Data{})
	if err != nil {
		t.Fatalf("state.Open: %v", err)
	}
	if err := os.RemoveAll(dir); err != nil {
		t.Fatalf("RemoveAll state directory: %v", err)
	}
	if err := os.WriteFile(dir, []byte("not a directory"), 0o600); err != nil {
		t.Fatalf("WriteFile over state directory: %v", err)
	}
	return store
}

func ownerMessage(updateID int64, text string) Update {
	return Update{
		UpdateID: updateID,
		Message: &Message{
			MessageID: updateID + 100,
			From:      &User{ID: testOwnerID, FirstName: "Owner"},
			Chat:      Chat{ID: testOwnerID, Type: "private"},
			Date:      1700000000,
			Text:      text,
		},
	}
}

// runPoller runs p until the fake has served wantCalls calls, then cancels the
// context and returns Run's result.
func runPoller(t *testing.T, p *Poller, fake *fakeUpdates, wantCalls int) error {
	t.Helper()

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	errCh := make(chan error, 1)
	go func() {
		errCh <- p.Run(ctx)
	}()

	fake.waitCalls(t, wantCalls)
	cancel()

	select {
	case err := <-errCh:
		return err
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return after cancellation")
		return nil
	}
}

func TestOwnerPrivateMessageIsHandled(t *testing.T) {
	store := newPollerStore(t)
	update := ownerMessage(1, "hello")
	fake := &fakeUpdates{batches: [][]Update{{update}}}
	handler := &recordingHandler{}

	p := NewPoller(fake, store, testOwnerID, handler, discardLogger())
	if err := runPoller(t, p, fake, 2); err != nil {
		t.Fatalf("Run: %v", err)
	}

	handled := handler.snapshot()
	if len(handled) != 1 {
		t.Fatalf("handler called %d times, want 1", len(handled))
	}
	if handled[0].UpdateID != update.UpdateID {
		t.Errorf("handled update ID = %d, want %d", handled[0].UpdateID, update.UpdateID)
	}
	if got := handled[0].Message.Text; got != "hello" {
		t.Errorf("handled text = %q, want %q", got, "hello")
	}

	calls := fake.callSnapshot()
	if calls[0].offset != 0 {
		t.Errorf("initial offset = %d, want 0", calls[0].offset)
	}
	if calls[0].timeout != 30*time.Second {
		t.Errorf("poll timeout = %v, want 30s", calls[0].timeout)
	}

	if got := store.Snapshot().UpdateOffset; got != 2 {
		t.Errorf("stored offset = %d, want 2", got)
	}
}

func TestGroupChannelBotAndOtherUserAreIgnored(t *testing.T) {
	store := newPollerStore(t)
	fake := &fakeUpdates{batches: [][]Update{{
		{UpdateID: 10, Message: &Message{
			From: &User{ID: testOwnerID},
			Chat: Chat{ID: -100, Type: "group"},
			Text: "group",
		}},
		{UpdateID: 11, Message: &Message{
			From: &User{ID: testOwnerID},
			Chat: Chat{ID: -200, Type: "channel"},
			Text: "channel",
		}},
		{UpdateID: 12, Message: &Message{
			From: &User{ID: 99, IsBot: true},
			Chat: Chat{ID: 99, Type: "private"},
			Text: "bot",
		}},
		{UpdateID: 13, Message: &Message{
			From: &User{ID: 777},
			Chat: Chat{ID: 777, Type: "private"},
			Text: "stranger",
		}},
		{UpdateID: 14, Message: &Message{
			Chat: Chat{ID: testOwnerID, Type: "private"},
			Text: "no sender",
		}}, // private chat without a sender
		{UpdateID: 15}, // unknown update type
	}}}
	handler := &recordingHandler{}

	p := NewPoller(fake, store, testOwnerID, handler, discardLogger())
	if err := runPoller(t, p, fake, 2); err != nil {
		t.Fatalf("Run: %v", err)
	}

	if handled := handler.snapshot(); len(handled) != 0 {
		t.Errorf("handler called %d times, want 0", len(handled))
	}
	if got := store.Snapshot().UpdateOffset; got != 16 {
		t.Errorf("stored offset = %d, want 16", got)
	}
}

func TestOffsetIsPersistedBeforeHandler(t *testing.T) {
	store := newPollerStore(t)
	fake := &fakeUpdates{batches: [][]Update{{ownerMessage(7, "persist first")}}}

	var sawOffset int64
	handler := handlerFunc(func(_ context.Context, _ Update) {
		sawOffset = store.Snapshot().UpdateOffset
	})

	p := NewPoller(fake, store, testOwnerID, handler, discardLogger())
	if err := runPoller(t, p, fake, 2); err != nil {
		t.Fatalf("Run: %v", err)
	}

	if sawOffset != 8 {
		t.Errorf("offset visible in handler = %d, want 8", sawOffset)
	}
}

func TestOffsetFailureStopsDispatch(t *testing.T) {
	store := brokenStore(t)
	fake := &fakeUpdates{batches: [][]Update{{ownerMessage(5, "must not run")}}}
	handler := &recordingHandler{}

	p := NewPoller(fake, store, testOwnerID, handler, discardLogger())

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	err := p.Run(ctx)
	if err == nil {
		t.Fatal("Run returned nil, want a state persistence error")
	}

	if handled := handler.snapshot(); len(handled) != 0 {
		t.Errorf("handler called %d times after offset failure, want 0", len(handled))
	}
	if got := len(fake.callSnapshot()); got != 1 {
		t.Errorf("GetUpdates called %d times, want 1", got)
	}
}

func TestRestartDoesNotReplay(t *testing.T) {
	store := newPollerStore(t)
	first := &fakeUpdates{batches: [][]Update{{
		ownerMessage(1, "first"),
		ownerMessage(2, "second"),
	}}}
	handler := &recordingHandler{}

	p := NewPoller(first, store, testOwnerID, handler, discardLogger())
	if err := runPoller(t, p, first, 2); err != nil {
		t.Fatalf("first Run: %v", err)
	}
	if got := store.Snapshot().UpdateOffset; got != 3 {
		t.Fatalf("stored offset after first run = %d, want 3", got)
	}

	second := &fakeUpdates{}
	restarted := NewPoller(second, store, testOwnerID, handler, discardLogger())
	if err := runPoller(t, restarted, second, 1); err != nil {
		t.Fatalf("second Run: %v", err)
	}

	calls := second.callSnapshot()
	if len(calls) == 0 {
		t.Fatal("restarted poller made no GetUpdates call")
	}
	if calls[0].offset != 3 {
		t.Errorf("restarted offset = %d, want 3", calls[0].offset)
	}
	if handled := handler.snapshot(); len(handled) != 2 {
		t.Errorf("handler called %d times total, want 2 (no replay)", len(handled))
	}
}

func TestPollErrorsBackOff(t *testing.T) {
	store := newPollerStore(t)
	// A per-attempt HTTP timeout surfaces as DeadlineExceeded even while the
	// parent context is alive; the poller must back off rather than shut down.
	fake := &fakeUpdates{err: fmt.Errorf("telegram getUpdates: %w", context.DeadlineExceeded)}
	handler := &recordingHandler{}

	p := NewPoller(fake, store, testOwnerID, handler, discardLogger())

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var waits []time.Duration
	p.sleep = func(ctx context.Context, d time.Duration) error {
		waits = append(waits, d)
		if len(waits) == 3 {
			cancel()
			return ctx.Err()
		}
		return nil
	}

	if err := p.Run(ctx); err != nil {
		t.Fatalf("Run after cancellation = %v, want nil", err)
	}

	want := []time.Duration{time.Second, 2 * time.Second, 4 * time.Second}
	if len(waits) != len(want) {
		t.Fatalf("backoff waits = %v, want %v", waits, want)
	}
	for i := range want {
		if waits[i] != want[i] {
			t.Errorf("wait %d = %v, want %v", i, waits[i], want[i])
		}
	}
}

func TestPollBackoffCapsAtThirtySeconds(t *testing.T) {
	store := newPollerStore(t)
	fake := &fakeUpdates{err: errors.New("poll failed")}

	p := NewPoller(fake, store, testOwnerID, &recordingHandler{}, discardLogger())

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var waits []time.Duration
	p.sleep = func(ctx context.Context, d time.Duration) error {
		waits = append(waits, d)
		if len(waits) == 6 {
			cancel()
			return ctx.Err()
		}
		return nil
	}

	if err := p.Run(ctx); err != nil {
		t.Fatalf("Run after cancellation = %v, want nil", err)
	}

	want := []time.Duration{
		time.Second,
		2 * time.Second,
		4 * time.Second,
		8 * time.Second,
		16 * time.Second,
		30 * time.Second,
	}
	if len(waits) != len(want) {
		t.Fatalf("backoff waits = %v, want %v", waits, want)
	}
	for i := range want {
		if waits[i] != want[i] {
			t.Errorf("wait %d = %v, want %v", i, waits[i], want[i])
		}
	}
}

func TestContextCancelReturnsNil(t *testing.T) {
	store := newPollerStore(t)
	fake := &fakeUpdates{}
	handler := &recordingHandler{}

	p := NewPoller(fake, store, testOwnerID, handler, discardLogger())

	ctx, cancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)
	go func() {
		errCh <- p.Run(ctx)
	}()

	fake.waitCalls(t, 1)
	cancel()

	select {
	case err := <-errCh:
		if err != nil {
			t.Fatalf("Run after cancellation = %v, want nil", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return after cancellation")
	}

	if handled := handler.snapshot(); len(handled) != 0 {
		t.Errorf("handler called %d times, want 0", len(handled))
	}
}
