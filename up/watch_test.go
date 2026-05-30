package up

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestRunRetry_ReturnsWhenContextCancelled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	var calls atomic.Int32
	RunRetry(ctx, "test", func(ctx context.Context) error {
		calls.Add(1)
		return nil
	})
	// fn may be called 0 or 1 times depending on scheduling; either is fine.
	// The key assertion is that RunRetry returned, which it has (we're here).
	require.LessOrEqual(t, calls.Load(), int32(1))
}

func TestRunRetry_RetriesOnError(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	var calls atomic.Int32
	RunRetry(ctx, "test", func(ctx context.Context) error {
		n := calls.Add(1)
		if n >= 3 {
			cancel()
			return nil
		}
		return errors.New("transient error")
	})
	require.GreaterOrEqual(t, calls.Load(), int32(3))
}

func TestRunRetry_StopsOnContextCancelledInsideFn(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		RunRetry(ctx, "test", func(ctx context.Context) error {
			<-ctx.Done()
			return ctx.Err()
		})
		close(done)
	}()

	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("RunRetry did not return after context cancellation")
	}
}

func TestRunRetry_DoesNotRetryAfterContextCancelled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	var calls atomic.Int32
	RunRetry(ctx, "test", func(ctx context.Context) error {
		calls.Add(1)
		return errors.New("error")
	})
	// With a cancelled context the loop exits immediately; fn should be
	// called at most once.
	require.LessOrEqual(t, calls.Load(), int32(1))
}
