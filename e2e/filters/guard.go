// guard rejects requests that lack an x-api-key header with a 401 local
// response. Used by GuardSuite to verify SendLocalResponse and the
// HeadersStatusStop path.
package filters

import "github.com/dio/transit/up"

func init() {
	up.Register("guard", func(w *up.Writer, r *up.Request) {
		if r.Header("x-api-key") == "" {
			w.SendLocalResponse(401, []byte(`{"error":"missing x-api-key"}`))
		}
	})
}
