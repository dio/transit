package up

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// testStruct is a simple type for decoder round-trip tests.
type testStruct struct {
	Name  string `json:"name" yaml:"name"`
	Value int    `json:"value" yaml:"value"`
}

// staticSrc returns a ConfigSource that always yields data.
func staticSrc(data []byte) ConfigSource {
	cp := make([]byte, len(data))
	copy(cp, data)
	return func(_ context.Context) ([]byte, error) { return cp, nil }
}

// errSrc returns a ConfigSource that always fails.
func errSrc(err error) ConfigSource {
	return func(_ context.Context) ([]byte, error) { return nil, err }
}

// --------------------------------------------------------------------------
// Static config
// --------------------------------------------------------------------------

func TestNewStaticConfig_ImmediateSnapshot(t *testing.T) {
	v := testStruct{Name: "static", Value: 42}
	p := NewStaticConfig(v)
	require.Equal(t, v, p.Snapshot())
}

func TestNewStaticConfig_StartStopNoOp(t *testing.T) {
	p := NewStaticConfig(testStruct{Name: "x"})
	stop := p.Start(context.Background())
	// If Start launched a goroutine it would race; stopping immediately is safe.
	stop()
	// Snapshot must still return original value.
	require.Equal(t, "x", p.Snapshot().Name)
}

// --------------------------------------------------------------------------
// RefreshOnce
// --------------------------------------------------------------------------

func TestRefreshOnce_UpdatesSnapshot(t *testing.T) {
	data := []byte(`{"name":"hello","value":7}`)
	p := NewPollingConfig(staticSrc(data), JSONDecoder[testStruct](), PollOptions{})
	require.NoError(t, p.RefreshOnce(context.Background()))
	require.Equal(t, testStruct{Name: "hello", Value: 7}, p.Snapshot())
}

func TestRefreshOnce_FetchErrorKeepsLastGood(t *testing.T) {
	data := []byte(`{"name":"good","value":1}`)
	var fail atomic.Bool
	src := ConfigSource(func(ctx context.Context) ([]byte, error) {
		if fail.Load() {
			return nil, errors.New("network down")
		}
		return data, nil
	})
	p := NewPollingConfig(src, JSONDecoder[testStruct](), PollOptions{})
	require.NoError(t, p.RefreshOnce(context.Background()))
	good := p.Snapshot()

	fail.Store(true)
	require.Error(t, p.RefreshOnce(context.Background()))
	require.Equal(t, good, p.Snapshot())
}

func TestRefreshOnce_DecodeErrorKeepsLastGood(t *testing.T) {
	good := []byte(`{"name":"good","value":1}`)
	bad := []byte(`{"name":"bad","value":2}`)
	var useBad atomic.Bool
	src := ConfigSource(func(ctx context.Context) ([]byte, error) {
		if useBad.Load() {
			return bad, nil
		}
		return good, nil
	})
	var failDec atomic.Bool
	dec := ConfigDecoder[testStruct](func(data []byte) (testStruct, error) {
		if failDec.Load() {
			return testStruct{}, errors.New("bad schema")
		}
		return JSONDecoder[testStruct]()(data)
	})
	p := NewPollingConfig(src, dec, PollOptions{})
	require.NoError(t, p.RefreshOnce(context.Background()))
	good2 := p.Snapshot()

	// Switch to new file content and broken decoder to trigger decode error.
	useBad.Store(true)
	failDec.Store(true)
	require.Error(t, p.RefreshOnce(context.Background()))
	require.Equal(t, good2, p.Snapshot())
}

func TestRefreshOnce_CtxCancellation(t *testing.T) {
	unblock := make(chan struct{})
	src := ConfigSource(func(ctx context.Context) ([]byte, error) {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-unblock:
			return []byte(`{}`), nil
		}
	})
	p := NewPollingConfig(src, JSONDecoder[testStruct](), PollOptions{})

	ctx, cancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)
	go func() { errCh <- p.RefreshOnce(ctx) }()

	cancel()
	select {
	case err := <-errCh:
		require.Error(t, err)
	case <-time.After(time.Second):
		t.Fatal("RefreshOnce did not honour ctx cancellation")
	}
	close(unblock)
}

