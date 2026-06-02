package up

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// TestHostRefreshLoop_RefreshOnce_Success verifies RefreshOnce applies a snapshot.
func TestHostRefreshLoop_RefreshOnce_Success(t *testing.T) {
	h := &recordingHandle{}
	s := NewHostSet[string](h)

	snap := HostSnapshot[string]{
		"a": {Address: "127.0.0.1:8001"},
	}
	snapFn := func(_ context.Context) (HostSnapshot[string], error) { return snap, nil }

	l := NewHostRefreshLoop[string](h, s, snapFn, HostRefreshOptions{})
	err := l.RefreshOnce(context.Background())
	require.NoError(t, err)

	ptr, ok := s.Get("a")
	require.True(t, ok)
	require.NotNil(t, ptr)
}

// TestHostRefreshLoop_RefreshOnce_Error verifies that a snapshot error keeps the previous set.
func TestHostRefreshLoop_RefreshOnce_Error(t *testing.T) {
	h := &recordingHandle{}
	s := NewHostSet[string](h)

	// Pre-populate the host set.
	s.Apply(HostSnapshot[string]{"a": {Address: "10.0.0.1:80"}})
	oldPtr, _ := s.Get("a")

	snapFn := func(_ context.Context) (HostSnapshot[string], error) {
		return nil, errors.New("lookup failed")
	}

	l := NewHostRefreshLoop[string](h, s, snapFn, HostRefreshOptions{})
	err := l.RefreshOnce(context.Background())
	require.Error(t, err)

	// Host set must remain unchanged.
	ptr, ok := s.Get("a")
	require.True(t, ok)
	require.Equal(t, oldPtr, ptr)
}

// TestHostRefreshLoop_StopStopsTicker verifies that after Stop, no more applies run.
func TestHostRefreshLoop_StopStopsTicker(t *testing.T) {
	h := &recordingHandle{}
	s := NewHostSet[string](h)

	var callCount atomic.Int32
	snapFn := func(_ context.Context) (HostSnapshot[string], error) {
		callCount.Add(1)
		return HostSnapshot[string]{"a": {Address: "10.0.0.1:80"}}, nil
	}

	l := NewHostRefreshLoop[string](h, s, snapFn, HostRefreshOptions{
		Interval: 20 * time.Millisecond,
	})
	l.Start()
	time.Sleep(60 * time.Millisecond)
	l.Stop()

	countAfterStop := callCount.Load()
	time.Sleep(60 * time.Millisecond)

	require.Equal(t, countAfterStop, callCount.Load(), "no more calls after Stop")
}

// TestHostRefreshLoop_StopDrainsInFlightApply verifies Stop waits for a scheduled apply.
func TestHostRefreshLoop_StopDrainsInFlightApply(t *testing.T) {
	// Use a channel-gated Schedule to simulate a slow main thread.
	gate := make(chan struct{})
	var applied atomic.Bool

	h := &recordingHandle{
		scheduleFn: func(fn func()) {
			go func() {
				<-gate
				fn()
				applied.Store(true)
			}()
		},
	}
	s := NewHostSet[string](h)

	snapFn := func(_ context.Context) (HostSnapshot[string], error) {
		return HostSnapshot[string]{"a": {Address: "10.0.0.1:80"}}, nil
	}

	l := NewHostRefreshLoop[string](h, s, snapFn, HostRefreshOptions{})

	// Schedule one apply manually.
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	errCh := make(chan error, 1)
	go func() {
		errCh <- l.RefreshOnce(ctx)
	}()

	// Wait a moment then release the gate.
	time.Sleep(10 * time.Millisecond)
	close(gate)

	err := <-errCh
	require.NoError(t, err)
	require.True(t, applied.Load(), "apply must have run before RefreshOnce returned")
}

// TestHostRefreshLoop_Coalescing verifies a slow main thread does not accumulate applies.
func TestHostRefreshLoop_Coalescing(t *testing.T) {
	// Gate the schedule so applies queue up.
	gate := make(chan struct{})
	var scheduleCount atomic.Int32

	h := &recordingHandle{
		scheduleFn: func(fn func()) {
			scheduleCount.Add(1)
			go func() {
				<-gate
				fn()
			}()
		},
	}
	s := NewHostSet[string](h)

	var fetchCount atomic.Int32
	snapFn := func(_ context.Context) (HostSnapshot[string], error) {
		fetchCount.Add(1)
		return HostSnapshot[string]{"a": {Address: "10.0.0.1:80"}}, nil
	}

	l := NewHostRefreshLoop[string](h, s, snapFn, HostRefreshOptions{
		Interval: 10 * time.Millisecond,
	})
	l.Start()

	// Let several ticks fire while the gate is closed.
	time.Sleep(80 * time.Millisecond)

	// Only one apply should have been scheduled (coalesced).
	require.Equal(t, int32(1), scheduleCount.Load(), "only one apply should be scheduled while gate is closed")

	close(gate)
	l.Stop()
}

