package filterchain

import "github.com/dio/transit/up"

func init() {
	up.Register("filter-chain", up.Chain(
		func(_ *up.Writer, _ *up.Request) {},
		WithLogging(),
		WithRequiredHeader("x-api-key"),
		WithStampHeader("x-filtered", "true"),
	))
}

// WithLogging returns a Middleware that logs every request at Info level.
func WithLogging() up.Middleware {
	return func(next up.HandlerFunc) up.HandlerFunc {
		return func(w *up.Writer, r *up.Request) {
			w.Log(up.LogInfo, "filter-chain: %s %s", r.Method, r.Path)
			next(w, r)
		}
	}
}

// WithRequiredHeader returns a Middleware that rejects requests (401) missing header.
func WithRequiredHeader(header string) up.Middleware {
	return func(next up.HandlerFunc) up.HandlerFunc {
		return func(w *up.Writer, r *up.Request) {
			if r.Header(header) == "" {
				w.SendLocalResponse(401,
					[]byte(`{"error":"missing required header"}`),
					[2]string{"content-type", "application/json"})
				return
			}
			next(w, r)
		}
	}
}

// WithStampHeader returns a Middleware that sets a fixed request header after calling next.
func WithStampHeader(name, value string) up.Middleware {
	return func(next up.HandlerFunc) up.HandlerFunc {
		return func(w *up.Writer, r *up.Request) {
			next(w, r)
			w.SetRequestHeader(name, value)
		}
	}
}
