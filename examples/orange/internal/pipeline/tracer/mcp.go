package tracer

import (
	"context"
	"net/http"
	"strings"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/propagation"
	oteltrace "go.opentelemetry.io/otel/trace"
	"go.opentelemetry.io/otel/trace/noop"
)

// StartMCPSpan starts an OpenInference span for an incoming MCP request.
// The span kind is TOOL for tools/call and CHAIN for all other methods.
// Parent context is extracted from the incoming HTTP headers via W3C propagation.
//
// Callers must call span.End() when the request completes.
func StartMCPSpan(ctx context.Context, r *http.Request, method, toolName string) (context.Context, oteltrace.Span) {
	t := activeTracer()
	if t == nil {
		return ctx, noop.Span{}
	}
	parentCtx := activePropagator().Extract(ctx, propagation.HeaderCarrier(r.Header))

	spanName := mcpSpanName(method, toolName)

	ctx, span := t.Start(parentCtx, spanName,
		oteltrace.WithSpanKind(oteltrace.SpanKindInternal),
		oteltrace.WithAttributes(mcpSpanAttrs(method, toolName)...),
	)
	return ctx, span
}

// InjectMCPTrace injects the current trace context from ctx into the given
// outbound HTTP request headers, enabling downstream services to continue
// the trace.
func InjectMCPTrace(ctx context.Context, headers http.Header) {
	activePropagator().Inject(ctx, propagation.HeaderCarrier(headers))
}

func mcpSpanName(method, toolName string) string {
	if method == "tools/call" && toolName != "" {
		return toolName
	}
	return method
}

func mcpSpanAttrs(method, toolName string) []attribute.KeyValue {
	attrs := []attribute.KeyValue{
		attribute.String(oiSpanKind, func() string {
			if method == "tools/call" {
				return oiSpanKindTool
			}
			return oiSpanKindChain
		}()),
	}
	if toolName != "" && method == "tools/call" {
		attrs = append(attrs, attribute.String(oiToolName, toolName))
	}
	return attrs
}

// RecordMCPResult sets output and error attributes on span based on the
// HTTP status code and optional response body.
func RecordMCPResult(span oteltrace.Span, statusCode int, body []byte, err error) {
	if err != nil {
		span.RecordError(err)
		return
	}
	if statusCode >= 400 {
		span.SetAttributes(attribute.Int("http.status_code", statusCode))
		return
	}
	if len(body) > 0 {
		val := string(body)
		if strings.HasPrefix(val, "{") || strings.HasPrefix(val, "[") {
			span.SetAttributes(
				attribute.String(oiOutputValue, val),
				attribute.String(oiOutputMIME, oiMIMETypeJSON),
			)
		}
	}
}

// activeTracer returns the package-level tracer if initialised, nil otherwise.
func activeTracer() oteltrace.Tracer {
	if tracer == nil {
		return nil
	}
	return tracer
}

func activePropagator() propagation.TextMapPropagator {
	if propagator == nil {
		return propagation.NewCompositeTextMapPropagator()
	}
	return propagator
}
