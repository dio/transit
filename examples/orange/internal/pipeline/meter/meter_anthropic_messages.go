package meter

import (
	"bytes"
	"encoding/json"
)

// ExtractAnthropicMessagesJSON extracts token usage from a non-streaming
// Anthropic Messages API response body.
//
// Messages API usage shape (https://docs.anthropic.com/en/api/messages):
//
//	{"usage":{"input_tokens":N,"output_tokens":M,
//	           "cache_creation_input_tokens":C,"cache_read_input_tokens":R,
//	           "cache_creation":{"ephemeral_5m_input_tokens":E5,
//	                             "ephemeral_1h_input_tokens":E1}}}
//
// RATIONALE for Messages suffix: naming mirrors ExtractOpenAIChatCompletionsJSON
// and ExtractOpenAIResponsesJSON — each extractor is named after the specific
// API endpoint it handles. Anthropic currently exposes one HTTP endpoint
// (POST /v1/messages); the suffix keeps the pattern uniform and leaves room
// for a future distinct API without a rename.
func ExtractAnthropicMessagesJSON(body []byte) TokenUsage {
	var resp struct {
		Usage *anthropicMessagesUsage `json:"usage"`
	}
	if json.Unmarshal(body, &resp) != nil || resp.Usage == nil {
		return TokenUsage{}
	}
	return resp.Usage.toTokenUsage()
}

// anthropicMessagesUsage mirrors the Anthropic Messages API usage object.
type anthropicMessagesUsage struct {
	InputTokens              uint32 `json:"input_tokens"`
	OutputTokens             uint32 `json:"output_tokens"`
	CacheCreationInputTokens uint32 `json:"cache_creation_input_tokens"`
	CacheReadInputTokens     uint32 `json:"cache_read_input_tokens"`
	CacheCreation            struct {
		Ephemeral5mInputTokens uint32 `json:"ephemeral_5m_input_tokens"`
		Ephemeral1hInputTokens uint32 `json:"ephemeral_1h_input_tokens"`
	} `json:"cache_creation"`
}

func (u *anthropicMessagesUsage) toTokenUsage() TokenUsage {
	return TokenUsage{
		Input:              u.InputTokens,
		Output:             u.OutputTokens,
		CacheCreationInput: u.CacheCreationInputTokens,
		CacheReadInput:     u.CacheReadInputTokens,
		CacheEphemeral5m:   u.CacheCreation.Ephemeral5mInputTokens,
		CacheEphemeral1h:   u.CacheCreation.Ephemeral1hInputTokens,
	}
}

// ExtractAnthropicMessagesSSE extracts token usage from an Anthropic Messages
// API SSE stream.
//
// The Messages API splits usage across two events:
//   - message_start (head of stream): input_tokens and all cache token fields.
//   - message_delta (tail of stream): output_tokens.
//
// Both windows are scanned; head carries input, tail carries output.
func ExtractAnthropicMessagesSSE(head, tail []byte) TokenUsage {
	var u TokenUsage

	// Head: message_start carries input tokens and cache tokens.
	var curEvent string
	scanLines(head, func(line []byte) {
		switch {
		case bytes.HasPrefix(line, []byte("event: ")):
			curEvent = string(line[7:])
		case u.Input == 0 && curEvent == "message_start" && bytes.HasPrefix(line, []byte("data: ")):
			var msg struct {
				Message struct {
					Usage *anthropicMessagesUsage `json:"usage"`
				} `json:"message"`
			}
			if json.Unmarshal(line[6:], &msg) == nil && msg.Message.Usage != nil {
				u = msg.Message.Usage.toTokenUsage()
			}
		}
	})

	// Tail: message_delta carries output_tokens.
	curEvent = ""
	scanLines(tail, func(line []byte) {
		if bytes.HasPrefix(line, []byte("event: ")) {
			curEvent = string(line[7:])
			return
		}
		if u.Output == 0 && curEvent == "message_delta" && bytes.HasPrefix(line, []byte("data: ")) {
			var delta struct {
				Usage struct {
					OutputTokens uint32 `json:"output_tokens"`
				} `json:"usage"`
			}
			if json.Unmarshal(line[6:], &delta) == nil {
				u.Output = delta.Usage.OutputTokens
			}
		}
	})

	return u
}
