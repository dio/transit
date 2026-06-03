package meter

import (
	"bytes"
	"encoding/json"
)

// ExtractOpenAIJSON extracts token usage from a non-streaming OpenAI-format response body.
// Handles Chat Completions (prompt_tokens/completion_tokens) and Responses API (input_tokens/output_tokens),
// including the nested prompt_tokens_details and completion_tokens_details objects.
func ExtractOpenAIJSON(body []byte) TokenUsage {
	var resp struct {
		Usage *openAIUsage `json:"usage"`
	}
	if json.Unmarshal(body, &resp) != nil || resp.Usage == nil {
		return TokenUsage{}
	}
	return resp.Usage.toTokenUsage()
}

// openAIUsage mirrors the OpenAI usage object (Chat Completions and Responses APIs).
type openAIUsage struct {
	PromptTokens        uint32 `json:"prompt_tokens"`
	CompletionTokens    uint32 `json:"completion_tokens"`
	InputTokens         uint32 `json:"input_tokens"`
	OutputTokens        uint32 `json:"output_tokens"`
	PromptTokensDetails struct {
		CachedTokens uint32 `json:"cached_tokens"`
		AudioTokens  uint32 `json:"audio_tokens"`
	} `json:"prompt_tokens_details"`
	CompletionTokensDetails struct {
		ReasoningTokens          uint32 `json:"reasoning_tokens"`
		AudioTokens              uint32 `json:"audio_tokens"`
		AcceptedPredictionTokens uint32 `json:"accepted_prediction_tokens"`
		RejectedPredictionTokens uint32 `json:"rejected_prediction_tokens"`
	} `json:"completion_tokens_details"`
}

func (u *openAIUsage) toTokenUsage() TokenUsage {
	return TokenUsage{
		Input:                    u.PromptTokens + u.InputTokens,
		Output:                   u.CompletionTokens + u.OutputTokens,
		CachedInput:              u.PromptTokensDetails.CachedTokens,
		AudioInput:               u.PromptTokensDetails.AudioTokens,
		ReasoningOutput:          u.CompletionTokensDetails.ReasoningTokens,
		AudioOutput:              u.CompletionTokensDetails.AudioTokens,
		AcceptedPredictionOutput: u.CompletionTokensDetails.AcceptedPredictionTokens,
		RejectedPredictionOutput: u.CompletionTokensDetails.RejectedPredictionTokens,
	}
}

// ExtractOpenAISSE extracts token usage from an OpenAI-format SSE stream.
// OpenAI sends usage in the final data chunk before [DONE] when
// stream_options.include_usage is set. Both field name variants are handled:
// prompt_tokens/completion_tokens (Chat Completions) and
// input_tokens/output_tokens (Responses API).
func ExtractOpenAISSE(head, tail []byte) TokenUsage {
	var u TokenUsage

	// Usage appears near the end of the stream; scan tail first, then head as
	// fallback for very short responses where tail == head.
	for _, buf := range [2][]byte{tail, head} {
		if u.Input > 0 && u.Output > 0 {
			break
		}
		scanLines(buf, func(line []byte) {
			if !bytes.HasPrefix(line, []byte("data: ")) {
				return
			}
			data := line[6:]
			if bytes.Equal(data, []byte("[DONE]")) {
				return
			}
			var ck struct {
				Usage *openAIUsage `json:"usage"`
			}
			if json.Unmarshal(data, &ck) != nil || ck.Usage == nil {
				return
			}
			if u.Input == 0 && u.Output == 0 {
				u = ck.Usage.toTokenUsage()
			}
		})
	}
	return u
}
