package up

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// TestStreamPromise_unresolvedResult: Result returns (zero, false) before Resolve.
func TestStreamPromise_unresolvedResult(t *testing.T) {
	p := NewStreamPromise[string]()
	v, ok := p.Result()
	require.False(t, ok, "Result() returned true before Resolve")
	require.Empty(t, v)
}

// TestStreamPromise_resolveResult: Result returns (v, true) after Resolve.
func TestStreamPromise_resolveResult(t *testing.T) {
	p := NewStreamPromise[int]()
	p.Resolve(42)

	v, ok := p.Result()
	require.True(t, ok, "Result() returned false after Resolve")
	require.Equal(t, 42, v)
}

// TestStreamPromise_firstResolveWins: second Resolve returns false and does not
// change the stored value.
func TestStreamPromise_firstResolveWins(t *testing.T) {
	p := NewStreamPromise[string]()

	first := p.Resolve("alpha")
	second := p.Resolve("beta")

	require.True(t, first, "first Resolve returned false")
	require.False(t, second, "second Resolve returned true")

	v, ok := p.Result()
	require.True(t, ok)
	require.Equal(t, "alpha", v)
}

// TestStreamPromise_doneClosedOnResolve: Done() channel is closed after Resolve.
func TestStreamPromise_doneClosedOnResolve(t *testing.T) {
	p := NewStreamPromise[bool]()

	// Done should not be closed yet.
	select {
	case <-p.Done():
		require.Fail(t, "Done() channel closed before Resolve")
	default:
	}

	p.Resolve(true)

	select {
	case <-p.Done():
		// expected
	case <-time.After(time.Second):
		require.Fail(t, "Done() channel not closed after Resolve")
	}
}

// TestStreamPromise_callbackBeforeResolve: callback registered before Resolve
// fires after Resolve.
func TestStreamPromise_callbackBeforeResolve(t *testing.T) {
	p := NewStreamPromise[int]()

	var fired atomic.Bool
	var gotVal int
	p.OnResolve(func(v int) {
		gotVal = v
		fired.Store(true)
	})

	p.Resolve(7)

	require.True(t, fired.Load(), "callback did not fire after Resolve")
	require.Equal(t, 7, gotVal)
}

// TestStreamPromise_callbackAfterResolve: callback registered after Resolve
// fires inline (synchronously inside OnResolve).
func TestStreamPromise_callbackAfterResolve(t *testing.T) {
	p := NewStreamPromise[string]()
	p.Resolve("hello")

	var fired bool
	p.OnResolve(func(v string) {
		fired = true
		require.Equal(t, "hello", v)
	})
	// Because the promise was already resolved, the callback must have fired
	// synchronously inside OnResolve — before we reach the check below.
	require.True(t, fired, "callback did not fire inline for already-resolved promise")
}

// TestStreamPromise_cancelBeforeResolve: cancel before Resolve prevents callback.
func TestStreamPromise_cancelBeforeResolve(t *testing.T) {
	p := NewStreamPromise[int]()

	var fired atomic.Bool
	cancel := p.OnResolve(func(_ int) {
		fired.Store(true)
	})

	cancel()
	p.Resolve(1)

	require.False(t, fired.Load(), "canceled callback fired after Resolve")
}

// TestStreamPromise_cancelAfterResolve: cancel after Resolve is a no-op and
// does not panic.
func TestStreamPromise_cancelAfterResolve(t *testing.T) {
	p := NewStreamPromise[int]()

	var fired atomic.Bool
	cancel := p.OnResolve(func(_ int) {
		fired.Store(true)
	})

	p.Resolve(2)
	require.True(t, fired.Load(), "callback did not fire on Resolve")

	// Calling cancel after the callback already fired must not panic.
	cancel()
	cancel() // idempotent
}

// TestStreamPromise_multipleCallbacks: all registered callbacks fire; individual
// cancels work independently.
func TestStreamPromise_multipleCallbacks(t *testing.T) {
	p := NewStreamPromise[int]()

	var (
		fired [3]atomic.Bool
	)

	p.OnResolve(func(v int) { fired[0].Store(true) })
	cancelB := p.OnResolve(func(v int) { fired[1].Store(true) })
	p.OnResolve(func(v int) { fired[2].Store(true) })

	// Cancel B before resolve; A and C should still fire.
	cancelB()

	p.Resolve(10)

	require.True(t, fired[0].Load(), "callback A did not fire")
	require.False(t, fired[1].Load(), "canceled callback B fired")
	require.True(t, fired[2].Load(), "callback C did not fire")
}

// TestStreamPromise_concurrentResolveSafe: concurrent Resolve + OnResolve must
// not race. Run with -race.
func TestStreamPromise_concurrentResolveSafe(t *testing.T) {
	const goroutines = 64
	const iterations = 50

	for iter := 0; iter < iterations; iter++ {
		p := NewStreamPromise[int]()

		var wg sync.WaitGroup
		var resolved atomic.Int64

		// Multiple goroutines attempt to Resolve concurrently.
		for i := 0; i < goroutines/2; i++ {
			wg.Add(1)
			go func(v int) {
				defer wg.Done()
				if p.Resolve(v) {
					resolved.Add(1)
				}
			}(i)
		}

		// Multiple goroutines register callbacks concurrently.
		for i := 0; i < goroutines/2; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				cancel := p.OnResolve(func(_ int) {})
				// Randomly cancel some of them.
				cancel()
			}()
		}

		wg.Wait()

		// Exactly one Resolve must have won.
		require.EqualValues(t, 1, resolved.Load(), "iter %d: wrong number of winners", iter)

		// Done must be closed.
		select {
		case <-p.Done():
		case <-time.After(time.Second):
			require.Fail(t, "Done() not closed after concurrent Resolve", "iter %d", iter)
		}
	}
}

// TestStreamPromise_cancelIdempotent: calling the returned cancel multiple times
// does not panic.
func TestStreamPromise_cancelIdempotent(t *testing.T) {
	p := NewStreamPromise[struct{}]()
	cancel := p.OnResolve(func(_ struct{}) {})
	cancel()
	cancel()
	cancel()
	p.Resolve(struct{}{})
}
