package up

import (
	"sync"
	"sync/atomic"
)

// StreamPromise[T] is a resolve-once promise. The zero value is unusable;
// construct with NewStreamPromise.
//
// Concurrency model:
//   - mu guards resolved, value, and callbacks.
//   - done is a channel closed on first Resolve; callers may wait on it.
//   - resolvedOnce ensures close(done) happens exactly once even under concurrent
//     Resolve calls racing with OnResolve.
type StreamPromise[T any] struct {
	mu       sync.Mutex
	resolved bool
	value    T

	// done is closed when the promise is resolved. Allocated once in
	// NewStreamPromise so callers can safely select on Done() before Resolve.
	done chan struct{}

	// resolvedOnce protects close(done).
	resolvedOnce atomic.Bool

	// callbacks is the list of pending (not yet canceled) OnResolve registrations.
	// Each entry is a pointer to a promiseCB so that cancel() can nil it in place.
	callbacks []*promiseCB[T]
}

// promiseCB holds one registered callback and its canceled flag.
type promiseCB[T any] struct {
	mu       sync.Mutex
	cb       func(T) // nil when canceled
	canceled bool
}

// NewStreamPromise returns a new, unresolved StreamPromise[T].
func NewStreamPromise[T any]() *StreamPromise[T] {
	return &StreamPromise[T]{
		done: make(chan struct{}),
	}
}

// Resolve publishes v. The first call wins and returns true; subsequent calls
// are no-ops and return false.
//
// All registered OnResolve callbacks that have not been canceled are fired
// synchronously from the goroutine that calls Resolve, in registration order.
func (p *StreamPromise[T]) Resolve(v T) bool {
	p.mu.Lock()
	if p.resolved {
		p.mu.Unlock()
		return false
	}
	p.resolved = true
	p.value = v

	// Drain the callback slice while still holding the lock so that a
	// concurrent OnResolve registered after we set p.resolved but before we
	// read the slice is handled correctly (it will fire inline in OnResolve).
	cbs := p.callbacks
	p.callbacks = nil
	p.mu.Unlock()

	// Close done exactly once.
	if p.resolvedOnce.CompareAndSwap(false, true) {
		close(p.done)
	}

	// Fire callbacks outside the promise lock to avoid deadlock if a callback
	// re-enters the promise (e.g. calls OnResolve again).
	for _, entry := range cbs {
		entry.mu.Lock()
		fn := entry.cb
		if !entry.canceled && fn != nil {
			entry.canceled = true // mark fired so cancel is a no-op
			entry.mu.Unlock()
			fn(v)
		} else {
			entry.mu.Unlock()
		}
	}
	return true
}

// Done returns a channel that is closed when the promise is resolved.
// Safe to use in a select.
func (p *StreamPromise[T]) Done() <-chan struct{} {
	return p.done
}

// Result returns (v, true) if the promise is resolved, or (zero, false) if not.
func (p *StreamPromise[T]) Result() (T, bool) {
	p.mu.Lock()
	resolved := p.resolved
	v := p.value
	p.mu.Unlock()
	if !resolved {
		var zero T
		return zero, false
	}
	return v, true
}

// OnResolve registers cb to fire when the promise is resolved.
// If the promise is already resolved, cb fires synchronously before OnResolve
// returns.
//
// Returns a cancel func. Calling cancel prevents cb from firing if it has not
// fired yet; cancel is idempotent and safe to call multiple times.
func (p *StreamPromise[T]) OnResolve(cb func(T)) (cancel func()) {
	entry := &promiseCB[T]{cb: cb}

	p.mu.Lock()
	if p.resolved {
		v := p.value
		p.mu.Unlock()
		// Fire inline; cancel is a no-op because cb already ran.
		cb(v)
		return func() {}
	}
	p.callbacks = append(p.callbacks, entry)
	p.mu.Unlock()

	return func() {
		entry.mu.Lock()
		entry.canceled = true
		entry.cb = nil
		entry.mu.Unlock()
	}
}
