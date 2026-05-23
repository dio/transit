# Async Callout

This example shows the user-facing async callout API. Handler code imports
`github.com/dio/transit/up` plus Envoy's shared buffer type for zero-copy
callout response buffers.

```go
func Handler(w *up.Writer, _ *up.Request) {
	_, err := w.HTTPCallout(up.HTTPCalloutRequest{
		Cluster: "auth-service",
		Headers: [][2]string{
			{":method", "POST"},
			{":path", "/check"},
			{"host", "auth-service.local"},
		},
		TimeoutMillis: 250,
	}, func(result up.HTTPCalloutResult, _ [][2]shared.UnsafeEnvoyBuffer, body []shared.UnsafeEnvoyBuffer) {
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
Callout response buffers are borrowed `shared.UnsafeEnvoyBuffer` values; call
`ToString` or `ToBytes` before retaining data beyond the callback.
