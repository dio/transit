// e2e-upstream-auth-group exercises [up.RegisterWithGroup]: a background
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

	up.RegisterWithGroup("e2e-upstream-auth-group", g, func(w *up.Writer, _ *up.Request) {
		headers.Range(func(k, v any) bool {
			w.SetRequestHeader(k.(string), v.(string))
			return true
		})
	})
}
