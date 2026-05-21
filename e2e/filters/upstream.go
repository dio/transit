// e2e-upstream is loaded as an upstream HTTP filter on a cluster (via
// HttpProtocolOptions.http_filters). It stamps "x-upstream-filter: ran"
// on every response so UpstreamFilterSuite can verify the filter ran on the
// cluster side rather than the listener side.
package filters

import "github.com/dio/transit/up"

func init() {
	up.RegisterWithResponse("e2e-upstream", func(_ *up.Writer, _ *up.Request) {}, upstreamOnResponse)
}

func upstreamOnResponse(w *up.Writer, chunk *up.ResponseChunk) {
	if chunk.StatusCode == 0 {
		return
	}
	w.SetResponseHeader("x-upstream-filter", "ran")
}
