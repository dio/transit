package meter

import "bytes"

// TokenUsage holds usage counts extracted from an LLM response. Despite the
// name, not all fields are strictly "tokens" — image generation models report
// image-input counts in the same structure. Provider-specific fields are zero
// when the provider does not report them.
type TokenUsage struct {
	// Common
	Input  uint32
	Output uint32

	// OpenAI: prompt_tokens_details
	CachedInput uint32 // prompt_tokens_details.cached_tokens
	AudioInput  uint32 // prompt_tokens_details.audio_tokens
	ImageInput  uint32 // input_tokens_details.image_tokens (image generation)

	// OpenAI: completion_tokens_details
	ReasoningOutput          uint32 // completion_tokens_details.reasoning_tokens
	AudioOutput              uint32 // completion_tokens_details.audio_tokens
	AcceptedPredictionOutput uint32 // completion_tokens_details.accepted_prediction_tokens
	RejectedPredictionOutput uint32 // completion_tokens_details.rejected_prediction_tokens

	// Anthropic: cache token breakdown
	CacheCreationInput uint32 // cache_creation_input_tokens (standard cache write)
	CacheReadInput     uint32 // cache_read_input_tokens (cache hit, billed at reduced rate)
	CacheEphemeral5m   uint32 // cache_creation.ephemeral_5m_input_tokens
	CacheEphemeral1h   uint32 // cache_creation.ephemeral_1h_input_tokens
}

// scanLines iterates newline-delimited records in data, stripping trailing CR.
func scanLines(data []byte, fn func([]byte)) {
	for {
		idx := bytes.IndexByte(data, '\n')
		if idx < 0 {
			return
		}
		fn(bytes.TrimRight(data[:idx], "\r"))
		data = data[idx+1:]
	}
}
