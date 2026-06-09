// Package meter is the streaming and non-streaming response observer for orange.
//
// It extracts LLM token usage from both response shapes:
//   - Streaming (Content-Type: text/event-stream): head+tail ring buffer via
//     up/buffer.HeadTail; dispatches to the provider-specific SSE extractor.
//   - Non-streaming (Content-Type: application/json): accumulates the full
//     body across chunks; dispatches to the provider-specific JSON extractor.
//
// The provider kind and endpoint are read from the orange dynamic-metadata
// namespace (written by the match filter) and together select one of five
// extraction strategies:
//
//	openai-non-streaming + chat_completions/messages → ExtractOpenAIChatCompletionsJSON
//	openai-streaming    + chat_completions/messages  → ExtractOpenAIChatCompletionsSSE
//	openai-non-streaming + responses                 → ExtractOpenAIResponsesJSON
//	openai-streaming    + responses                  → ExtractOpenAIResponsesSSE
//	anthropic-non-streaming → ExtractAnthropicMessagesJSON
//	anthropic-streaming     → ExtractAnthropicMessagesSSE
//
// On stream completion the extracted counts are emitted as Envoy counters
// (orange_input_tokens, orange_output_tokens) and as dynamic metadata under
// the "orange_meter" namespace.
//
// The observer runs on the Envoy worker thread with zero added latency:
// chunks are forwarded to the downstream client as they arrive.
package meter

import (
	"strconv"
	"strings"

	"github.com/dio/transit/examples/orange/internal/pipeline/match"
	"github.com/dio/transit/up"
	"github.com/dio/transit/up/buffer"
	"github.com/dio/transit/up/compress"
)

// ExtensionName is the Envoy filter name.
const ExtensionName = "orange-meter"

var (
	inputTokensID  up.MetricID
	outputTokensID up.MetricID
	imageCountID   up.MetricID

	// OpenAI-specific
	cachedInputID              up.MetricID
	audioInputID               up.MetricID
	imageInputID               up.MetricID
	reasoningOutputID          up.MetricID
	audioOutputID              up.MetricID
	acceptedPredictionOutputID up.MetricID
	rejectedPredictionOutputID up.MetricID

	// Anthropic-specific
	cacheCreationInputID up.MetricID
	cacheReadInputID     up.MetricID
	cacheEphemeral5mID   up.MetricID
	cacheEphemeral1hID   up.MetricID
)

func init() {
	up.Register(
		ExtensionName,
		func(_ *up.Writer, _ *up.Request) {},
		up.WithConfig(func(h up.ConfigHandle) error {
			var err error
			for _, def := range []struct {
				id   *up.MetricID
				name string
			}{
				{&inputTokensID, "orange_input_tokens"},
				{&outputTokensID, "orange_output_tokens"},
				{&imageCountID, "orange_image_count"},
				// OpenAI detail counters
				{&cachedInputID, "orange_cached_input_tokens"},
				{&audioInputID, "orange_audio_input_tokens"},
				{&imageInputID, "orange_image_input_tokens"},
				{&reasoningOutputID, "orange_reasoning_output_tokens"},
				{&audioOutputID, "orange_audio_output_tokens"},
				{&acceptedPredictionOutputID, "orange_accepted_prediction_output_tokens"},
				{&rejectedPredictionOutputID, "orange_rejected_prediction_output_tokens"},
				// Anthropic detail counters
				{&cacheCreationInputID, "orange_cache_creation_input_tokens"},
				{&cacheReadInputID, "orange_cache_read_input_tokens"},
				{&cacheEphemeral5mID, "orange_cache_ephemeral_5m_input_tokens"},
				{&cacheEphemeral1hID, "orange_cache_ephemeral_1h_input_tokens"},
			} {
				if *def.id, err = h.DefineCounter(def.name); err != nil {
					return err
				}
			}
			return nil
		}),
		up.WithResponse(meterResponse),
	)
}

// providerKind is the API wire-format of the upstream response body. It is
// derived from match.MetadataKeyProvider (log field "provider"), which stores
// Decision.ProviderKind — the codec kind, not the upstream's brand identity. A
// Gemini backend using the OpenAI compatibility shim has providerKind ==
// kindOpenAI because the adapter translates its response into OpenAI JSON before
// the meter sees it. Do not confuse with the "upstream" log field, which is the
// config backend name (Decision.ProviderBackend).
type providerKind uint8

