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
	var failDec atomic.Bool
	dec := ConfigDecoder[testStruct](func(data []byte) (testStruct, error) {
		if failDec.Load() {
			return testStruct{}, errors.New("bad schema")
		}
		return JSONDecoder[testStruct]()(data)
	})
	p := NewPollingConfig(staticSrc(good), dec, PollOptions{})
	require.NoError(t, p.RefreshOnce(context.Background()))
	good2 := p.Snapshot()

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
