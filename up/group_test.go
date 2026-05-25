package up

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// TestGroup_anyActorExitStopsAll verifies that when one goroutine registered
// with AddGoroutine returns normally, the context passed to all other goroutines
// is cancelled — i.e. Start stops the entire group on any actor exit.
func TestGroup_anyActorExitStopsAll(t *testing.T) {
	g := NewGroup()

	// First actor: returns immediately, triggering group shutdown.
	g.AddGoroutine(func(_ context.Context) {})

	// Second actor: blocks until its context is cancelled, then signals done.
	done := make(chan struct{})
	g.AddGoroutine(func(ctx context.Context) {
		<-ctx.Done()
		close(done)
	})

	g.Start()

	require.Eventually(t, func() bool {
		select {
		case <-done:
			return true
		default:
			return false
		}
	}, 2*time.Second, 10*time.Millisecond)
}
