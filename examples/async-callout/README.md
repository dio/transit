# Async Callout

This example shows the user-facing async callout API. Handler code imports only
`github.com/dio/transit/up`; Envoy SDK types stay inside Transit.

```go
func Handler(w *up.Writer, _ *up.Request) {
	_, err := w.HTTPCallout(up.HTTPCalloutRequest{
		Cluster: "auth-service",
		Headers: [][2]string{
			{":method", "POST"},
			{":path", "/check"},
			{":authority", "auth-service.local"},
		},
		TimeoutMillis: 250,
	}, func(result up.HTTPCalloutResult, _ [][2]up.Buffer, body []up.Buffer) {
		if result != up.HTTPCalloutSuccess {
			w.SendLocalResponse(503, []byte(`{"error":"auth unavailable"}`))
			return
		}
		w.SetRequestHeader("x-auth-checked", "true")
	})
	if err != nil {
		w.SendLocalResponse(503, []byte(`{"error":"auth unavailable"}`))
	}
}
```

`w.HTTPCallout` pauses the request and runs the continuation from Envoy's
callout callback, which is the safe place to send a local response.
Callout response buffers are `up.Buffer`; call `String` or `Bytes` before
retaining data beyond the callback.
