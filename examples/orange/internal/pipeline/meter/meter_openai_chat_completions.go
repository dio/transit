package meter

import (
	"bytes"
	"encoding/json"
)

// ExtractOpenAIChatCompletionsJSON extracts token usage from a non-streaming OpenAI Chat
// Completions response body.
//
// Chat Completions usage shape (https://platform.openai.com/docs/api-reference/chat/object):
//
//	{"usage":{"prompt_tokens":N,"completion_tokens":M,
//	           "prompt_tokens_details":{"cached_tokens":C,"audio_tokens":A},
//	           "completion_tokens_details":{"reasoning_tokens":R,"audio_tokens":A,
//	                                        "accepted_prediction_tokens":AP,
//	                                        "rejected_prediction_tokens":RP}}}
//
// For the Responses API (POST /v1/responses) use [ExtractOpenAIResponsesJSON].
//
// RATIONALE for two separate functions: Chat Completions and Responses API
// share the concept of "cached input" and "reasoning output" but use different
// JSON container names (prompt_tokens_details vs input_tokens_details,
// completion_tokens_details vs output_tokens_details). Merging them into one
// decoder would require a union struct that silently drops the wrong set of
// fields, making the schema contract unclear. Keeping separate functions
// documents which API each extractor is for and lets the meter route
// explicitly by endpoint metadata.
func ExtractOpenAIChatCompletionsJSON(body []byte) TokenUsage {
	var resp struct {
		Usage *chatCompletionsUsage `json:"usage"`
	}
	if json.Unmarshal(body, &resp) != nil || resp.Usage == nil {
		return TokenUsage{}
	}
	return resp.Usage.toTokenUsage()
}

// chatCompletionsUsage mirrors the Chat Completions usage object.
// Do not add Responses API fields here; use responsesAPIUsage in
// meter_openai_responses.go instead.
type chatCompletionsUsage struct {
	PromptTokens        uint32 `json:"prompt_tokens"`
	CompletionTokens    uint32 `json:"completion_tokens"`
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

func (u *chatCompletionsUsage) toTokenUsage() TokenUsage {
	return TokenUsage{
		Input:                    u.PromptTokens,
		Output:                   u.CompletionTokens,
		CachedInput:              u.PromptTokensDetails.CachedTokens,
		AudioInput:               u.PromptTokensDetails.AudioTokens,
		ReasoningOutput:          u.CompletionTokensDetails.ReasoningTokens,
		AudioOutput:              u.CompletionTokensDetails.AudioTokens,
		AcceptedPredictionOutput: u.CompletionTokensDetails.AcceptedPredictionTokens,
		RejectedPredictionOutput: u.CompletionTokensDetails.RejectedPredictionTokens,
	}
}

// ExtractOpenAIChatCompletionsSSE extracts token usage from an OpenAI Chat Completions SSE
// stream.
//
// Chat Completions sends usage in the final data chunk before [DONE] when
// stream_options.include_usage is set:
//
//	data: {"choices":[],"usage":{"prompt_tokens":N,"completion_tokens":M,...}}
//	data: [DONE]
//
// For the Responses API SSE stream use [ExtractOpenAIResponsesSSE]. The two
// formats differ both in field names and in event structure (top-level usage
// chunk here vs nested response.completed event in the Responses API); a
// single function would need heuristics to tell them apart.
func ExtractOpenAIChatCompletionsSSE(head, tail []byte) TokenUsage {
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
				Usage *chatCompletionsUsage `json:"usage"`
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
