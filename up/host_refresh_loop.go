package up

import (
	"context"
	"math/rand/v2"
	"sync"
	"time"
)

// DefaultRefreshInterval is the interval used when HostRefreshOptions.Interval is zero.
const DefaultRefreshInterval = 30 * time.Second

// DefaultRefreshTimeout is the per-fetch context timeout used when HostRefreshOptions.Timeout is zero.
const DefaultRefreshTimeout = time.Second

// HostSnapshotFunc fetches the current desired host snapshot. It may be called
// from a background goroutine. Return a non-nil error to keep the previous snapshot.
type HostSnapshotFunc[K comparable] func(ctx context.Context) (HostSnapshot[K], error)

// HostRefreshOptions configures a HostRefreshLoop.
type HostRefreshOptions struct {
	// Interval between refreshes. Zero uses DefaultRefreshInterval.
	Interval time.Duration
	// Timeout for each snapshot fetch. Zero uses DefaultRefreshTimeout.
	Timeout time.Duration
	// Jitter adds a random ±Jitter offset to each tick interval.
	Jitter time.Duration
	// Observe is called on the cluster main thread after each Apply.
	// Optional; nil disables the observer.
	Observe HostRefreshObserver
}

// HostRefreshEvent carries diagnostics from a single refresh cycle.
type HostRefreshEvent struct {
	// Duration covers the full fetch + apply round-trip.
	Duration time.Duration

	// Added, Removed, Unchanged count host changes for the cycle.
	// All three are zero when Err is non-nil.
	Added     int
	Removed   int
	Unchanged int

	// Err is non-nil when the snapshot fetch returned an error.
	Err error
}

// HostRefreshObserver is called on the cluster main thread after each Apply (or on error).
type HostRefreshObserver func(HostRefreshEvent)

// HostRefreshLoop periodically refreshes a HostSet from a user-supplied snapshot function.
type HostRefreshLoop[K comparable] struct {
	handle   ClusterHandle
	set      *HostSet[K]
	snapshot HostSnapshotFunc[K]
	interval time.Duration
	timeout  time.Duration
	jitter   time.Duration
	observe  HostRefreshObserver

	mu      sync.Mutex
	pending bool // true while a Schedule call is in-flight and not yet executed
	closed  bool // true after Stop; scheduled applies check this before running

	group *Group
}

// NewHostRefreshLoop creates a HostRefreshLoop. Call Start to begin ticking.
func NewHostRefreshLoop[K comparable](
	handle ClusterHandle,
	set *HostSet[K],
	snapshot HostSnapshotFunc[K],
	opts HostRefreshOptions,
) *HostRefreshLoop[K] {
	interval := opts.Interval
	if interval <= 0 {
		interval = DefaultRefreshInterval
	}
	timeout := opts.Timeout
	if timeout <= 0 {
		timeout = DefaultRefreshTimeout
	}
	return &HostRefreshLoop[K]{
		handle:   handle,
		set:      set,
		snapshot: snapshot,
		interval: interval,
		timeout:  timeout,
		jitter:   opts.Jitter,
		observe:  opts.Observe,
		group:    NewGroup(),
	}
}

// Start launches the background ticker goroutine. It does not perform an
// immediate refresh — call RefreshOnce first if a warm host set is needed.
func (l *HostRefreshLoop[K]) Start() {
	l.group.AddGoroutine(func(ctx context.Context) {
		t := time.NewTicker(l.tickInterval())
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				if l.jitter > 0 {
					t.Reset(l.tickInterval())
				}
				l.fetchAndSchedule(ctx, time.Now())
			}
		}
	})
	l.group.Start()
}

// Stop cancels the ticker, waits for any in-flight fetch to finish, and waits
// for any already-scheduled apply to complete before returning. After Stop
// returns no further Apply will run.
func (l *HostRefreshLoop[K]) Stop() {
	// Signal the scheduler to reject further applies after the in-flight one.
	l.mu.Lock()
	l.closed = true
	l.mu.Unlock()

	// Cancel the ticker goroutine and wait for it to exit.
	l.group.Stop()

	// Wait for any pending apply that was already dispatched via Schedule.
	// The scheduled closure holds mu while running; we acquire it here to
	// ensure it has finished before returning.
	l.mu.Lock()
	l.mu.Unlock() //nolint:staticcheck
}

