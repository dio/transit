package tracer

import (
	"context"
	"fmt"
	"os"
	"strings"
	"sync"

	"go.opentelemetry.io/contrib/exporters/autoexport"
	"go.opentelemetry.io/contrib/propagators/autoprop"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/stdout/stdouttrace"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	oteltrace "go.opentelemetry.io/otel/trace"
	"go.opentelemetry.io/otel/trace/noop"

	"github.com/dio/transit/examples/orange/internal/pipeline/match"
	"github.com/dio/transit/up"
	"github.com/dio/transit/up/buffer"
)

const (
	// ExtensionName is the Envoy filter name.
	ExtensionName = "orange-tracer"

	serviceName = "orange"
)

var (
	tracer     oteltrace.Tracer
	propagator propagation.TextMapPropagator
	initOnce   sync.Once
	initErr    error
)

func init() {
	up.Register(
		ExtensionName,
		tracerRequest,
		up.WithConfig(func(_ up.ConfigHandle) error {
			initOnce.Do(func() {
				tracer, propagator, initErr = initTP(context.Background())
			})
			return initErr
		}),
		up.WithResponse(tracerResponse),
	)
}

// initTP initialises the TracerProvider from environment variables.
// Mirrors ai-gateway's NewTracingFromEnv pattern.
func initTP(ctx context.Context) (oteltrace.Tracer, propagation.TextMapPropagator, error) {
	if os.Getenv("OTEL_SDK_DISABLED") == "true" {
		return noop.NewTracerProvider().Tracer(serviceName), autoprop.NewTextMapPropagator(), nil
	}
	if os.Getenv("OTEL_TRACES_EXPORTER") == "none" {
		return noop.NewTracerProvider().Tracer(serviceName), autoprop.NewTextMapPropagator(), nil
	}

	hasOTLP := os.Getenv("OTEL_EXPORTER_OTLP_ENDPOINT") != "" ||
		os.Getenv("OTEL_EXPORTER_OTLP_TRACES_ENDPOINT") != ""
	exporter := os.Getenv("OTEL_TRACES_EXPORTER")
	if exporter == "" && !hasOTLP {
		return noop.NewTracerProvider().Tracer(serviceName), autoprop.NewTextMapPropagator(), nil
	}

	res, err := resource.Merge(
		resource.Default(),
		resource.NewSchemaless(
			attribute.String("service.name", envOrDefault("OTEL_SERVICE_NAME", serviceName)),
		),
	)
	if err != nil {
		return nil, nil, fmt.Errorf("orange tracer: build resource: %w", err)
	}

	var tp *sdktrace.TracerProvider
	if exporter == "console" {
		exp, err := stdouttrace.New(stdouttrace.WithPrettyPrint())
		if err != nil {
			return nil, nil, fmt.Errorf("orange tracer: console exporter: %w", err)
		}
		tp = sdktrace.NewTracerProvider(
			sdktrace.WithSyncer(exp),
			sdktrace.WithResource(res),
		)
	} else {
		exp, err := autoexport.NewSpanExporter(ctx)
		if err != nil {
			return nil, nil, fmt.Errorf("orange tracer: auto exporter: %w", err)
		}
		tp = sdktrace.NewTracerProvider(
			sdktrace.WithBatcher(exp),
			sdktrace.WithResource(res),
		)
	}

	prop := autoprop.NewTextMapPropagator()
	return tp.Tracer(serviceName), prop, nil
}

// tracerState is the per-stream tracing state stored in the stream context slot.
type tracerState struct {
	span      oteltrace.Span
	endpoint  string
	streaming bool
	ring      *buffer.HeadTail
	respBuf   []byte
	skip      bool
}

// tracerRequest is the request-headers callback. It derives the endpoint from
// the request path, starts an OTel span, and injects traceparent into the
// outgoing upstream request headers so downstream services can correlate.
func tracerRequest(w *up.Writer, r *up.Request) {
	s := &tracerState{}
	*r.Context = s

	if tracer == nil {
		s.skip = true
		return
	}

	endpoint := endpointFromPath(r.Path)
	if endpoint == "" {
		s.skip = true
		return
	}
	s.endpoint = endpoint

	// Extract parent context from incoming W3C traceparent/tracestate headers.
	carrier := requestHeaderCarrier{r}
	parentCtx := propagator.Extract(context.Background(), carrier)

	spanName, spanKind := spanNameAndKind(endpoint)
	_, span := tracer.Start(parentCtx, spanName,
		oteltrace.WithSpanKind(oteltrace.SpanKindInternal),
		oteltrace.WithAttributes(attribute.String(oiSpanKind, spanKind)),
	)
	s.span = span

	// Inject traceparent into outgoing upstream request headers so upstream
	// providers and downstream collectors can link spans.
	injectCarrier := writerHeaderCarrier{w}
	propagator.Inject(oteltrace.ContextWithSpan(context.Background(), span), injectCarrier)
}

