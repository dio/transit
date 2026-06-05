package tracer

import (
	"bytes"
	"strings"

	"github.com/tidwall/gjson"
	"go.opentelemetry.io/otel/attribute"
	oteltrace "go.opentelemetry.io/otel/trace"

	"github.com/dio/transit/examples/orange/internal/pipeline/match"
	"github.com/dio/transit/up/buffer"
)

// setResponseAttrs parses the response body and sets OpenInference output
// attributes on span, then adds token-count attributes. Parsing is endpoint-
// aware so embeddings, images, and LLM completions each use the right fields.
func setResponseAttrs(
	span oteltrace.Span,
	endpoint string,
	streaming bool,
	body []byte,
	ring *buffer.HeadTail,
) {
	switch endpoint {
	case match.EndpointEmbeddings:
		setEmbeddingResponseAttrs(span, body)
	case match.EndpointImages:
		setImageResponseAttrs(span, body)
	case match.EndpointMessages:
		if streaming {
			setAnthropicSSEResponseAttrs(span, ring.Head(), ring.Tail())
		} else {
			setAnthropicJSONResponseAttrs(span, body)
		}
	default: // chat_completions, responses
		if streaming {
			setOpenAISSEResponseAttrs(span, ring.Head(), ring.Tail())
		} else {
			setOpenAIJSONResponseAttrs(span, body)
		}
	}
}

// setOpenAIJSONResponseAttrs sets span attributes for a non-streaming OpenAI
// chat-completions or responses body.
func setOpenAIJSONResponseAttrs(span oteltrace.Span, body []byte) {
	if len(body) == 0 {
		return
	}
	r := gjson.ParseBytes(body)

	span.SetAttributes(
		attribute.String(oiOutputValue, string(body)),
		attribute.String(oiOutputMIME, oiMIMETypeJSON),
	)

	// Output messages from choices[].
	r.Get("choices").ForEach(func(_, choice gjson.Result) bool {
		i := int(choice.Get("index").Int())
		msg := choice.Get("message")
		if role := msg.Get("role").String(); role != "" {
			span.SetAttributes(attribute.String(outputMessageAttr(i, oiMessageRole), role))
		}
		if content := msg.Get("content").String(); content != "" {
			span.SetAttributes(attribute.String(outputMessageAttr(i, oiMessageContent), content))
		}
		if reason := choice.Get("finish_reason").String(); reason != "" {
			span.SetAttributes(attribute.String(oiLLMFinishReason, reason))
		}
		return true
	})

	setOpenAITokenAttrs(span, r)
}

// setOpenAISSEResponseAttrs sets span attributes for a streaming OpenAI response
// using the head+tail ring buffer (same data the meter reads).
func setOpenAISSEResponseAttrs(span oteltrace.Span, head, tail []byte) {
	// Walk SSE events across head and tail. Extract the last [DONE] usage chunk.
	scanSSE(head, tail, func(data []byte) bool {
		if gjson.GetBytes(data, "usage.total_tokens").Exists() {
			r := gjson.ParseBytes(data)
			setOpenAITokenAttrs(span, r)
		}
		return true
	})
}

func setOpenAITokenAttrs(span oteltrace.Span, r gjson.Result) {
	usage := r.Get("usage")
	if !usage.Exists() {
		return
	}
	setIntAttr(span, oiTokenCountPrompt, usage.Get("prompt_tokens").Int())
	setIntAttr(span, oiTokenCountCompletion, usage.Get("completion_tokens").Int())
	setIntAttr(span, oiTokenCountTotal, usage.Get("total_tokens").Int())

	details := usage.Get("prompt_tokens_details")
	setIntAttr(span, oiTokenCountCacheRead, details.Get("cached_tokens").Int())
	setIntAttr(span, oiTokenCountCacheWrite, details.Get("audio_tokens").Int()) // not standard but harmless

	compDetails := usage.Get("completion_tokens_details")
	setIntAttr(span, oiTokenCountReasoning, compDetails.Get("reasoning_tokens").Int())
}

