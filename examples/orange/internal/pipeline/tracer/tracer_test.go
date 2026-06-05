package tracer

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel/attribute"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace/noop"
)

func TestEndpointFromPath(t *testing.T) {
	cases := []struct {
		path string
		want string
	}{
		{"/v1/chat/completions", "chat_completions"},
		{"/v1/chat/completions?stream=true", "chat_completions"},
		{"/v1/messages", "messages"},
		{"/v1/embeddings", "embeddings"},
		{"/v1/images/generations", "images"},
		{"/v1/responses", "responses"},
		{"/v1/models", ""},
		{"/mcp/sse", ""},
	}
	for _, tc := range cases {
		t.Run(tc.path, func(t *testing.T) {
			assert.Equal(t, tc.want, endpointFromPath(tc.path))
		})
	}
}

func TestSpanNameAndKind(t *testing.T) {
	cases := []struct {
		endpoint string
		wantName string
		wantKind string
	}{
		{"chat_completions", "ChatCompletion", oiSpanKindLLM},
		{"messages", "Messages", oiSpanKindLLM},
		{"embeddings", "Embeddings", oiSpanKindEmbedding},
		{"images", "ImageGeneration", oiSpanKindLLM},
		{"responses", "Responses", oiSpanKindLLM},
		{"unknown", "LLM", oiSpanKindLLM},
	}
	for _, tc := range cases {
		t.Run(tc.endpoint, func(t *testing.T) {
			name, kind := spanNameAndKind(tc.endpoint)
			assert.Equal(t, tc.wantName, name)
			assert.Equal(t, tc.wantKind, kind)
		})
	}
}

func TestResolveSystem(t *testing.T) {
	assert.Equal(t, oiLLMSystemAnthropic, resolveSystem("anthropic"))
	assert.Equal(t, oiLLMSystemAnthropic, resolveSystem("awsanthropic"))
	assert.Equal(t, oiLLMSystemAnthropic, resolveSystem("gcpanthropic"))
	assert.Equal(t, oiLLMSystemOpenAI, resolveSystem("openai"))
	assert.Equal(t, oiLLMSystemOpenAI, resolveSystem("gcpvertexai"))
}

func TestOpenAIJSONResponseAttrs(t *testing.T) {
	exp := tracetest.NewInMemoryExporter()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSyncer(exp))
	_, span := tp.Tracer("test").Start(context.Background(), "test")

	body := []byte(`{
		"choices": [
			{"index": 0, "message": {"role": "assistant", "content": "hello"}, "finish_reason": "stop"}
		],
		"usage": {"prompt_tokens": 10, "completion_tokens": 5, "total_tokens": 15}
	}`)
	setOpenAIJSONResponseAttrs(span, body)
	span.End()

	spans := exp.GetSpans()
	require.Len(t, spans, 1)
	attrs := attrMap(spans[0].Attributes)

	assert.Equal(t, "assistant", attrs[outputMessageAttr(0, oiMessageRole)])
	assert.Equal(t, "hello", attrs[outputMessageAttr(0, oiMessageContent)])
	assert.Equal(t, "stop", attrs[oiLLMFinishReason])
	assert.Equal(t, int64(10), attrs[oiTokenCountPrompt])
	assert.Equal(t, int64(5), attrs[oiTokenCountCompletion])
	assert.Equal(t, int64(15), attrs[oiTokenCountTotal])
}

func TestAnthropicJSONResponseAttrs(t *testing.T) {
	exp := tracetest.NewInMemoryExporter()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSyncer(exp))
	_, span := tp.Tracer("test").Start(context.Background(), "test")

	body := []byte(`{
		"content": [{"type": "text", "text": "world"}],
		"stop_reason": "end_turn",
		"usage": {"input_tokens": 8, "output_tokens": 3}
	}`)
	setAnthropicJSONResponseAttrs(span, body)
	span.End()

	spans := exp.GetSpans()
	require.Len(t, spans, 1)
	attrs := attrMap(spans[0].Attributes)

	assert.Equal(t, "world", attrs[outputMessageAttr(0, oiMessageContent)])
	assert.Equal(t, "end_turn", attrs[oiLLMFinishReason])
	assert.Equal(t, int64(8), attrs[oiTokenCountPrompt])
	assert.Equal(t, int64(3), attrs[oiTokenCountCompletion])
}

func TestEmbeddingResponseAttrs(t *testing.T) {
	exp := tracetest.NewInMemoryExporter()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSyncer(exp))
	_, span := tp.Tracer("test").Start(context.Background(), "test")

	body := []byte(`{"usage": {"prompt_tokens": 6, "total_tokens": 6}}`)
	setEmbeddingResponseAttrs(span, body)
	span.End()

	spans := exp.GetSpans()
	require.Len(t, spans, 1)
	attrs := attrMap(spans[0].Attributes)
	assert.Equal(t, int64(6), attrs[oiTokenCountPrompt])
}

func TestInitTPNoopOnDisabled(t *testing.T) {
	t.Setenv("OTEL_SDK_DISABLED", "true")
	tr, prop, err := initTP(context.Background())
	require.NoError(t, err)
	require.NotNil(t, tr)
	require.NotNil(t, prop)
	_, isNoop := tr.(noop.Tracer)
	assert.True(t, isNoop, "expected noop tracer when OTEL_SDK_DISABLED=true")
}

func TestInitTPNoopWhenNoEndpoint(t *testing.T) {
	t.Setenv("OTEL_SDK_DISABLED", "")
	t.Setenv("OTEL_TRACES_EXPORTER", "")
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "")
	t.Setenv("OTEL_EXPORTER_OTLP_TRACES_ENDPOINT", "")
	tr, _, err := initTP(context.Background())
	require.NoError(t, err)
	_, isNoop := tr.(noop.Tracer)
	assert.True(t, isNoop, "expected noop tracer when no endpoint configured")
}

// attrMap converts OTel span attributes to a string-keyed map for easy lookup.
func attrMap(kvs []attribute.KeyValue) map[string]any {
	m := make(map[string]any, len(kvs))
	for _, kv := range kvs {
		switch kv.Value.Type() {
		case attribute.STRING:
			m[string(kv.Key)] = kv.Value.AsString()
		case attribute.INT64:
			m[string(kv.Key)] = kv.Value.AsInt64()
		case attribute.FLOAT64:
			m[string(kv.Key)] = kv.Value.AsFloat64()
		case attribute.BOOL:
			m[string(kv.Key)] = kv.Value.AsBool()
		}
	}
	return m
}
