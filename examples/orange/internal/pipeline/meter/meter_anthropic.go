package meter

import (
	"bytes"
	"encoding/json"
)

// ExtractAnthropicJSON extracts token usage from a non-streaming Anthropic-format response body.
func ExtractAnthropicJSON(body []byte) TokenUsage {
	var resp struct {
		Usage *anthropicUsage `json:"usage"`
	}
	if json.Unmarshal(body, &resp) != nil || resp.Usage == nil {
		return TokenUsage{}
	}
	return resp.Usage.toTokenUsage()
}

// anthropicUsage mirrors the Anthropic Messages API usage object.
type anthropicUsage struct {
	InputTokens              uint32 `json:"input_tokens"`
	OutputTokens             uint32 `json:"output_tokens"`
	CacheCreationInputTokens uint32 `json:"cache_creation_input_tokens"`
	CacheReadInputTokens     uint32 `json:"cache_read_input_tokens"`
	CacheCreation            struct {
		Ephemeral5mInputTokens uint32 `json:"ephemeral_5m_input_tokens"`
		Ephemeral1hInputTokens uint32 `json:"ephemeral_1h_input_tokens"`
	} `json:"cache_creation"`
}

func (u *anthropicUsage) toTokenUsage() TokenUsage {
	return TokenUsage{
		Input:              u.InputTokens,
		Output:             u.OutputTokens,
		CacheCreationInput: u.CacheCreationInputTokens,
		CacheReadInput:     u.CacheReadInputTokens,
		CacheEphemeral5m:   u.CacheCreation.Ephemeral5mInputTokens,
		CacheEphemeral1h:   u.CacheCreation.Ephemeral1hInputTokens,
	}
}

// ExtractAnthropicSSE extracts token usage from an Anthropic-format SSE stream.
// Input tokens arrive in the message_start event (head of stream).
// Output tokens arrive in the message_delta event (tail of stream).
func ExtractAnthropicSSE(head, tail []byte) TokenUsage {
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
					Usage *anthropicUsage `json:"usage"`
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