// RefreshOnce fetches a snapshot and schedules an apply via ClusterHandle.Schedule,
// blocking until the apply has run on the main thread (or until ctx is cancelled).
// Returns the snapshot fetch error, if any; on error the apply is not scheduled.
func (l *HostRefreshLoop[K]) RefreshOnce(ctx context.Context) error {
	st := time.Now()

	fetchCtx, cancel := context.WithTimeout(ctx, l.timeout)
	defer cancel()

	snap, err := l.snapshot(fetchCtx)
	if err != nil {
		if l.observe != nil {
			event := HostRefreshEvent{Duration: time.Since(st), Err: err}
			done := make(chan struct{})
			l.handle.Schedule(func() {
				defer close(done)
				l.mu.Lock()
				closed := l.closed
				l.mu.Unlock()
				if !closed {
					l.observe(event)
				}
			})
			select {
			case <-done:
			case <-ctx.Done():
			}
		}
		return err
	}

	done := make(chan struct{})
	l.handle.Schedule(func() {
		defer close(done)
		l.mu.Lock()
		defer l.mu.Unlock()
		if l.closed {
			return
		}
		prev := l.set.Current()
		l.set.Apply(snap)
		if l.observe != nil {
			next := l.set.Current()
			added, removed, unchanged := diffCounts(prev, next, snap)
			l.observe(HostRefreshEvent{
				Duration:  time.Since(st),
				Added:     added,
				Removed:   removed,
				Unchanged: unchanged,
			})
		}
	})

	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// tickInterval returns the base interval plus a random jitter offset.
func (l *HostRefreshLoop[K]) tickInterval() time.Duration {
	if l.jitter == 0 {
		return l.interval
	}
	// ±jitter: random value in [-jitter, +jitter].
	offset := time.Duration(rand.Int64N(int64(2*l.jitter+1))) - l.jitter
	d := l.interval + offset
	if d <= 0 {
		d = time.Millisecond // floor; prevents zero or negative tickers
	}
	return d
}

// fetchAndSchedule runs the snapshot fetch and, on success, schedules Apply on
// the main thread. Uses coalescing to skip scheduling if an apply is pending.
func (l *HostRefreshLoop[K]) fetchAndSchedule(ctx context.Context, fetchStart time.Time) {
	fetchCtx, cancel := context.WithTimeout(ctx, l.timeout)
	defer cancel()

	snap, err := l.snapshot(fetchCtx)

	if err != nil {
		if l.observe != nil {
			event := HostRefreshEvent{Duration: time.Since(fetchStart), Err: err}
			l.handle.Schedule(func() {
				l.mu.Lock()
				closed := l.closed
				l.mu.Unlock()
				if !closed {
					l.observe(event)
				}
			})
		}
		return
	}

	// Coalescing: skip if an apply is already pending.
	l.mu.Lock()
	if l.pending || l.closed {
		l.mu.Unlock()
		return
	}
	l.pending = true
	l.mu.Unlock()

	capturedStart := fetchStart
	l.handle.Schedule(func() {
		l.mu.Lock()
		defer l.mu.Unlock()
		l.pending = false
		if l.closed {
			return
		}

		prev := l.set.Current()
		l.set.Apply(snap)

		if l.observe != nil {
			next := l.set.Current()
			added, removed, unchanged := diffCounts(prev, next, snap)
			l.observe(HostRefreshEvent{
				Duration:  time.Since(capturedStart),
				Added:     added,
				Removed:   removed,
				Unchanged: unchanged,
			})
		}
	})
}

// diffCounts computes Added/Removed/Unchanged for an observer event.
// prev is the snapshot before Apply, next is the snapshot after Apply, desired is the new snapshot.
func diffCounts[K comparable](prev, next map[K]HostEntry, desired HostSnapshot[K]) (added, removed, unchanged int) {
	for k := range desired {
		pe, wasThere := prev[k]
		ne, isThere := next[k]
		switch {
		case !wasThere && isThere:
			added++
		case wasThere && isThere && ne.Host == pe.Host:
			unchanged++
		case wasThere && isThere && ne.Host != pe.Host:
			// Spec changed — counted as one add (the remove is implicit).
			added++
		}
	}
	for k := range prev {
		if _, ok := next[k]; !ok {
			removed++
		}
	}
	return
}
