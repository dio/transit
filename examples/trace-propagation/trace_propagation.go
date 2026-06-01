// Package tracepropagation is a Transit Envoy dynamic module example that
// demonstrates W3C trace context propagation through a full four-hop path.
//
// Architecture:
//
//	Client ──► Envoy:ProxyPort [trace-propagation filter — ingress span + tags]
//	               ↓ routes to trace-propagation-local:ServerPort
//	           Embedded Go HTTP server  [Go OTLP SDK — embedded span, child of ingress]
//	               ↓ injects updated traceparent, calls Envoy:EgressPort
//	           Envoy:EgressPort [trace-propagation-egress filter — egress span + tags]
//	               ↓ routes to backend:BackendPort
//	           Backend sink
//
// Runtime overrides (for e2e):
//
//	TRACE_PROPAGATION_LISTEN_ADDR  — embedded server address (default: 127.0.0.1:9192)
//	TRACE_PROPAGATION_EGRESS_URL   — egress base URL (default: http://127.0.0.1:9193)
//	TRACE_PROPAGATION_OTEL_ENDPOINT — OTLP gRPC endpoint for embedded server spans
//	                                  (default: 127.0.0.1:4317)
package tracepropagation

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"

	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
	"go.opentelemetry.io/otel/propagation"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	oteltrace "go.opentelemetry.io/otel/trace"
	"go.opentelemetry.io/otel/trace/noop"

	"github.com/dio/transit/up"
)

// ExtensionName is the Envoy filter name for the inbound hop.
const ExtensionName = "trace-propagation"

// EgressExtensionName is the Envoy filter name for the egress hop.
const EgressExtensionName = "trace-propagation-egress"

// UpstreamExtensionName is the Envoy filter name for the upstream (cluster) hop.
const UpstreamExtensionName = "trace-propagation-upstream"

var traceHeaders = []string{
	"traceparent",
	"tracestate",
	"x-request-id",
}

// CopyTraceHeaders copies W3C trace context headers from src to dst.
// Exported for unit testing.
func CopyTraceHeaders(dst, src http.Header) {
	for _, h := range traceHeaders {
		if v := src.Get(h); v != "" {
			dst.Set(h, v)
		}
	}
}

type proxyServer struct {
	egressURL  string
	tracer     oteltrace.Tracer
	propagator propagation.TextMapPropagator
}

func (s *proxyServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	// Extract W3C trace context from inbound headers and create child span.
	ctx = s.propagator.Extract(ctx, propagation.HeaderCarrier(r.Header))
	ctx, span := s.tracer.Start(ctx, "trace-propagation.embedded")
	defer span.End()

	req, err := http.NewRequest(r.Method, s.egressURL+r.RequestURI, r.Body)
	if err != nil {
		span.RecordError(err)
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	// Copy then overwrite trace headers with the child span context so Envoy
	// egress sees this span as the parent.
	CopyTraceHeaders(req.Header, r.Header)
	s.propagator.Inject(ctx, propagation.HeaderCarrier(req.Header))

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		span.RecordError(err)
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	defer func() {
		if err := resp.Body.Close(); err != nil {
			span.RecordError(err)
		}
	}()
	for k, vs := range resp.Header {
		for _, v := range vs {
			w.Header().Add(k, v)
		}
	}
	w.WriteHeader(resp.StatusCode)
	io.Copy(w, resp.Body) //nolint:errcheck
}

func init() { Register() }

// Register wires both filter names into Envoy and starts the embedded server.
func Register() {
	listenAddr := "127.0.0.1:9192"
	if v := os.Getenv("TRACE_PROPAGATION_LISTEN_ADDR"); v != "" {
		listenAddr = v
	}
	egressURL := "http://127.0.0.1:9193"
	if v := os.Getenv("TRACE_PROPAGATION_EGRESS_URL"); v != "" {
		egressURL = v
	}
	otelEndpoint := "127.0.0.1:4317"
	if v := os.Getenv("TRACE_PROPAGATION_OTEL_ENDPOINT"); v != "" {
		otelEndpoint = v
	}

	tracer, prop := setupTracer(otelEndpoint)
	srv := &proxyServer{
		egressURL:  egressURL,
		tracer:     tracer,
		propagator: prop,
	}

	g := up.NewGroup()
	g.Add(
		func() error {
			ln, err := net.Listen("tcp", listenAddr)
			if err != nil {
				fmt.Fprintf(os.Stderr, "trace-propagation: listen %s: %v\n", listenAddr, err)
				return err
			}
			fmt.Fprintf(os.Stderr, "trace-propagation: listening on %s\n", ln.Addr())
			httpSrv := &http.Server{Handler: srv}
			return httpSrv.Serve(ln)
		},
		func() {},
	)

	// Inbound filter: annotates the Envoy-created ingress span.
	up.Register(ExtensionName, func(w *up.Writer, r *up.Request) {
		span := w.GetActiveSpan()
		if span == nil {
			return
		}
		span.SetOperation("trace-propagation.ingress")
		span.SetTag("http.method", r.Method)
		span.SetTag("http.path", r.Path)
	}, up.WithGroup(g))

	// Egress filter: annotates the Envoy-created egress span.
	up.Register(EgressExtensionName, func(w *up.Writer, r *up.Request) {
		span := w.GetActiveSpan()
		if span == nil {
			return
		}
		span.SetOperation("trace-propagation.egress")
		span.SetTag("http.method", r.Method)
		span.SetTag("http.path", r.Path)
	})

	// Upstream filter: runs on the cluster side (backend connection).
	// Stamps x-upstream-filter on every request so the sink can confirm it ran.
	// Also tags the active span if one is available in upstream context.
	up.Register(UpstreamExtensionName, func(w *up.Writer, r *up.Request) {
		w.SetRequestHeader("x-upstream-filter", "ran")
		span := w.GetActiveSpan()
		if span == nil {
			return
		}
		span.SetTag("upstream.filter", "ran")
	})
}

// setupTracer creates an OTLP gRPC exporter pointing at endpoint and returns a
// Tracer and W3C propagator. Falls back to a no-op tracer on error.
func setupTracer(endpoint string) (oteltrace.Tracer, propagation.TextMapPropagator) {
	prop := propagation.NewCompositeTextMapPropagator(propagation.TraceContext{})
	exp, err := otlptracegrpc.New(
		context.Background(),
		otlptracegrpc.WithEndpoint(endpoint),
		otlptracegrpc.WithInsecure(),
	)
	if err != nil {
		fmt.Fprintf(os.Stderr, "trace-propagation: otel exporter: %v — embedded spans disabled\n", err)
		return noop.NewTracerProvider().Tracer(""), prop
	}
	tp := sdktrace.NewTracerProvider(
		sdktrace.WithBatcher(exp),
		sdktrace.WithSampler(sdktrace.AlwaysSample()),
	)
	return tp.Tracer("trace-propagation-embedded"), prop
}
