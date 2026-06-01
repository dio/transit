// e2e-upstream is loaded as an upstream HTTP filter on a cluster (via
// HttpProtocolOptions.http_filters). It stamps "x-upstream-filter: ran"
// on every response and removes "x-upstream-source" so UpstreamFilterSuite
// can verify both header mutation directions from the cluster side.
package filters

import "github.com/dio/transit/up"

func init() {
	up.Register("e2e-upstream", func(_ *up.Writer, _ *up.Request) {}, up.WithResponse(upstreamOnResponse))
}

func upstreamOnResponse(w *up.Writer, chunk *up.ResponseChunk) {
	if chunk.StatusCode == 0 {
		return
	}
	w.SetResponseHeader("x-upstream-filter", "ran")
	w.RemoveResponseHeader("x-upstream-source")
}
