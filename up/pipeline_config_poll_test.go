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

	"github.com/stretchr/testify/require"
)

// --------------------------------------------------------------------------
// Polling: observer
// --------------------------------------------------------------------------

func TestPolling_ObserverFiresOnSuccess(t *testing.T) {
	data := []byte(`{"name":"obs","value":1}`)
	var mu sync.Mutex
	var events []ConfigEvent
	observe := func(ev ConfigEvent) {
		mu.Lock()
		events = append(events, ev)
		mu.Unlock()
	}
	p := NewPollingConfig(staticSrc(data), JSONDecoder[testStruct](), PollOptions{
		Interval: time.Hour, // only fire immediately
		Observe:  observe,
	})
	stop := p.Start(context.Background())
	defer stop()

	// Wait for immediate first fetch.
	require.Eventually(t, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(events) > 0
	}, time.Second, 5*time.Millisecond)
	stop()

	mu.Lock()
	ev := events[0]
	mu.Unlock()

	require.NoError(t, ev.Err)
	require.NotEmpty(t, ev.Version)
	require.Positive(t, ev.Duration)
}

func TestPolling_ObserverFiresOnError(t *testing.T) {
	fetchErr := errors.New("sentinel fetch error")
	var mu sync.Mutex
	var events []ConfigEvent
	p := NewPollingConfig(errSrc(fetchErr), JSONDecoder[testStruct](), PollOptions{
		Interval: time.Hour,
		Observe: func(ev ConfigEvent) {
			mu.Lock()
			events = append(events, ev)
			mu.Unlock()
		},
	})
	stop := p.Start(context.Background())
	defer stop()

	require.Eventually(t, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(events) > 0
	}, time.Second, 5*time.Millisecond)
	stop()

	mu.Lock()
	ev := events[0]
	mu.Unlock()

	require.Error(t, ev.Err)
	require.Empty(t, ev.Version)
}

// --------------------------------------------------------------------------
// Polling: Stop waits for in-flight fetch
// --------------------------------------------------------------------------

func TestPolling_StopWaitsForInFlight(t *testing.T) {
	fetchStarted := make(chan struct{}, 1)
	unblock := make(chan struct{})

	// The source blocks on unblock ignoring ctx; this simulates a non-cancellable
	// operation (e.g. a slow disk read) to verify Stop() waits for completion.
	src := ConfigSource(func(_ context.Context) ([]byte, error) {
		select {
		case fetchStarted <- struct{}{}:
		default:
		}
		<-unblock // released by test; not context-sensitive
		return []byte(`{"name":"x","value":1}`), nil
	})

	p := NewPollingConfig(src, JSONDecoder[testStruct](), PollOptions{
		Interval: time.Hour, // only the immediate fetch fires
		Timeout:  time.Hour, // long timeout so per-attempt ctx stays alive
	})
	stop := p.Start(context.Background())

	// Wait for the goroutine to enter the source.
	<-fetchStarted

	stopDone := make(chan struct{})
	go func() {
		stop()
		close(stopDone)
	}()

	// Stop() must be blocked while the fetch is in progress.
	select {
	case <-stopDone:
		t.Fatal("Stop() returned before fetch completed")
	case <-time.After(30 * time.Millisecond):
	}

	close(unblock) // allow fetch to finish

	select {
	case <-stopDone:
	case <-time.After(2 * time.Second):
		t.Fatal("Stop() did not return after fetch completed")
	}
}

// --------------------------------------------------------------------------
// Polling: jitter varies the tick interval
// --------------------------------------------------------------------------

func TestPolling_JitterVariesInterval(t *testing.T) {
	data := []byte(`{"name":"jitter","value":0}`)

	var mu sync.Mutex
	var times []time.Time
	src := ConfigSource(func(_ context.Context) ([]byte, error) {
		mu.Lock()
		times = append(times, time.Now())
		mu.Unlock()
		return data, nil
	})

	p := NewPollingConfig(src, JSONDecoder[testStruct](), PollOptions{
		Interval: 20 * time.Millisecond,
		Jitter:   15 * time.Millisecond,
	})
	stop := p.Start(context.Background())
	defer stop()

	// Collect at least 5 fetch timestamps (1 immediate + 4 ticks).
	require.Eventually(t, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(times) >= 5
	}, 5*time.Second, 5*time.Millisecond)
	stop()

	mu.Lock()
	captured := append([]time.Time(nil), times...)
	mu.Unlock()

	// Compute consecutive gaps; skip gap[0] (immediate → first tick).
	var gaps []time.Duration
	for i := 2; i < len(captured); i++ {
		gaps = append(gaps, captured[i].Sub(captured[i-1]))
	}
	require.GreaterOrEqual(t, len(gaps), 2, "need at least 2 gaps to compare")

	// At least two of the gaps must differ by more than 1ms (jitter randomises the interval).
	distinct := false
	for i := 1; i < len(gaps); i++ {
		diff := gaps[i] - gaps[i-1]
		if diff < 0 {
			diff = -diff
		}
		if diff > time.Millisecond {
			distinct = true
			break
		}
	}
	require.True(t, distinct, "jitter produced no variation in tick intervals: %v", gaps)
}

// --------------------------------------------------------------------------
// Polling: repeated refreshes accumulate correctly
// --------------------------------------------------------------------------

func TestPolling_RepeatedRefreshes(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "cfg.json")
	require.NoError(t, os.WriteFile(path, []byte(`{"name":"poll","value":7}`), 0o600))

	var count int32
	p := NewFileConfig(path, JSONDecoder[testStruct](), PollOptions{
		Interval: 20 * time.Millisecond,
		Observe: func(ev ConfigEvent) {
			if ev.Err == nil {
				atomic.AddInt32(&count, 1)
			}
		},
	})

	ctx, cancel := context.WithCancel(context.Background())
	stop := p.Start(ctx)
	defer stop()

	require.Eventually(t, func() bool {
		return atomic.LoadInt32(&count) >= 3
	}, 2*time.Second, 5*time.Millisecond)
	cancel()
	stop()
}