const (
	kindOpenAI    providerKind = iota // OpenAI wire-format (default; also used for OpenAI-compatible upstreams like Gemini)
	kindAnthropic                     // Anthropic Messages API wire-format (direct or passthrough)
)

// streamState is per-request state stored in chunk.Context across callbacks.
type streamState struct {
	ring            *buffer.HeadTail // non-nil on the streaming path
	buf             []byte           // accumulator on the non-streaming path
	kind            providerKind
	endpoint        string // match.EndpointResponses or "" for chat_completions/messages
	contentEncoding string // e.g. "gzip"; used to decompress non-streaming bodies
	streaming       bool
	skip            bool
}

// meterResponse is the response observer. It is called:
//   - Once on response headers (Data == nil, EndStream == false): allocate state.
//   - Once per body chunk: feed data into ring or buf.
//   - Once with EndStream == true: extract usage and emit metrics.
func meterResponse(w *up.Writer, chunk *up.ResponseChunk) {
	if chunk.Data == nil && !chunk.EndStream {
		s := &streamState{}
		switch {
		case strings.Contains(chunk.ContentType, "text/event-stream"):
			s.streaming = true
			s.ring = buffer.NewHeadTail(8*1024, 64*1024)
		case strings.Contains(chunk.ContentType, "application/json"):
			// non-streaming: buf is populated lazily on first chunk
		default:
			s.skip = true
		}
		if !s.skip {
			s.kind = resolveKind(w)
			s.endpoint = resolveEndpoint(w)
			s.contentEncoding = chunk.ContentEncoding
		}
		*chunk.Context = s
		return
	}

	s, ok := (*chunk.Context).(*streamState)
	if !ok || s == nil || s.skip {
		return
	}

	if len(chunk.Data) > 0 {
		if s.streaming {
			s.ring.Write(chunk.Data)
		} else {
			s.buf = append(s.buf, chunk.Data...)
		}
	}

	if !chunk.EndStream {
		return
	}

	if enc := s.contentEncoding; enc != "" && enc != "identity" && !s.streaming {
		if decoded, err := compress.Decode(enc, s.buf); err == nil {
			s.buf = decoded
		}
	}

	var u TokenUsage
	switch {
	case s.streaming && s.kind == kindAnthropic:
		u = ExtractAnthropicMessagesSSE(s.ring.Head(), s.ring.Tail())
	case s.streaming && s.endpoint == match.EndpointResponses:
		u = ExtractOpenAIResponsesSSE(s.ring.Head(), s.ring.Tail())
	case s.streaming:
		u = ExtractOpenAIChatCompletionsSSE(s.ring.Head(), s.ring.Tail())
	case s.kind == kindAnthropic:
		u = ExtractAnthropicMessagesJSON(s.buf)
	case s.endpoint == match.EndpointResponses:
		u = ExtractOpenAIResponsesJSON(s.buf)
	case s.endpoint == match.EndpointEmbeddings:
		u = ExtractOpenAIEmbeddingsJSON(s.buf)
	case s.endpoint == match.EndpointImages:
		emitImageGeneration(w, s.buf)
		return
	default:
		u = ExtractOpenAIChatCompletionsJSON(s.buf)
	}

	EmitUsage(w, u)
}

