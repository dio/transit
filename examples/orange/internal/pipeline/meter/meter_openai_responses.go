package meter

import (
	"bytes"
	"encoding/json"
)

// ExtractOpenAIResponsesJSON extracts token usage from a non-streaming OpenAI
// Responses API response body.
//
// Responses API usage shape (https://platform.openai.com/docs/api-reference/responses):
//
//	{"usage":{"input_tokens":N,"output_tokens":M,"total_tokens":T,
//	           "input_tokens_details":{"cached_tokens":C},
//	           "output_tokens_details":{"reasoning_tokens":R}}}
//
// Billing note: reasoning_tokens are already counted within output_tokens and
// must not be billed separately. The effective cost formula is:
//
//	(input_tokens - cached_tokens) × InputRate
//	+ cached_tokens × CachedInputRate
//	+ output_tokens × OutputRate
//
// For Chat Completions (POST /v1/chat/completions) use [ExtractOpenAIChatCompletionsJSON].
func ExtractOpenAIResponsesJSON(body []byte) TokenUsage {
	var resp struct {
		Usage *responsesAPIUsage `json:"usage"`
	}
	if json.Unmarshal(body, &resp) != nil || resp.Usage == nil {
		return TokenUsage{}
	}
	return resp.Usage.toTokenUsage()
}

// responsesAPIUsage mirrors the Responses API usage object.
// Do not add Chat Completions fields here; use chatCompletionsUsage in
// meter_openai_chat_completions.go instead.
type responsesAPIUsage struct {
	InputTokens  uint32 `json:"input_tokens"`
	OutputTokens uint32 `json:"output_tokens"`
	InputTokensDetails struct {
		CachedTokens uint32 `json:"cached_tokens"`
	} `json:"input_tokens_details"`
	OutputTokensDetails struct {
		// ReasoningTokens are included in OutputTokens for billing; tracked
		// separately for observability only.
		ReasoningTokens uint32 `json:"reasoning_tokens"`
	} `json:"output_tokens_details"`
}

func (u *responsesAPIUsage) toTokenUsage() TokenUsage {
	return TokenUsage{
		Input:           u.InputTokens,
		Output:          u.OutputTokens,
		CachedInput:     u.InputTokensDetails.CachedTokens,
		ReasoningOutput: u.OutputTokensDetails.ReasoningTokens,
	}
}

// ExtractOpenAIResponsesSSE extracts token usage from an OpenAI Responses API
// SSE stream.
//
// The Responses API emits a response.completed event at end of stream:
//
//	event: response.completed
//	data: {"type":"response.completed","response":{"usage":{"input_tokens":N,
//	        "input_tokens_details":{"cached_tokens":C},"output_tokens":M,
//	        "output_tokens_details":{"reasoning_tokens":R},...}}}
//
// For Chat Completions SSE use [ExtractOpenAIChatCompletionsSSE], which reads the final
// data chunk before [DONE] instead.
func ExtractOpenAIResponsesSSE(head, tail []byte) TokenUsage {
	var u TokenUsage

	// Usage is in the response.completed event near the end of the stream;
	// scan tail first, then head as fallback for short responses.
	for _, buf := range [2][]byte{tail, head} {
		if u.Input > 0 && u.Output > 0 {
			break
		}
		scanLines(buf, func(line []byte) {
			if !bytes.HasPrefix(line, []byte("data: ")) {
				return
			}
			data := line[6:]
			// Responses API: usage is under response.usage in the
			// response.completed event, not at the top level of the data object.
			var ck struct {
				Response *struct {
					Usage *responsesAPIUsage `json:"usage"`
				} `json:"response"`
			}
			if json.Unmarshal(data, &ck) != nil || ck.Response == nil || ck.Response.Usage == nil {
				return
			}
			if u.Input == 0 && u.Output == 0 {
				u = ck.Response.Usage.toTokenUsage()
			}
		})
	}
	return u
}
