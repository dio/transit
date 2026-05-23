package filters

import (
	"context"
	"fmt"

	"github.com/envoyproxy/envoy/source/extensions/dynamic_modules/sdk/go/shared"

	"github.com/dio/transit/up"
)

func init() {
	up.Register("e2e-async-callout", asyncCallout)
	up.RegisterWithMutableBody("e2e-async-callout-body", asyncCalloutBodyHeaders, asyncCalloutBodyHandler, nil)
}

func asyncCallout(w *up.Writer, r *up.Request) {
	switch r.Path {
	case "/missing-host":
		// Intentionally omit the required host header so HTTPCallout returns an
		// init error. The filter sends the error text as a local 503 response.
		_, err := w.HTTPCallout(up.HTTPCalloutRequest{
			Cluster: "async-callout-upstream",
			Headers: [][2]string{
				{":method", "GET"},
				{":path", "/missing-host"},
				{":scheme", "http"},
				// host deliberately omitted
			},
			TimeoutMillis: 1000,
		}, func(_ up.HTTPCalloutResult, _ [][2]shared.UnsafeEnvoyBuffer, _ []shared.UnsafeEnvoyBuffer) {
			w.SendLocalResponse(200, []byte("unexpected-ok"))
		})
		if err != nil {
			w.SendLocalResponse(503, []byte(err.Error()), [2]string{"content-type", "text/plain"})
		}

	case "/forward":
		// Callout succeeds; callback mutates a request header without sending a
		// local response so the request continues to the forward-echo upstream.
		_, err := w.HTTPCallout(up.HTTPCalloutRequest{
			Cluster: "async-callout-upstream",
			Headers: [][2]string{
				{":method", "GET"},
				{":path", "/forward"},
				{":scheme", "http"},
				{"host", "async-callout.local"},
			},
			TimeoutMillis: 1000,
		}, func(result up.HTTPCalloutResult, _ [][2]shared.UnsafeEnvoyBuffer, _ []shared.UnsafeEnvoyBuffer) {
			if result != up.HTTPCalloutSuccess {
				w.SendLocalResponse(503, []byte("callout failed"), [2]string{"content-type", "text/plain"})
				return
			}
			w.SetRequestHeader("x-callout-result", "forwarded")
		})
		if err != nil {
			w.SendLocalResponse(503, []byte(err.Error()), [2]string{"content-type", "text/plain"})
		}

	case "/go-do":
		// Goroutine issues a callout via w.Do, sets a request header from the
		// response body, then lets the request forward to the forward-echo upstream.
		w.Go(func(ctx context.Context) {
			resp, err := w.Do(ctx, up.HTTPCalloutRequest{
				Cluster: "async-callout-upstream",
				Headers: [][2]string{
					{":method", "GET"},
					{":path", "/go-do"},
					{":scheme", "http"},
					{"host", "async-callout.local"},
				},
				TimeoutMillis: 1000,
			})
			if err != nil || resp.Result != up.HTTPCalloutSuccess {
				return
			}
			if len(resp.Body) > 0 {
				w.SetRequestHeader("x-go-result", resp.Body[0].ToString())
			}
		})

	default:
		// /checked and any other path: call out, return callout body as local response.
		_, err := w.HTTPCallout(up.HTTPCalloutRequest{
			Cluster: "async-callout-upstream",
			Headers: [][2]string{
				{":method", "GET"},
				{":path", r.Path},
				{":scheme", "http"},
				{"host", "async-callout.local"},
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
}

// asyncCalloutBodyHeaders is the request-headers handler for e2e-async-callout-body.
// It launches a Go+Do callout; the result is set as x-go-result on the forwarded request.
func asyncCalloutBodyHeaders(w *up.Writer, r *up.Request) {
	w.Go(func(ctx context.Context) {
		resp, err := w.Do(ctx, up.HTTPCalloutRequest{
			Cluster: "async-callout-upstream",
			Headers: [][2]string{
				{":method", "GET"},
				{":path", r.Path},
				{":scheme", "http"},
				{"host", "async-callout.local"},
			},
			TimeoutMillis: 1000,
		})
		if err != nil || resp.Result != up.HTTPCalloutSuccess {
			return
		}
		if len(resp.Body) > 0 {
			w.SetRequestHeader("x-go-result", resp.Body[0].ToString())
		}
	})
}

// asyncCalloutBodyHandler runs after the Go+Do goroutine resumes (goStarted=false).
// It records the body length as a request header so the forward-echo upstream echoes it back.
func asyncCalloutBodyHandler(w *up.Writer, chunk *up.BodyChunk) {
	if chunk.EndStream {
		w.SetRequestHeader("x-body-len", fmt.Sprintf("%d", len(chunk.Data)))
	}
}
