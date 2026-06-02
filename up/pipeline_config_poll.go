package up

import (
	"context"
	"time"
)

// RefreshEvent is passed to observer callbacks on each refresh attempt.
type RefreshEvent struct {
	Version  string        // new snapshot version, empty on error
	Duration time.Duration // how long the fetch+decode took
	Err      error         // nil on success
}

// RefreshObserver is called after every Refresh attempt (success or failure).
type RefreshObserver func(RefreshEvent)

// WithObserver adds an observer called after every Refresh attempt.
// Must be called before StartPolling. Safe to call multiple times.
func (p *PipelineConfig[T]) WithObserver(obs RefreshObserver) {
	p.obsMu.Lock()
	p.observers = append(p.observers, obs)
	p.obsMu.Unlock()
}

// StartPolling starts a background goroutine that calls Refresh at the given
// interval. The goroutine stops when ctx is cancelled. Each Refresh uses a
// per-attempt context with the given timeout (0 = no timeout).
// The first Refresh fires immediately before the first tick.
// Calling StartPolling again on the same PipelineConfig is a no-op.
func (p *PipelineConfig[T]) StartPolling(ctx context.Context, interval, timeout time.Duration) {
	p.pollOnce.Do(func() {
		go func() {
			// Fire immediately before the first tick.
			p.doRefresh(ctx, timeout)

			ticker := time.NewTicker(interval)
			defer ticker.Stop()
			for {
				select {
				case <-ctx.Done():
					return
				case <-ticker.C:
					p.doRefresh(ctx, timeout)
				}
			}
		}()
	})
}

// doRefresh calls Refresh with a per-attempt context respecting the given timeout.
func (p *PipelineConfig[T]) doRefresh(ctx context.Context, timeout time.Duration) {
	if timeout <= 0 {
		p.Refresh(ctx) //nolint:errcheck
		return
	}
	aCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	p.Refresh(aCtx) //nolint:errcheck
}
