package up

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
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
	p := New[testStruct](StaticSource([]byte(`{"name":"x","value":1}`)), JSONDecoder[testStruct]())
	_, ok := p.Snapshot()
	if ok {
		t.Fatal("expected no snapshot before Refresh")
	}
}

func TestPipelineConfig_NewStatic_ImmediateSnapshot(t *testing.T) {
	v := testStruct{Name: "static", Value: 42}
	p := NewStatic(v)
	snap, ok := p.Snapshot()
	if !ok {
		t.Fatal("expected snapshot to be immediately available from NewStatic")
	}
	if snap.Value != v {
		t.Fatalf("unexpected value: got %+v want %+v", snap.Value, v)
	}
	if snap.Version != "static" {
		t.Fatalf("expected version 'static', got %q", snap.Version)
	}
	if snap.FetchedAt.IsZero() {
		t.Fatal("expected non-zero FetchedAt")
	}
}

func TestPipelineConfig_Refresh_PublishesSnapshot(t *testing.T) {
	data := []byte(`{"name":"hello","value":7}`)
	p := New[testStruct](StaticSource(data), JSONDecoder[testStruct]())

	if err := p.Refresh(context.Background()); err != nil {
		t.Fatalf("unexpected Refresh error: %v", err)
	}
	snap, ok := p.Snapshot()
	if !ok {
		t.Fatal("expected snapshot after Refresh")
	}
	if snap.Value.Name != "hello" || snap.Value.Value != 7 {
		t.Fatalf("unexpected snapshot value: %+v", snap.Value)
	}
	if snap.FetchedAt.IsZero() {
		t.Fatal("expected non-zero FetchedAt")
	}
}

func TestPipelineConfig_FetchError_KeepsLastGood(t *testing.T) {
	// First, load a good snapshot.
	data := []byte(`{"name":"good","value":1}`)
	p := New[testStruct](StaticSource(data), JSONDecoder[testStruct]())
	if err := p.Refresh(context.Background()); err != nil {
		t.Fatalf("initial Refresh error: %v", err)
	}

	// Now swap to an erroring source by creating a new config using errSource.
	p2 := New[testStruct](&errSource{err: errors.New("network down")}, JSONDecoder[testStruct]())
	// Manually seed a good snapshot via NewStatic trick: store same holder.
	// Actually, use a two-source pattern: use the first p's snapshot as reference,
	// then build a second PipelineConfig with error source but pre-seeded.
	// Simpler: just test with p and a configurable source.

	// Use a switchable source for simplicity.
	sw := &switchableSource{current: StaticSource(data)}
	pc := New[testStruct](sw, JSONDecoder[testStruct]())
	if err := pc.Refresh(context.Background()); err != nil {
		t.Fatalf("initial Refresh error: %v", err)
	}
	snapBefore, _ := pc.Snapshot()

	sw.setErr(errors.New("network down"))
	err := pc.Refresh(context.Background())
	if err == nil {
		t.Fatal("expected error from Refresh with failing source")
	}

	snapAfter, ok := pc.Snapshot()
	if !ok {
		t.Fatal("expected last-good snapshot to remain after fetch error")
	}
	if snapAfter.Value != snapBefore.Value {
		t.Fatalf("last-good value changed: got %+v want %+v", snapAfter.Value, snapBefore.Value)
	}

	// suppress unused warning
	_ = p2
}

func TestPipelineConfig_DecodeError_KeepsLastGood(t *testing.T) {
	good := []byte(`{"name":"good","value":1}`)
	sw := &switchableSource{current: StaticSource(good)}
	sd := &switchableDecoder[testStruct]{current: JSONDecoder[testStruct]()}
	pc := New[testStruct](sw, sd)

	if err := pc.Refresh(context.Background()); err != nil {
		t.Fatalf("initial Refresh error: %v", err)
	}
	snapBefore, _ := pc.Snapshot()

	sd.setErr(errors.New("bad schema"))
	err := pc.Refresh(context.Background())
	if err == nil {
		t.Fatal("expected decode error")
	}

	snapAfter, ok := pc.Snapshot()
	if !ok {
		t.Fatal("expected last-good snapshot to remain after decode error")
	}
	if snapAfter.Value != snapBefore.Value {
		t.Fatalf("last-good value changed: got %+v want %+v", snapAfter.Value, snapBefore.Value)
	}
}

func TestPipelineConfig_ConcurrentRefreshRead(t *testing.T) {
	// Ensure readers never see a partial update and no races.
	data := []byte(`{"name":"concurrent","value":99}`)
	p := New[testStruct](StaticSource(data), JSONDecoder[testStruct]())
	// Pre-load so readers don't all see (zero, false).
	if err := p.Refresh(context.Background()); err != nil {
		t.Fatalf("initial Refresh: %v", err)
	}

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
					// Snapshot must be consistent (both fields set together).
					if snap.FetchedAt.IsZero() {
						t.Errorf("FetchedAt is zero in concurrent snapshot")
					}
				}
			}
		}()
	}
	wg.Wait()
}

func TestPipelineConfig_MustSnapshot_PanicsWhenEmpty(t *testing.T) {
	p := New[testStruct](StaticSource([]byte(`{}`)), JSONDecoder[testStruct]())
	defer func() {
		if r := recover(); r == nil {
			t.Fatal("expected panic from MustSnapshot when no snapshot loaded")
		}
	}()
	p.MustSnapshot()
}

func TestPipelineConfig_MustSnapshot_ReturnsValue(t *testing.T) {
	v := testStruct{Name: "must", Value: 3}
	p := NewStatic(v)
	got := p.MustSnapshot()
	if got != v {
		t.Fatalf("MustSnapshot returned %+v, want %+v", got, v)
	}
}

func TestJSONDecoder_RoundTrip(t *testing.T) {
	d := JSONDecoder[testStruct]()
	input := testStruct{Name: "round", Value: 5}
	data := []byte(fmt.Sprintf(`{"name":%q,"value":%d}`, input.Name, input.Value))
	got, err := d.Decode(data)
	if err != nil {
		t.Fatalf("Decode error: %v", err)
	}
	if got != input {
		t.Fatalf("round-trip mismatch: got %+v want %+v", got, input)
	}
}

func TestJSONDecoder_Error(t *testing.T) {
	d := JSONDecoder[testStruct]()
	_, err := d.Decode([]byte(`not json`))
	if err == nil {
		t.Fatal("expected error for invalid JSON")
	}
}

func TestStaticSource_AlwaysReturnsData(t *testing.T) {
	data := []byte(`hello`)
	src := StaticSource(data)
	for i := 0; i < 5; i++ {
		got, err := src.Fetch(context.Background())
		if err != nil {
			t.Fatalf("Fetch error: %v", err)
		}
		if string(got) != "hello" {
			t.Fatalf("unexpected data: %s", got)
		}
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