// EmitUsage writes token usage to Envoy counters and dynamic metadata. The HTTP
// meter calls this after parsing response bodies; orange-responsesws calls it from its
// inbound stream bridge after the sidecar has inspected WebSocket frames.
func EmitUsage(w *up.Writer, u TokenUsage) {
	for _, inc := range []struct {
		id  up.MetricID
		val uint32
	}{
		{inputTokensID, u.Input},
		{outputTokensID, u.Output},
		{cachedInputID, u.CachedInput},
		{audioInputID, u.AudioInput},
		{imageInputID, u.ImageInput},
		{reasoningOutputID, u.ReasoningOutput},
		{audioOutputID, u.AudioOutput},
		{acceptedPredictionOutputID, u.AcceptedPredictionOutput},
		{rejectedPredictionOutputID, u.RejectedPredictionOutput},
		{cacheCreationInputID, u.CacheCreationInput},
		{cacheReadInputID, u.CacheReadInput},
		{cacheEphemeral5mID, u.CacheEphemeral5m},
		{cacheEphemeral1hID, u.CacheEphemeral1h},
	} {
		if inc.val > 0 {
			w.IncrementCounter(inc.id, uint64(inc.val))
		}
	}
	w.SetMetadata("orange_meter", "input_tokens", strconv.FormatUint(uint64(u.Input), 10))
	w.SetMetadata("orange_meter", "output_tokens", strconv.FormatUint(uint64(u.Output), 10))
	if u.CachedInput > 0 {
		w.SetMetadata("orange_meter", "cached_input_tokens", strconv.FormatUint(uint64(u.CachedInput), 10))
	}
	if u.ImageInput > 0 {
		w.SetMetadata("orange_meter", "image_input_tokens", strconv.FormatUint(uint64(u.ImageInput), 10))
	}
	if u.ReasoningOutput > 0 {
		w.SetMetadata("orange_meter", "reasoning_output_tokens", strconv.FormatUint(uint64(u.ReasoningOutput), 10))
	}
	if u.CacheCreationInput > 0 {
		w.SetMetadata("orange_meter", "cache_creation_input_tokens", strconv.FormatUint(uint64(u.CacheCreationInput), 10))
	}
	if u.CacheReadInput > 0 {
		w.SetMetadata("orange_meter", "cache_read_input_tokens", strconv.FormatUint(uint64(u.CacheReadInput), 10))
	}
}

// resolveKind reads Decision.ProviderKind from the orange dynamic metadata
// (match.MetadataKeyProvider, log field "provider") and maps it to a
// providerKind. This tells the meter which JSON shape the response body has
// after the adapter has run. It is NOT the config backend name — for that see
// Decision.ProviderBackend / match.MetadataKeyUpstream (log field "upstream").
func resolveKind(w *up.Writer) providerKind {
	v, ok := w.GetMetadataString(up.MetadataSourceDynamic, match.MetadataNamespace, match.MetadataKeyProvider)
	if !ok {
		return kindOpenAI
	}
	switch v.String() {
	case "anthropic", "awsanthropic", "gcpanthropic":
		return kindAnthropic
	default:
		return kindOpenAI
	}
}

// resolveEndpoint reads the endpoint discriminator written by the match filter.
// Returns match.EndpointResponses for POST /v1/responses; empty string otherwise.
func resolveEndpoint(w *up.Writer) string {
	v, ok := w.GetMetadataString(up.MetadataSourceDynamic, match.MetadataNamespace, match.MetadataKeyEndpoint)
	if !ok {
		return ""
	}
	return v.String()
}

// emitImageGeneration extracts image generation metrics from the response body,
// supplements size/quality from request-side metadata when the response omits
// them (DALL-E 2/3), and emits all observable data as counters and metadata.
func emitImageGeneration(w *up.Writer, body []byte) {
	r := ExtractOpenAIImageGenerationsJSON(body)
	// Fall back to native Gemini format when the adapt filter has not yet
	// translated the response body (meter runs before adapt on the response path).
	if r.Count == 0 && r.Tokens.Input == 0 && r.Tokens.Output == 0 {
		if gr := ExtractGeminiImageGenerationsJSON(body); gr.Count > 0 || gr.Tokens.Input > 0 {
			r = gr
		}
	}

	// Fall back to request-side metadata for size/quality (absent on DALL-E responses).
	if r.Size == "" {
		if v, ok := w.GetMetadataString(up.MetadataSourceDynamic, match.MetadataNamespace, match.MetadataKeyImageSize); ok {
			r.Size = v.String()
		}
	}
	if r.Quality == "" {
		if v, ok := w.GetMetadataString(up.MetadataSourceDynamic, match.MetadataNamespace, match.MetadataKeyImageQuality); ok {
			r.Quality = v.String()
		}
	}

	if r.Count > 0 {
		w.IncrementCounter(imageCountID, uint64(r.Count))
		w.SetMetadata("orange_meter", "response_modalities", "image")
		w.SetMetadata("orange_meter", "image_count", strconv.FormatUint(uint64(r.Count), 10))
	}
	if r.Size != "" {
		w.SetMetadata("orange_meter", "image_size", r.Size)
	}
	if r.Quality != "" {
		w.SetMetadata("orange_meter", "image_quality", r.Quality)
	}

	// Emit token usage for models that provide it (e.g. gpt-image-1).
	EmitUsage(w, r.Tokens)
}
