package up

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"sync/atomic"
	"time"
)

const (
	// DefaultPollInterval is used when PollOptions.Interval is zero.
	DefaultPollInterval = 30 * time.Second
	// DefaultPollTimeout is used when PollOptions.Timeout is zero.
	DefaultPollTimeout = 5 * time.Second
)

// ConfigDecoder[T] parses raw bytes into a typed snapshot.
// Called from the background polling goroutine; must be safe to call concurrently.
type ConfigDecoder[T any] func(data []byte) (T, error)

// ConfigSource fetches raw config bytes. May be called from a background goroutine.
// A non-nil error keeps the last-good snapshot.
type ConfigSource func(ctx context.Context) ([]byte, error)

// ConfigEvent carries diagnostics from one refresh cycle.
type ConfigEvent struct {
	Version  string        // opaque; hash or counter; empty on error
	Duration time.Duration // fetch+decode wall time
	Err      error         // non-nil means last-good snapshot was kept
}

// PollOptions configures the polling behaviour for NewPollingConfig / NewFileConfig.
type PollOptions struct {
	Interval time.Duration // zero → DefaultPollInterval
	Timeout  time.Duration // zero → DefaultPollTimeout; applied per fetch attempt
	Jitter   time.Duration // random [0, Jitter) added to each interval

	// Observe is called after every refresh cycle (success or failure).
	// Called from the polling goroutine; must not block indefinitely.
	Observe func(ConfigEvent)
}

// PipelineConfig[T] holds a decoded config snapshot refreshable from a source.
// Snapshot() is safe from any goroutine. The request path never calls the source.
type PipelineConfig[T any] struct {
	source      ConfigSource
	decoder     ConfigDecoder[T]
	opts        PollOptions
	current     atomic.Pointer[T]
	lastVersion atomic.Value // stores string; empty if no version yet
	isStatic    bool
	refreshNow  chan struct{}
}

// NewStaticConfig returns a PipelineConfig whose snapshot never changes.
// Start is a no-op. Snapshot() returns v immediately.
func NewStaticConfig[T any](v T) *PipelineConfig[T] {
	p := &PipelineConfig[T]{isStatic: true}
	vv := v
	p.current.Store(&vv)
	return p
}

// NewPollingConfig returns a PipelineConfig backed by an arbitrary source.
// The first fetch fires immediately on Start; subsequent fetches tick at Interval ± Jitter.
func NewPollingConfig[T any](src ConfigSource, dec ConfigDecoder[T], opts PollOptions) *PipelineConfig[T] {
	if opts.Interval == 0 {
		opts.Interval = DefaultPollInterval
	}
	if opts.Timeout == 0 {
		opts.Timeout = DefaultPollTimeout
	}
	return &PipelineConfig[T]{source: src, decoder: dec, opts: opts, refreshNow: make(chan struct{}, 1)}
}

// Snapshot returns the current decoded config. Returns the zero value if no
// successful fetch has completed yet. Never returns a partially-updated value.
func (p *PipelineConfig[T]) Snapshot() T {
	v := p.current.Load()
	if v == nil {
		var zero T
		return zero
	}
	return *v
}

// RefreshOnce fetches and decodes a single snapshot, blocking until done or
// ctx is cancelled. Updates Snapshot() on success; on error keeps last-good.
// Useful for warming the cache before Start() begins ticking.
func (p *PipelineConfig[T]) RefreshOnce(ctx context.Context) error {
	return p.fetchAndStore(ctx)
}

// SignalRefresh requests an immediate refresh (called by file watchers, etc).
// Non-blocking; ignored if a refresh is already pending.
func (p *PipelineConfig[T]) SignalRefresh() {
	select {
	case p.refreshNow <- struct{}{}:
	default:
	}
}

// fetchAndStore performs one fetch+decode cycle, atomically publishing on success.
// Calls Observe (if set) after every attempt.
func (p *PipelineConfig[T]) fetchAndStore(ctx context.Context) error {
	start := time.Now()
	data, err := p.source(ctx)
	if err != nil {
		if p.opts.Observe != nil {
			p.opts.Observe(ConfigEvent{Duration: time.Since(start), Err: err})
		}
		return err
	}

	sum := sha256.Sum256(data)
	version := hex.EncodeToString(sum[:8])

	// Skip decode if checksum hasn't changed.
	lastVer, _ := p.lastVersion.Load().(string)
	if lastVer == version {
		// Notify observer with empty version to indicate no change (skip logging).
		if p.opts.Observe != nil {
			p.opts.Observe(ConfigEvent{Version: "", Duration: time.Since(start)})
		}
		return nil
	}

	v, err := p.decoder(data)
	if err != nil {
		if p.opts.Observe != nil {
			p.opts.Observe(ConfigEvent{Duration: time.Since(start), Err: err})
		}
		return err
	}
	p.current.Store(&v)
	p.lastVersion.Store(version)
	if p.opts.Observe != nil {
		p.opts.Observe(ConfigEvent{Version: version, Duration: time.Since(start)})
	}
	return nil
}

// JSONDecoder[T] decodes JSON bytes into T using encoding/json.
func JSONDecoder[T any]() ConfigDecoder[T] {
	return func(data []byte) (T, error) {
		var v T
		if err := json.Unmarshal(data, &v); err != nil {
			return v, err
		}
		return v, nil
	}
}