// --------------------------------------------------------------------------
// Concurrent snapshot reads
// --------------------------------------------------------------------------

func TestPipelineConfig_ConcurrentSnapshotReads(t *testing.T) {
	data := []byte(`{"name":"concurrent","value":99}`)
	p := NewPollingConfig(staticSrc(data), JSONDecoder[testStruct](), PollOptions{})
	require.NoError(t, p.RefreshOnce(context.Background()))

	const goroutines = 50
	const iterations = 200
	var wg sync.WaitGroup
	wg.Add(goroutines * 2)

	for i := 0; i < goroutines; i++ {
		go func() {
			defer wg.Done()
			for range iterations {
				p.RefreshOnce(context.Background()) //nolint:errcheck
			}
		}()
	}
	for i := 0; i < goroutines; i++ {
		go func() {
			defer wg.Done()
			for range iterations {
				snap := p.Snapshot()
				assert.NotEmpty(t, snap.Name, "concurrent read saw empty Name")
			}
		}()
	}
	wg.Wait()
}

// --------------------------------------------------------------------------
// File source
// --------------------------------------------------------------------------

func TestNewFileConfig_ReadsFreshOnSwap(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "cfg.json")

	require.NoError(t, os.WriteFile(path, []byte(`{"name":"first","value":1}`), 0o600))
	p := NewFileConfig(path, JSONDecoder[testStruct](), PollOptions{})
	require.NoError(t, p.RefreshOnce(context.Background()))
	require.Equal(t, "first", p.Snapshot().Name)

	require.NoError(t, os.WriteFile(path, []byte(`{"name":"second","value":2}`), 0o600))
	require.NoError(t, p.RefreshOnce(context.Background()))
	require.Equal(t, "second", p.Snapshot().Name)
}

func TestNewFileConfig_MissingFileReturnsError(t *testing.T) {
	p := NewFileConfig("/no/such/file.json", JSONDecoder[testStruct](), PollOptions{})
	require.Error(t, p.RefreshOnce(context.Background()))
}

// --------------------------------------------------------------------------
// JSON / YAML decoders
// --------------------------------------------------------------------------

func TestJSONDecoder_RoundTrip(t *testing.T) {
	dec := JSONDecoder[testStruct]()
	got, err := dec([]byte(`{"name":"round","value":5}`))
	require.NoError(t, err)
	require.Equal(t, testStruct{Name: "round", Value: 5}, got)
}

func TestJSONDecoder_Error(t *testing.T) {
	dec := JSONDecoder[testStruct]()
	_, err := dec([]byte(`not json`))
	require.Error(t, err)
}

// --------------------------------------------------------------------------
// Checksum caching
// --------------------------------------------------------------------------

func TestRefreshOnce_ChecksumCachingSkipsRedundantDecodes(t *testing.T) {
	data := []byte(`{"name":"cached","value":42}`)
	var decodeCount atomic.Int32
	dec := ConfigDecoder[testStruct](func(b []byte) (testStruct, error) {
		decodeCount.Add(1)
		return JSONDecoder[testStruct]()(b)
	})
	p := NewPollingConfig(staticSrc(data), dec, PollOptions{})

	// First refresh decodes.
	require.NoError(t, p.RefreshOnce(context.Background()))
	require.Equal(t, int32(1), decodeCount.Load())

	// Second refresh with same data: decode skipped, count unchanged.
	require.NoError(t, p.RefreshOnce(context.Background()))
	require.Equal(t, int32(1), decodeCount.Load())

	// Different data: decode runs again.
	p2 := NewPollingConfig(staticSrc([]byte(`{"name":"new","value":99}`)), dec, PollOptions{})
	require.NoError(t, p2.RefreshOnce(context.Background()))
	require.Equal(t, int32(2), decodeCount.Load())
}

