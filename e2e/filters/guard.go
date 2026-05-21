package filters

import "github.com/dio/transit/up"

func init() {
	up.Register("guard", func(w *up.Writer, r *up.Request) {
		if r.Header("x-api-key") == "" {
			w.SendLocalResponse(401, []byte(`{"error":"missing x-api-key"}`))
		}
	})
}
