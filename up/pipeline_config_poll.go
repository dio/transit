package up

import (
	"context"
	"math/rand"
	"sync"
	"time"
)

// Start launches the background polling goroutine. No-op for static configs.
// The first fetch fires immediately; subsequent fetches tick at Interval ± Jitter.
// The returned stop func cancels polling and waits for any in-flight fetch to finish.
// Wire through up.WithGroup or call from Cluster.ServerInitialized / OnDestroy.
func (p *PipelineConfig[T]) Start(ctx context.Context) (stop func()) {
	if p.isStatic {
		return func() {}
	}

	innerCtx, cancel := context.WithCancel(ctx)
	var wg sync.WaitGroup
	wg.Add(1)

	go func() {
		defer wg.Done()
		p.pollFetch(innerCtx) // immediate first fetch

		interval := p.opts.Interval
		for {
			tick := interval
			if p.opts.Jitter > 0 {
				// #nosec G404 — jitter is not security-sensitive
				tick += time.Duration(rand.Int63n(int64(p.opts.Jitter)))
			}
			timer := time.NewTimer(tick)
			select {
			case <-innerCtx.Done():
				timer.Stop()
				return
			case <-timer.C:
				p.pollFetch(innerCtx)
			}
		}
	}()

	return func() {
		cancel()
		wg.Wait()
	}
}

// pollFetch wraps fetchAndStore with the per-attempt timeout from PollOptions.
func (p *PipelineConfig[T]) pollFetch(ctx context.Context) {
	fetchCtx := ctx
	var cancel context.CancelFunc
	if p.opts.Timeout > 0 {
		fetchCtx, cancel = context.WithTimeout(ctx, p.opts.Timeout)
		defer cancel()
	}
	p.fetchAndStore(fetchCtx) //nolint:errcheck
}
