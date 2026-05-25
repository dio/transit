package async_callout

import (
	"github.com/envoyproxy/envoy/source/extensions/dynamic_modules/sdk/go/shared"

	"github.com/dio/transit/up"
)

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
			{"host", "auth-service.local"},
		},
		Body:          []byte(`{"scope":"read"}`),
		TimeoutMillis: 250,
	}, func(result up.HTTPCalloutResult, _ [][2]shared.UnsafeEnvoyBuffer, body []shared.UnsafeEnvoyBuffer) {
		bodyStr := ""
		if len(body) > 0 {
			bodyStr = body[0].ToString()
		}
		code, resp := CheckAuth(result, bodyStr)
		if code != 0 {
			w.SendLocalResponse(code, resp, [2]string{"content-type", "application/json"})
			return
		}
		w.SetRequestHeader("x-auth-checked", "true")
	})
	if err != nil {
		w.SendLocalResponse(503, []byte(`{"error":"auth unavailable"}`), [2]string{"content-type", "application/json"})
	}
}

// CheckAuth evaluates an auth callout result and response body string.
// Returns (0, nil) meaning "pass through", or (statusCode, body) to send a local response.
func CheckAuth(result up.HTTPCalloutResult, body string) (int, []byte) {
	if result != up.HTTPCalloutSuccess {
		return 503, []byte(`{"error":"auth unavailable"}`)
	}
	if body != "ok" {
		return 403, []byte(`{"error":"denied"}`)
	}
	return 0, nil
}