// TestHostRefreshLoop_ObserverSuccessEvent verifies the observer receives correct counts.
func TestHostRefreshLoop_ObserverSuccessEvent(t *testing.T) {
	h := &recordingHandle{}
	s := NewHostSet[string](h)

	var mu sync.Mutex
	var events []HostRefreshEvent

	obs := func(e HostRefreshEvent) {
		mu.Lock()
		events = append(events, e)
		mu.Unlock()
	}

	snapFn := func(_ context.Context) (HostSnapshot[string], error) {
		return HostSnapshot[string]{
			"a": {Address: "10.0.0.1:80"},
			"b": {Address: "10.0.0.2:80"},
		}, nil
	}

	l := NewHostRefreshLoop[string](h, s, snapFn, HostRefreshOptions{Observe: obs})
	err := l.RefreshOnce(context.Background())
	require.NoError(t, err)

	mu.Lock()
	defer mu.Unlock()
	require.Len(t, events, 1)
	e := events[0]
	require.NoError(t, e.Err)
	require.Equal(t, 2, e.Added)
	require.Equal(t, 0, e.Removed)
	require.Equal(t, 0, e.Unchanged)
}

// TestHostRefreshLoop_ObserverErrorEvent verifies the observer receives error event with zero counts.
func TestHostRefreshLoop_ObserverErrorEvent(t *testing.T) {
	h := &recordingHandle{}
	s := NewHostSet[string](h)

	var mu sync.Mutex
	var events []HostRefreshEvent

	obs := func(e HostRefreshEvent) {
		mu.Lock()
		events = append(events, e)
		mu.Unlock()
	}

	snapFn := func(_ context.Context) (HostSnapshot[string], error) {
		return nil, errors.New("network error")
	}

	l := NewHostRefreshLoop[string](h, s, snapFn, HostRefreshOptions{
		Observe:  obs,
		Interval: 10 * time.Millisecond,
	})
	l.Start()

	// Let a tick fire.
	require.Eventually(t, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(events) > 0
	}, 2*time.Second, 10*time.Millisecond)

	l.Stop()

	mu.Lock()
	defer mu.Unlock()
	e := events[0]
	require.Error(t, e.Err)
	require.Equal(t, 0, e.Added)
	require.Equal(t, 0, e.Removed)
	require.Equal(t, 0, e.Unchanged)
}

// TestHostRefreshLoop_ObserverUnchangedCount verifies Unchanged count when re-applying same hosts.
func TestHostRefreshLoop_ObserverUnchangedCount(t *testing.T) {
	h := &recordingHandle{}
	s := NewHostSet[string](h)

	snap := HostSnapshot[string]{
		"a": {Address: "10.0.0.1:80"},
	}

	// First apply — all added.
	s.Apply(snap)

	var mu sync.Mutex
	var events []HostRefreshEvent

	obs := func(e HostRefreshEvent) {
		mu.Lock()
		events = append(events, e)
		mu.Unlock()
	}

	// Second refresh with same snapshot — all unchanged.
	snapFn := func(_ context.Context) (HostSnapshot[string], error) { return snap, nil }
	l := NewHostRefreshLoop[string](h, s, snapFn, HostRefreshOptions{Observe: obs})
	err := l.RefreshOnce(context.Background())
	require.NoError(t, err)

	mu.Lock()
	defer mu.Unlock()
	require.Len(t, events, 1)
	e := events[0]
	require.Equal(t, 0, e.Added)
	require.Equal(t, 0, e.Removed)
	require.Equal(t, 1, e.Unchanged)
}

// TestHostRefreshLoop_RefreshOnce_CtxCancelled verifies RefreshOnce respects context cancellation.
func TestHostRefreshLoop_RefreshOnce_CtxCancelled(t *testing.T) {
	gate := make(chan struct{})
	h := &recordingHandle{
		scheduleFn: func(fn func()) {
			go func() {
				<-gate
				fn()
			}()
		},
	}
	s := NewHostSet[string](h)

	snapFn := func(_ context.Context) (HostSnapshot[string], error) {
		return HostSnapshot[string]{"a": {Address: "10.0.0.1:80"}}, nil
	}

	l := NewHostRefreshLoop[string](h, s, snapFn, HostRefreshOptions{})

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()

	err := l.RefreshOnce(ctx)
	require.ErrorIs(t, err, context.DeadlineExceeded)

	// Unblock any pending goroutines.
	close(gate)
}

// TestHostRefreshLoop_DefaultsApplied verifies that zero options use the defaults.
func TestHostRefreshLoop_DefaultsApplied(t *testing.T) {
	h := &recordingHandle{}
	s := NewHostSet[string](h)
	snapFn := func(_ context.Context) (HostSnapshot[string], error) { return nil, nil }

	l := NewHostRefreshLoop[string](h, s, snapFn, HostRefreshOptions{})

	require.Equal(t, DefaultRefreshInterval, l.interval)
	require.Equal(t, DefaultRefreshTimeout, l.timeout)
}