// tracerResponse is the response observer. Called:
//   - Once on response headers (Data == nil, EndStream == false): read metadata,
//     set request-side LLM attributes on span.
//   - Once per body chunk: accumulate bytes.
//   - Once with EndStream == true: parse body, set response attributes, end span.
func tracerResponse(w *up.Writer, chunk *up.ResponseChunk) {
	if chunk.Data == nil && !chunk.EndStream {
		// Response headers phase.
		s, ok := (*chunk.Context).(*tracerState)
		if !ok || s == nil || s.skip || s.span == nil {
			return
		}

		// Detect streaming before body arrives.
		if strings.Contains(chunk.ContentType, "text/event-stream") {
			s.streaming = true
			s.ring = buffer.NewHeadTail(8*1024, 64*1024)
		}

		// Populate request-side attributes now that match has written metadata.
		setRequestAttrs(w, s.span, s.endpoint)
		return
	}

	s, ok := (*chunk.Context).(*tracerState)
	if !ok || s == nil || s.skip || s.span == nil {
		return
	}

	if len(chunk.Data) > 0 {
		if s.streaming {
			s.ring.Write(chunk.Data)
		} else {
			s.respBuf = append(s.respBuf, chunk.Data...)
		}
	}

	if !chunk.EndStream {
		return
	}

	// Parse response and set output attributes, then end the span.
	setResponseAttrs(s.span, s.endpoint, s.streaming, s.respBuf, s.ring)
	s.span.End()
}

// setRequestAttrs sets request-side OpenInference LLM attributes on the span
// using dynamic metadata written by the match filter.
func setRequestAttrs(w *up.Writer, span oteltrace.Span, endpoint string) {
	model, _ := w.GetMetadataString(up.MetadataSourceDynamic, match.MetadataNamespace, match.MetadataKeyModel)
	provider, _ := w.GetMetadataString(up.MetadataSourceDynamic, match.MetadataNamespace, match.MetadataKeyProvider)

	attrs := make([]attribute.KeyValue, 0, 4)
	if model.Len > 0 {
		switch endpoint {
		case match.EndpointEmbeddings:
			attrs = append(attrs, attribute.String(oiEmbeddingModelName, model.String()))
		default:
			attrs = append(attrs, attribute.String(oiLLMModelName, model.String()))
		}
	}
	if provider.Len > 0 {
		attrs = append(attrs, attribute.String(oiLLMSystem, resolveSystem(provider.String())))
	}
	span.SetAttributes(attrs...)
}

// endpointFromPath maps an HTTP path to a match endpoint constant.
func endpointFromPath(path string) string {
	// Trim query string.
	if i := strings.IndexByte(path, '?'); i >= 0 {
		path = path[:i]
	}
	switch path {
	case "/v1/chat/completions":
		return match.EndpointChatCompletions
	case "/v1/messages":
		return match.EndpointMessages
	case "/v1/embeddings":
		return match.EndpointEmbeddings
	case "/v1/images/generations":
		return match.EndpointImages
	case "/v1/responses":
		return match.EndpointResponses
	}
	return ""
}

// spanNameAndKind returns the OpenInference span name and span kind for an endpoint.
func spanNameAndKind(endpoint string) (name, kind string) {
	switch endpoint {
	case match.EndpointChatCompletions:
		return "ChatCompletion", oiSpanKindLLM
	case match.EndpointMessages:
		return "Messages", oiSpanKindLLM
	case match.EndpointEmbeddings:
		return "Embeddings", oiSpanKindEmbedding
	case match.EndpointImages:
		return "ImageGeneration", oiSpanKindLLM
	case match.EndpointResponses:
		return "Responses", oiSpanKindLLM
	default:
		return "LLM", oiSpanKindLLM
	}
}

// resolveSystem maps a match provider kind to an OI llm.system value.
func resolveSystem(providerKind string) string {
	switch providerKind {
	case "anthropic", "awsanthropic", "gcpanthropic":
		return oiLLMSystemAnthropic
	default:
		return oiLLMSystemOpenAI
	}
}

func envOrDefault(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// requestHeaderCarrier adapts up.Request for OTel propagation extraction.
type requestHeaderCarrier struct{ r *up.Request }

func (c requestHeaderCarrier) Get(key string) string { return c.r.Header(strings.ToLower(key)) }
func (c requestHeaderCarrier) Set(string, string)    {}
func (c requestHeaderCarrier) Keys() []string        { return nil }

// writerHeaderCarrier adapts up.Writer for OTel propagation injection.
type writerHeaderCarrier struct{ w *up.Writer }

func (c writerHeaderCarrier) Get(string) string { return "" }
func (c writerHeaderCarrier) Set(key, val string) {
	c.w.SetRequestHeader(strings.ToLower(key), val)
}
func (c writerHeaderCarrier) Keys() []string { return nil }
