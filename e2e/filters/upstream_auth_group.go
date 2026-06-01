// e2e-upstream-auth-group exercises [up.Register] + [up.WithGroup]: a background
// goroutine populates a sync.Map of headers, and the request handler injects
// them upstream. No package-level variables are used — state is shared via closure.
package filters

import (
	"context"
	"sync"

	"github.com/dio/transit/up"
)

func init() {
	var headers sync.Map

	g := up.NewGroup()
	g.AddGoroutine(func(ctx context.Context) {
		headers.Store("authorization", "Bearer group-token")
		<-ctx.Done()
	})

	up.Register("e2e-upstream-auth-group", func(w *up.Writer, _ *up.Request) {
		headers.Range(func(k, v any) bool {
			name, ok := k.(string)
			if !ok {
				return true
			}
			value, ok := v.(string)
			if !ok {
				return true
			}
			w.SetRequestHeader(name, value)
			return true
		})
	}, up.WithGroup(g))
}
