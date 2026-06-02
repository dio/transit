package up

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// testStruct is a simple type for decoder round-trip tests.
type testStruct struct {
	Name  string `json:"name"`
	Value int    `json:"value"`
}

// errSource is a ConfigSource that always returns an error.
type errSource struct{ err error }

func (e *errSource) Fetch(_ context.Context) ([]byte, error) { return nil, e.err }

// errDecoder is a ConfigDecoder that always returns an error.
type errDecoder[T any] struct{ err error }

func (d errDecoder[T]) Decode(_ []byte) (T, error) {
	var zero T
	return zero, d.err
}

func TestPipelineConfig_InitialNoSnapshot(t *testing.T) {
	p := New(StaticSource([]byte(`{"name":"x","value":1}`)), JSONDecoder[testStruct]())
	_, ok := p.Snapshot()
	require.False(t, ok, "expected no snapshot before Refresh")
}

func TestPipelineConfig_NewStatic_ImmediateSnapshot(t *testing.T) {
	v := testStruct{Name: "static", Value: 42}
	p := NewStatic(v)
	snap, ok := p.Snapshot()
	require.True(t, ok, "expected snapshot to be immediately available from NewStatic")
	require.Equal(t, v, snap.Value)
	require.Equal(t, "static", snap.Version)
	require.False(t, snap.FetchedAt.IsZero(), "expected non-zero FetchedAt")
}

func TestPipelineConfig_Refresh_PublishesSnapshot(t *testing.T) {
	data := []byte(`{"name":"hello","value":7}`)
	p := New(StaticSource(data), JSONDecoder[testStruct]())

	require.NoError(t, p.Refresh(context.Background()))
	snap, ok := p.Snapshot()
	require.True(t, ok, "expected snapshot after Refresh")
	require.Equal(t, "hello", snap.Value.Name)
	require.Equal(t, 7, snap.Value.Value)
	require.False(t, snap.FetchedAt.IsZero(), "expected non-zero FetchedAt")
}

func TestPipelineConfig_FetchError_KeepsLastGood(t *testing.T) {
	data := []byte(`{"name":"good","value":1}`)

	// Use a switchable source for simplicity.
	sw := &switchableSource{current: StaticSource(data)}
	pc := New(sw, JSONDecoder[testStruct]())
	require.NoError(t, pc.Refresh(context.Background()))
	snapBefore, _ := pc.Snapshot()

	sw.setErr(errors.New("network down"))
	require.Error(t, pc.Refresh(context.Background()))

	snapAfter, ok := pc.Snapshot()
	require.True(t, ok, "expected last-good snapshot to remain after fetch error")
	require.Equal(t, snapBefore.Value, snapAfter.Value)
}

func TestPipelineConfig_DecodeError_KeepsLastGood(t *testing.T) {
	good := []byte(`{"name":"good","value":1}`)
	sw := &switchableSource{current: StaticSource(good)}
	sd := &switchableDecoder[testStruct]{current: JSONDecoder[testStruct]()}
	pc := New(sw, sd)

	require.NoError(t, pc.Refresh(context.Background()))
	snapBefore, _ := pc.Snapshot()

	sd.setErr(errors.New("bad schema"))
	require.Error(t, pc.Refresh(context.Background()))

	snapAfter, ok := pc.Snapshot()
	require.True(t, ok, "expected last-good snapshot to remain after decode error")
	require.Equal(t, snapBefore.Value, snapAfter.Value)
}

func TestPipelineConfig_ConcurrentRefreshRead(t *testing.T) {
	// Ensure readers never see a partial update and no races.
	data := []byte(`{"name":"concurrent","value":99}`)
	p := New(StaticSource(data), JSONDecoder[testStruct]())
	require.NoError(t, p.Refresh(context.Background()))

	const goroutines = 50
	const iterations = 200
	var wg sync.WaitGroup
	wg.Add(goroutines * 2)

	// Writers.
	for i := 0; i < goroutines; i++ {
		go func() {
			defer wg.Done()
			for j := 0; j < iterations; j++ {
				_ = p.Refresh(context.Background())
			}
		}()
	}
	// Readers.
	for i := 0; i < goroutines; i++ {
		go func() {
			defer wg.Done()
			for j := 0; j < iterations; j++ {
				snap, ok := p.Snapshot()
				if ok {
					// Use assert (not require) — require.FailNow panics in goroutines.
					assert.False(t, snap.FetchedAt.IsZero(), "FetchedAt is zero in concurrent snapshot")
				}
			}
		}()
	}
	wg.Wait()
}

func TestPipelineConfig_MustSnapshot_PanicsWhenEmpty(t *testing.T) {
	p := New(StaticSource([]byte(`{}`)), JSONDecoder[testStruct]())
	require.Panics(t, func() { p.MustSnapshot() })
}

func TestPipelineConfig_MustSnapshot_ReturnsValue(t *testing.T) {
	v := testStruct{Name: "must", Value: 3}
	p := NewStatic(v)
	require.Equal(t, v, p.MustSnapshot())
}

func TestJSONDecoder_RoundTrip(t *testing.T) {
	d := JSONDecoder[testStruct]()
	input := testStruct{Name: "round", Value: 5}
	data := fmt.Appendf(nil, `{"name":%q,"value":%d}`, input.Name, input.Value)
	got, err := d.Decode(data)
	require.NoError(t, err)
	require.Equal(t, input, got)
}

func TestJSONDecoder_Error(t *testing.T) {
	d := JSONDecoder[testStruct]()
	_, err := d.Decode([]byte(`not json`))
	require.Error(t, err)
}

func TestStaticSource_AlwaysReturnsData(t *testing.T) {
	data := []byte(`hello`)
	src := StaticSource(data)
	for range 5 {
		got, err := src.Fetch(context.Background())
		require.NoError(t, err)
		require.Equal(t, "hello", string(got))
	}
}

// switchableSource lets tests change the underlying source mid-test.
type switchableSource struct {
	mu      sync.Mutex
	current ConfigSource
	err     error
}

func (s *switchableSource) Fetch(ctx context.Context) ([]byte, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.err != nil {
		return nil, s.err
	}
	return s.current.Fetch(ctx)
}

func (s *switchableSource) setErr(err error) {
	s.mu.Lock()
	s.err = err
	s.mu.Unlock()
}

// switchableDecoder lets tests swap decoder behavior.
type switchableDecoder[T any] struct {
	mu      sync.Mutex
	current ConfigDecoder[T]
	err     error
}

func (d *switchableDecoder[T]) Decode(data []byte) (T, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.err != nil {
		var zero T
		return zero, d.err
	}
	return d.current.Decode(data)
}

func (d *switchableDecoder[T]) setErr(err error) {
	d.mu.Lock()
	d.err = err
	d.mu.Unlock()
}
