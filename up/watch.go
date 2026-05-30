package up

import (
	"context"
	"log"
)

// RunRetry calls fn in a loop until ctx is cancelled. When fn returns a
// non-nil error and the context is still active, the error is logged with
// label as a prefix and fn is retried immediately. When fn returns nil the
// loop continues regardless — fn should only return nil on a clean shutdown.
//
// Use this inside a [ClusterGroup.Go] goroutine to wrap a streaming RPC
// receive loop so that transient stream failures retry automatically:
//
//	cg.Go(func(ctx context.Context) {
//	    up.RunRetry(ctx, "catalog-watch", func(ctx context.Context) error {
//	        stream, err := client.Watch(ctx, connect.NewRequest(&req))
//	        if err != nil {
//	            return err
//	        }
//	        for stream.Receive() {
//	            handle(stream.Msg())
//	        }
//	        return stream.Err()
//	    })
//	})
func RunRetry(ctx context.Context, label string, fn func(ctx context.Context) error) {
	for {
		if err := fn(ctx); err != nil && ctx.Err() == nil {
			log.Printf("transit: %s: stream error: %v; retrying", label, err)
		}
		if ctx.Err() != nil {
			return
		}
	}
}
