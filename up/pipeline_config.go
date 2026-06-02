package up

import (
	"context"
	"encoding/json"
	"fmt"
	"sync/atomic"
	"time"
)

// ConfigSource fetches raw config bytes from an external source.
// Fetch must be safe to call concurrently.
type ConfigSource interface {
	Fetch(ctx context.Context) ([]byte, error)
}

// ConfigDecoder[T] decodes raw bytes into a typed snapshot.
type ConfigDecoder[T any] interface {
	Decode(data []byte) (T, error)
}

// Snapshot[T] is an immutable config snapshot with provenance metadata.
type Snapshot[T any] struct {
	Value     T
	Version   string    // opaque; source may set to checksum, etag, seq, etc.
	FetchedAt time.Time
}

// PipelineConfig[T] holds an atomically published config snapshot.
// Last-good semantics: if Refresh fails the previous snapshot remains visible.
// Safe for concurrent use.
type PipelineConfig[T any] struct {
	source  ConfigSource
	decoder ConfigDecoder[T]
	// stored value is *snapshotHolder[T]; nil means no snapshot yet.
	current atomic.Pointer[snapshotHolder[T]]
}

// snapshotHolder wraps a Snapshot so we can store *snapshotHolder in atomic.Pointer.
type snapshotHolder[T any] struct {
	snap Snapshot[T]
}

// New creates a PipelineConfig that fetches from source and decodes with decoder.
// The snapshot is NOT loaded until the first call to Refresh or Load.
func New[T any](source ConfigSource, decoder ConfigDecoder[T]) *PipelineConfig[T] {
	return &PipelineConfig[T]{
		source:  source,
		decoder: decoder,
	}
}

// Refresh fetches and decodes a new snapshot. On success it atomically replaces
// the current snapshot. On failure the previous snapshot is unchanged and the
// error is returned. Safe to call concurrently.
func (p *PipelineConfig[T]) Refresh(ctx context.Context) error {
	data, err := p.source.Fetch(ctx)
	if err != nil {
		return fmt.Errorf("pipeline_config: fetch: %w", err)
	}
	value, err := p.decoder.Decode(data)
	if err != nil {
		return fmt.Errorf("pipeline_config: decode: %w", err)
	}
	holder := &snapshotHolder[T]{
		snap: Snapshot[T]{
			Value:     value,
			FetchedAt: time.Now(),
		},
	}
	p.current.Store(holder)
	return nil
}

// Snapshot returns (snapshot, true) if at least one successful Refresh has
// completed, or (zero, false) otherwise.
func (p *PipelineConfig[T]) Snapshot() (Snapshot[T], bool) {
	h := p.current.Load()
	if h == nil {
		var zero Snapshot[T]
		return zero, false
	}
	return h.snap, true
}

// MustSnapshot returns the current snapshot value, panicking if no snapshot
// has been loaded yet. Suitable for call sites that load config at startup.
func (p *PipelineConfig[T]) MustSnapshot() T {
	snap, ok := p.Snapshot()
	if !ok {
		panic("pipeline_config: no snapshot loaded")
	}
	return snap.Value
}

// StaticSource returns a ConfigSource that always returns the given bytes.
// Useful for tests and for embedding config literals in code.
func StaticSource(data []byte) ConfigSource {
	cp := make([]byte, len(data))
	copy(cp, data)
	return &staticSource{data: cp}
}

type staticSource struct {
	data []byte
}

func (s *staticSource) Fetch(_ context.Context) ([]byte, error) {
	return s.data, nil
}

// NewStatic creates a PipelineConfig[T] pre-loaded with value v using a static
// source. Snapshot() immediately returns (snap, true) without calling Refresh.
func NewStatic[T any](v T) *PipelineConfig[T] {
	p := &PipelineConfig[T]{}
	holder := &snapshotHolder[T]{
		snap: Snapshot[T]{
			Value:     v,
			Version:   "static",
			FetchedAt: time.Now(),
		},
	}
	p.current.Store(holder)
	return p
}

// jsonDecoder is the implementation of ConfigDecoder[T] for JSON.
type jsonDecoder[T any] struct{}

func (d jsonDecoder[T]) Decode(data []byte) (T, error) {
	var v T
	if err := json.Unmarshal(data, &v); err != nil {
		return v, err
	}
	return v, nil
}

// JSONDecoder[T] decodes JSON bytes into T using encoding/json.
func JSONDecoder[T any]() ConfigDecoder[T] {
	return jsonDecoder[T]{}
}
