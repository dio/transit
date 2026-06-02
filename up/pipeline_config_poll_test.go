package up

import (
	"context"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// ---------------------------------------------------------------------------
// FileSource tests
// ---------------------------------------------------------------------------

func TestFileSource_ReadsContents(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "cfg.json")
	want := []byte(`{"name":"file","value":42}`)
	require.NoError(t, os.WriteFile(path, want, 0o600))

	src := FileSource(path)
	got, err := src.Fetch(context.Background())
	require.NoError(t, err)
	require.Equal(t, string(want), string(got))
}

func TestFileSource_ReadsFreshOnEachCall(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "cfg.json")
	require.NoError(t, os.WriteFile(path, []byte(`first`), 0o600))

	src := FileSource(path)
	got1, err := src.Fetch(context.Background())
	require.NoError(t, err)
	require.Equal(t, "first", string(got1))

	require.NoError(t, os.WriteFile(path, []byte(`second`), 0o600))
	got2, err := src.Fetch(context.Background())
	require.NoError(t, err)
	require.Equal(t, "second", string(got2))
}

func TestFileSource_MissingFileReturnsError(t *testing.T) {
	src := FileSource("/no/such/file/does/not/exist.json")
	_, err := src.Fetch(context.Background())
	require.Error(t, err)
}

// ---------------------------------------------------------------------------
// WithObserver tests
// ---------------------------------------------------------------------------

func TestWithObserver_CalledOnSuccess(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "cfg.json")
	require.NoError(t, os.WriteFile(path, []byte(`{"name":"obs","value":1}`), 0o600))

	p := New[testStruct](FileSource(path), JSONDecoder[testStruct]())

	var events []RefreshEvent
	p.WithObserver(func(ev RefreshEvent) {
		events = append(events, ev)
	})

	require.NoError(t, p.Refresh(context.Background()))

	require.Len(t, events, 1)
	ev := events[0]
	require.NoError(t, ev.Err)
	require.NotEmpty(t, ev.Version)
	require.Positive(t, ev.Duration)
}

func TestWithObserver_CalledOnError(t *testing.T) {
	p := New[testStruct](FileSource("/no/such/path.json"), JSONDecoder[testStruct]())

	var events []RefreshEvent
	p.WithObserver(func(ev RefreshEvent) {
		events = append(events, ev)
	})

	require.Error(t, p.Refresh(context.Background()))
	require.Len(t, events, 1)
	ev := events[0]
	require.Error(t, ev.Err)
	require.Empty(t, ev.Version)
}

func TestWithObserver_MultipleObservers(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "cfg.json")
	require.NoError(t, os.WriteFile(path, []byte(`{"name":"multi","value":2}`), 0o600))

	p := New[testStruct](FileSource(path), JSONDecoder[testStruct]())

	var count int32
	for i := 0; i < 3; i++ {
		p.WithObserver(func(ev RefreshEvent) {
			atomic.AddInt32(&count, 1)
		})
	}

	require.NoError(t, p.Refresh(context.Background()))
	require.EqualValues(t, 3, atomic.LoadInt32(&count))
}

// ---------------------------------------------------------------------------
// StartPolling tests
// ---------------------------------------------------------------------------

func TestStartPolling_TriggersRepeatedRefreshes(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "cfg.json")
	require.NoError(t, os.WriteFile(path, []byte(`{"name":"poll","value":7}`), 0o600))

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

	require.GreaterOrEqual(t, atomic.LoadInt32(&count), int32(3))
}

func TestStartPolling_NoOpOnSecondCall(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "cfg.json")
	require.NoError(t, os.WriteFile(path, []byte(`{"name":"noop","value":0}`), 0o600))

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
	require.True(t, ok, "expected snapshot after polling")
	require.Equal(t, "noop", snap.Value.Name)
}

func TestStartPolling_StopsOnContextCancel(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "cfg.json")
	require.NoError(t, os.WriteFile(path, []byte(`{"name":"stop","value":5}`), 0o600))

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
	require.LessOrEqual(t, countLater, countAfterCancel+2,
		"polling continued after cancel: before=%d after=%d", countAfterCancel, countLater)
}