// --------------------------------------------------------------------------
// File watching
// --------------------------------------------------------------------------

func TestStartFileWatch_TriggersRefreshOnChange(t *testing.T) {
	tmpFile := filepath.Join(t.TempDir(), "config.json")
	data := []byte(`{"name":"watch","value":1}`)
	require.NoError(t, os.WriteFile(tmpFile, data, 0o644))

	var refreshCount atomic.Int32
	src := ConfigSource(func(ctx context.Context) ([]byte, error) {
		refreshCount.Add(1)
		return os.ReadFile(tmpFile)
	})

	var observedRefresh atomic.Bool
	opts := PollOptions{
		Interval: 10 * time.Second, // long interval; file watch should trigger sooner
		Observe: func(ev ConfigEvent) {
			if ev.Err == nil && ev.Version != "" {
				observedRefresh.Store(true)
			}
		},
	}
	p := NewPollingConfig(src, JSONDecoder[testStruct](), opts)
	require.NoError(t, p.RefreshOnce(context.Background()))

	// Start polling loop and file watching.
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	stopPoll := p.Start(ctx)
	defer stopPoll()

	stopWatch := StartFileWatch(p, tmpFile)
	defer stopWatch()

	// Clear the flag from initial Start refresh.
	time.Sleep(50 * time.Millisecond)
	observedRefresh.Store(false)

	// Modify file to trigger watcher.
	require.NoError(t, os.WriteFile(tmpFile, []byte(`{"name":"watch","value":2}`), 0o644))

	// Wait for watcher to detect and trigger refresh.
	deadline := time.Now().Add(500 * time.Millisecond)
	for time.Now().Before(deadline) {
		if observedRefresh.Load() {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	// Verify refresh was observed.
	require.True(t, observedRefresh.Load(), "file watch did not trigger refresh")
}

// --------------------------------------------------------------------------
// ModTime caching
// --------------------------------------------------------------------------

func TestNewFileConfig_CachesModTimeAndSize(t *testing.T) {
	tmpFile := filepath.Join(t.TempDir(), "config.json")
	data1 := []byte(`{"name":"test","value":1}`)
	require.NoError(t, os.WriteFile(tmpFile, data1, 0o644))

	p := NewFileConfig(tmpFile, JSONDecoder[testStruct](), PollOptions{})

	// First refresh succeeds and caches ModTime.
	require.NoError(t, p.RefreshOnce(context.Background()))
	snap1 := p.Snapshot()

	// Second refresh with same file (same ModTime) succeeds.
	// The source uses cached data instead of reading.
	require.NoError(t, p.RefreshOnce(context.Background()))
	snap2 := p.Snapshot()
	require.Equal(t, snap1, snap2)

	// Modify file: ModTime changes, source reads again.
	time.Sleep(10 * time.Millisecond)
	require.NoError(t, os.WriteFile(tmpFile, []byte(`{"name":"test","value":2}`), 0o644))
	require.NoError(t, p.RefreshOnce(context.Background()))
	snap3 := p.Snapshot()
	require.NotEqual(t, snap1.Value, snap3.Value)
}

func TestNewFileConfig_HandlesStatErrorGracefully(t *testing.T) {
	tmpFile := filepath.Join(t.TempDir(), "config.json")
	data := []byte(`{"name":"stat","value":99}`)
	require.NoError(t, os.WriteFile(tmpFile, data, 0o644))

	p := NewFileConfig(tmpFile, JSONDecoder[testStruct](), PollOptions{})
	require.NoError(t, p.RefreshOnce(context.Background()))

	// Delete the file: Stat fails on next refresh.
	require.NoError(t, os.Remove(tmpFile))

	// Refresh returns cached data instead of failing (graceful degradation).
	require.NoError(t, p.RefreshOnce(context.Background()))
	require.Equal(t, "stat", p.Snapshot().Name)
}
