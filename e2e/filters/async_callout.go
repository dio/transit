package filters

import (
	"github.com/dio/transit/up"
	"github.com/envoyproxy/envoy/source/extensions/dynamic_modules/sdk/go/shared"
)

func init() {
	up.Register("e2e-async-callout", asyncCallout)
}

func asyncCallout(w *up.Writer, r *up.Request) {
	_, err := w.HTTPCallout(up.HTTPCalloutRequest{
		Cluster: "async-callout-upstream",
		Headers: [][2]string{
			{":method", "GET"},
			{":path", r.Path},
			{":scheme", "http"},
			{":authority", "async-callout.local"},
		},
		TimeoutMillis: 1000,
	}, func(result up.HTTPCalloutResult, _ [][2]shared.UnsafeEnvoyBuffer, body []shared.UnsafeEnvoyBuffer) {
		if result != up.HTTPCalloutSuccess {
			w.SendLocalResponse(503, []byte("callout failed"), [2]string{"content-type", "text/plain"})
			return
		}
		if len(body) == 0 {
			w.SendLocalResponse(503, []byte("empty callout body"), [2]string{"content-type", "text/plain"})
			return
		}
		w.SendLocalResponse(
			200,
			[]byte(body[0].ToString()),
			[2]string{"content-type", "text/plain"},
			[2]string{"x-async-callout", "ok"},
		)
	})
	if err != nil {
		w.SendLocalResponse(503, []byte(err.Error()), [2]string{"content-type", "text/plain"})
	}
}
