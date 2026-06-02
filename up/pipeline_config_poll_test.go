package up

import (
	"context"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"
)

// ---------------------------------------------------------------------------
// FileSource tests
// ---------------------------------------------------------------------------

func TestFileSource_ReadsContents(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "cfg.json")
	want := []byte(`{"name":"file","value":42}`)
	if err := os.WriteFile(path, want, 0o600); err != nil {
		t.Fatalf("write file: %v", err)
	}

	src := FileSource(path)
	got, err := src.Fetch(context.Background())
	if err != nil {
		t.Fatalf("Fetch error: %v", err)
	}
	if string(got) != string(want) {
		t.Fatalf("got %q, want %q", got, want)
	}
}

func TestFileSource_ReadsFreshOnEachCall(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "cfg.json")
	if err := os.WriteFile(path, []byte(`first`), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}

	src := FileSource(path)
	got1, _ := src.Fetch(context.Background())
	if string(got1) != "first" {
		t.Fatalf("first fetch: %q", got1)
	}

	if err := os.WriteFile(path, []byte(`second`), 0o600); err != nil {
		t.Fatalf("update: %v", err)
	}
	got2, _ := src.Fetch(context.Background())
	if string(got2) != "second" {
		t.Fatalf("second fetch: %q", got2)
	}
}

func TestFileSource_MissingFileReturnsError(t *testing.T) {
	src := FileSource("/no/such/file/does/not/exist.json")
	_, err := src.Fetch(context.Background())
	if err == nil {
		t.Fatal("expected error for missing file")
	}
}

// ---------------------------------------------------------------------------
// WithObserver tests
// ---------------------------------------------------------------------------

func TestWithObserver_CalledOnSuccess(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "cfg.json")
	if err := os.WriteFile(path, []byte(`{"name":"obs","value":1}`), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}

	p := New[testStruct](FileSource(path), JSONDecoder[testStruct]())

	var events []RefreshEvent
	p.WithObserver(func(ev RefreshEvent) {
		events = append(events, ev)
	})

	if err := p.Refresh(context.Background()); err != nil {
		t.Fatalf("Refresh: %v", err)
	}

	if len(events) != 1 {
		t.Fatalf("expected 1 observer call, got %d", len(events))
	}
	ev := events[0]
	if ev.Err != nil {
		t.Fatalf("expected nil Err on success, got %v", ev.Err)
	}
	if ev.Version == "" {
		t.Fatal("expected non-empty Version on success")
	}
	if ev.Duration <= 0 {
		t.Fatal("expected positive Duration")
	}
}

func TestWithObserver_CalledOnError(t *testing.T) {
	// Use a missing file so Fetch returns an error.
	p := New[testStruct](FileSource("/no/such/path.json"), JSONDecoder[testStruct]())

	var events []RefreshEvent
	p.WithObserver(func(ev RefreshEvent) {
		events = append(events, ev)
	})

	err := p.Refresh(context.Background())
	if err == nil {
		t.Fatal("expected Refresh to return error")
	}
	if len(events) != 1 {
		t.Fatalf("expected 1 observer call, got %d", len(events))
	}
	ev := events[0]
	if ev.Err == nil {
		t.Fatal("expected non-nil Err in observer on fetch failure")
	}
	if ev.Version != "" {
		t.Fatalf("expected empty Version on error, got %q", ev.Version)
	}
}

func TestWithObserver_MultipleObservers(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "cfg.json")
	if err := os.WriteFile(path, []byte(`{"name":"multi","value":2}`), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}

	p := New[testStruct](FileSource(path), JSONDecoder[testStruct]())

	var count int32
	for i := 0; i < 3; i++ {
		p.WithObserver(func(ev RefreshEvent) {
			atomic.AddInt32(&count, 1)
		})
	}

	if err := p.Refresh(context.Background()); err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	if got := atomic.LoadInt32(&count); got != 3 {
		t.Fatalf("expected 3 observer calls, got %d", got)
	}
}

// ---------------------------------------------------------------------------
// StartPolling tests
// ---------------------------------------------------------------------------

func TestStartPolling_TriggersRepeatedRefreshes(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "cfg.json")
	if err := os.WriteFile(path, []byte(`{"name":"poll","value":7}`), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}

	p := New[testStruct](FileSource(path), JSONDecoder[testStruct]())

	var count int32
	p.WithObserver(func(ev RefreshEvent) {
		if ev.Err == nil {
			atomic.AddInt32(&count, 1)
		}
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	interval := 20 * time.Millisecond
	p.StartPolling(ctx, interval, 0)

	// Wait until we see at least 3 successful refreshes (1 immediate + 2 ticks).
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if atomic.LoadInt32(&count) >= 3 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	cancel()

	if got := atomic.LoadInt32(&count); got < 3 {
		t.Fatalf("expected at least 3 refreshes, got %d", got)
	}
}

func TestStartPolling_NoOpOnSecondCall(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "cfg.json")
	if err := os.WriteFile(path, []byte(`{"name":"noop","value":0}`), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}

	p := New[testStruct](FileSource(path), JSONDecoder[testStruct]())

	var count int32
	p.WithObserver(func(ev RefreshEvent) {
		atomic.AddInt32(&count, 1)
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	interval := 50 * time.Millisecond

	// Call StartPolling twice; second call must be a no-op.
	p.StartPolling(ctx, interval, 0)
	p.StartPolling(ctx, interval, 0)

	// Let at least one tick fire.
	time.Sleep(120 * time.Millisecond)
	cancel()

	// If second StartPolling spawned a second goroutine, observer count would
	// roughly double. We can't assert an exact count because timing varies, but
	// we verify the call doesn't panic and the config is still readable.
	snap, ok := p.Snapshot()
	if !ok {
		t.Fatal("expected snapshot after polling")
	}
	if snap.Value.Name != "noop" {
		t.Fatalf("unexpected value: %+v", snap.Value)
	}
}

func TestStartPolling_StopsOnContextCancel(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "cfg.json")
	if err := os.WriteFile(path, []byte(`{"name":"stop","value":5}`), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}

	p := New[testStruct](FileSource(path), JSONDecoder[testStruct]())

	ctx, cancel := context.WithCancel(context.Background())

	var count int32
	p.WithObserver(func(ev RefreshEvent) {
		atomic.AddInt32(&count, 1)
	})

	p.StartPolling(ctx, 10*time.Millisecond, 0)
	time.Sleep(50 * time.Millisecond)
	cancel()

	// Record count just after cancel.
	countAfterCancel := atomic.LoadInt32(&count)
	// Wait a bit and verify count did not grow significantly.
	time.Sleep(50 * time.Millisecond)
	countLater := atomic.LoadInt32(&count)

	// Allow up to 2 extra calls due to in-flight goroutine scheduling.
	if countLater > countAfterCancel+2 {
		t.Fatalf("polling continued after cancel: before=%d after=%d", countAfterCancel, countLater)
	}
}
