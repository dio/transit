// e2e-tracer interacts with the active Envoy tracing span to verify that a
// dynamic module filter can read and annotate the request's trace. It sets two
// span tags so OtelTracesSuite can assert they appear as OTLP span attributes:
//
//   - "e2e.custom" = "hello-from-tracer"
//   - "e2e.method" = the HTTP request method
//
// Used by OtelTracesSuite together with the envoy.tracers.opentelemetry tracer.
package filters

import "github.com/dio/transit/up"

func init() {
	up.Register("e2e-tracer", tracerOnRequest)
}

func tracerOnRequest(w *up.Writer, r *up.Request) {
	span := w.GetActiveSpan()
	if span == nil {
		return
	}
	span.SetTag("e2e.custom", "hello-from-tracer")
	span.SetTag("e2e.method", r.Method)
}
