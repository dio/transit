// Package observability is a Transit Envoy dynamic module example that
// demonstrates observability via the active tracing span (OTEL export path)
// and dynamic metadata for access logs.
//
// On request headers it:
//   - Sets the span operation name to "http.request"
//   - Tags the span with http.method and http.path
//   - Tags the span with llm.model if the x-model header is present
//   - Writes the model to dynamic metadata under the "observability" namespace
//   - Increments a request counter
//
// On response (EndStream), it:
//   - Tags the span with the HTTP response code
//   - Increments a response counter
//   - Writes the response code to dynamic metadata
package observability

import (
	"strings"

	"github.com/dio/transit/up"
)

// ExtensionName is the Envoy filter name used in envoy.yaml.
const ExtensionName = "observability"

// Metric IDs defined once at config time.
var (
	requestsID  up.MetricID
	responsesID up.MetricID
)

func init() {
	up.RegisterWithConfig(
		ExtensionName,
		func(h up.ConfigHandle) error {
			var err error
			requestsID, err = h.DefineCounter("observability_requests_total")
			if err != nil {
				return err
			}
			responsesID, err = h.DefineCounter("observability_responses_total")
			return err
		},
		onRequest,
		onResponse,
	)
}

// ModelFromHeader returns (value, true) when v is a non-empty, non-whitespace
// model name. Exported for unit testing.
func ModelFromHeader(v string) (string, bool) {
	v = strings.TrimSpace(v)
	if v == "" {
		return "", false
	}
	return v, true
}

func onRequest(w *up.Writer, r *up.Request) {
	span := w.GetActiveSpan()
	if span != nil {
		span.SetOperation("http.request")
		span.SetTag("http.method", r.Method)
		span.SetTag("http.path", r.Path)
	}

	if model, ok := ModelFromHeader(r.Header("x-model")); ok {
		if span != nil {
			span.SetTag("llm.model", model)
		}
		w.SetMetadata(ExtensionName, "model", model)
	}

	w.IncrementCounter(requestsID, 1)
}

func onResponse(w *up.Writer, chunk *up.ResponseChunk) {
	if chunk.StatusCode != 0 {
		// Response headers phase: tag status code on the span.
		span := w.GetActiveSpan()
		if span != nil {
			code, ok := w.GetAttributeString(up.AttributeIDResponseCode)
			if ok {
				span.SetTag("http.status_code", code.String())
			}
		}
		w.SetMetadata(ExtensionName, "status_code", chunk.StatusCode)
	}

	if chunk.EndStream {
		w.IncrementCounter(responsesID, 1)
	}
}