// setAnthropicJSONResponseAttrs sets span attributes for Anthropic Messages API.
func setAnthropicJSONResponseAttrs(span oteltrace.Span, body []byte) {
	if len(body) == 0 {
		return
	}
	r := gjson.ParseBytes(body)

	span.SetAttributes(
		attribute.String(oiOutputValue, string(body)),
		attribute.String(oiOutputMIME, oiMIMETypeJSON),
	)

	r.Get("content").ForEach(func(_, block gjson.Result) bool {
		if block.Get("type").String() == "text" {
			span.SetAttributes(attribute.String(outputMessageAttr(0, oiMessageContent), block.Get("text").String()))
		}
		return true
	})

	if reason := r.Get("stop_reason").String(); reason != "" {
		span.SetAttributes(attribute.String(oiLLMFinishReason, reason))
	}

	usage := r.Get("usage")
	setIntAttr(span, oiTokenCountPrompt, usage.Get("input_tokens").Int())
	setIntAttr(span, oiTokenCountCompletion, usage.Get("output_tokens").Int())
	setIntAttr(span, oiTokenCountTotal, usage.Get("input_tokens").Int()+usage.Get("output_tokens").Int())
	setIntAttr(span, oiTokenCountCacheRead, usage.Get("cache_read_input_tokens").Int())
	setIntAttr(span, oiTokenCountCacheWrite, usage.Get("cache_creation_input_tokens").Int())
}

// setAnthropicSSEResponseAttrs sets span attributes for streaming Anthropic Messages API.
func setAnthropicSSEResponseAttrs(span oteltrace.Span, head, tail []byte) {
	scanSSE(head, tail, func(data []byte) bool {
		if gjson.GetBytes(data, "type").String() == "message_delta" {
			r := gjson.ParseBytes(data)
			if reason := r.Get("delta.stop_reason").String(); reason != "" {
				span.SetAttributes(attribute.String(oiLLMFinishReason, reason))
			}
			usage := r.Get("usage")
			setIntAttr(span, oiTokenCountCompletion, usage.Get("output_tokens").Int())
		}
		if gjson.GetBytes(data, "type").String() == "message_start" {
			r := gjson.ParseBytes(data)
			usage := r.Get("message.usage")
			setIntAttr(span, oiTokenCountPrompt, usage.Get("input_tokens").Int())
			setIntAttr(span, oiTokenCountCacheRead, usage.Get("cache_read_input_tokens").Int())
			setIntAttr(span, oiTokenCountCacheWrite, usage.Get("cache_creation_input_tokens").Int())
		}
		return true
	})
}

// setEmbeddingResponseAttrs sets span attributes for OpenAI Embeddings responses.
func setEmbeddingResponseAttrs(span oteltrace.Span, body []byte) {
	if len(body) == 0 {
		return
	}
	r := gjson.ParseBytes(body)
	span.SetAttributes(
		attribute.String(oiOutputValue, string(body)),
		attribute.String(oiOutputMIME, oiMIMETypeJSON),
	)
	usage := r.Get("usage")
	setIntAttr(span, oiTokenCountPrompt, usage.Get("prompt_tokens").Int())
	setIntAttr(span, oiTokenCountTotal, usage.Get("total_tokens").Int())
}

// setImageResponseAttrs sets span attributes for image generation responses.
func setImageResponseAttrs(span oteltrace.Span, body []byte) {
	if len(body) == 0 {
		return
	}
	span.SetAttributes(
		attribute.String(oiOutputValue, string(body)),
		attribute.String(oiOutputMIME, oiMIMETypeJSON),
	)
}

// scanSSE walks raw SSE bytes in [head, tail] and calls fn for each data: line.
// Mirrors the scan pattern used by the meter extractors.
func scanSSE(head, tail []byte, fn func(data []byte) bool) {
	prefix := []byte("data: ")
	for _, buf := range [][]byte{head, tail} {
		for len(buf) > 0 {
			line, rest, _ := bytes.Cut(buf, []byte("\n"))
			buf = rest
			if len(line) < len(prefix) || !bytes.Equal(line[:len(prefix)], prefix) {
				continue
			}
			payload := line[len(prefix):]
			if strings.TrimSpace(string(payload)) == "[DONE]" {
				continue
			}
			if !fn(payload) {
				return
			}
		}
	}
}

func setIntAttr(span oteltrace.Span, key string, val int64) {
	if val > 0 {
		span.SetAttributes(attribute.Int64(key, val))
	}
}
