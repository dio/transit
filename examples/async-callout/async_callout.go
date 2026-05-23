package async_callout

import "github.com/dio/transit/up"

func init() {
	up.Register("async-callout", Handler)
}

// Handler calls an Envoy cluster asynchronously before deciding the response.
func Handler(w *up.Writer, _ *up.Request) {
	_, err := w.HTTPCallout(up.HTTPCalloutRequest{
		Cluster: "auth-service",
		Headers: [][2]string{
			{":method", "POST"},
			{":path", "/check"},
			{":scheme", "http"},
			{":authority", "auth-service.local"},
		},
		Body:          []byte(`{"scope":"read"}`),
		TimeoutMillis: 250,
	}, func(result up.HTTPCalloutResult, _ [][2]up.Buffer, body []up.Buffer) {
		if result != up.HTTPCalloutSuccess {
			w.SendLocalResponse(503, []byte(`{"error":"auth unavailable"}`), [2]string{"content-type", "application/json"})
			return
		}
		if len(body) == 0 || body[0].String() != "ok" {
			w.SendLocalResponse(403, []byte(`{"error":"denied"}`), [2]string{"content-type", "application/json"})
			return
		}
		w.SetRequestHeader("x-auth-checked", "true")
	})
	if err != nil {
		w.SendLocalResponse(503, []byte(`{"error":"auth unavailable"}`), [2]string{"content-type", "application/json"})
	}
}
