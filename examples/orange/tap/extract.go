package tap

import (
	"bytes"
	"encoding/json"
)

// TokenUsage holds token counts extracted from an LLM response.
type TokenUsage struct {
	Input  uint32
	Output uint32
}

// ExtractUsageFromSSE scans head and tail bytes of an SSE stream for token counts.
// Handles OpenAI (prompt_tokens/completion_tokens and input_tokens/output_tokens)
// and Anthropic (message_start + message_delta) streaming formats.
//
// WS-F: consolidate with examples/sse-tap once the response observer API lands.
func ExtractUsageFromSSE(head, tail []byte) TokenUsage {
	var u TokenUsage

	// Head: Anthropic message_start carries input tokens.
	var curEvent string
	scanLines(head, func(line []byte) {
		switch {
		case bytes.HasPrefix(line, []byte("event: ")):
			curEvent = string(line[7:])
		case u.Input == 0 && curEvent == "message_start" && bytes.HasPrefix(line, []byte("data: ")):
			var msg struct {
				Message struct {
					Usage struct {
						InputTokens uint32 `json:"input_tokens"`
					} `json:"usage"`
				} `json:"message"`
			}
			if json.Unmarshal(line[6:], &msg) == nil && msg.Message.Usage.InputTokens > 0 {
				u.Input = msg.Message.Usage.InputTokens
			}
		}
	})

	// Tail: output tokens (both formats) + OpenAI input if not in head.
	curEvent = ""
	scanLines(tail, func(line []byte) {
		if !bytes.HasPrefix(line, []byte("data: ")) && !bytes.HasPrefix(line, []byte("event: ")) {
			return
		}
		if bytes.HasPrefix(line, []byte("event: ")) {
			curEvent = string(line[7:])
			return
		}
		data := line[6:]
		if bytes.Equal(data, []byte("[DONE]")) {
			return
		}

		if u.Input == 0 || u.Output == 0 {
			var ck struct {
				Usage *struct {
					PromptTokens     uint32 `json:"prompt_tokens"`
					CompletionTokens uint32 `json:"completion_tokens"`
					InputTokens      uint32 `json:"input_tokens"`
					OutputTokens     uint32 `json:"output_tokens"`
				} `json:"usage"`
			}
			if json.Unmarshal(data, &ck) == nil && ck.Usage != nil {
				if u.Input == 0 {
					u.Input = ck.Usage.PromptTokens + ck.Usage.InputTokens
				}
				if u.Output == 0 {
					u.Output = ck.Usage.CompletionTokens + ck.Usage.OutputTokens
				}
			}
		}

		if curEvent == "message_delta" && u.Output == 0 {
			var delta struct {
				Usage struct {
					OutputTokens uint32 `json:"output_tokens"`
				} `json:"usage"`
			}
			if json.Unmarshal(data, &delta) == nil {
				u.Output = delta.Usage.OutputTokens
			}
		}
	})

	return u
}

// ExtractUsageFromJSON extracts token counts from a non-streaming JSON response body.
// Handles OpenAI (usage.prompt_tokens / usage.completion_tokens) and
// Anthropic (usage.input_tokens / usage.output_tokens).
func ExtractUsageFromJSON(body []byte) TokenUsage {
	var u TokenUsage
	var resp struct {
		Usage *struct {
			PromptTokens     uint32 `json:"prompt_tokens"`
			CompletionTokens uint32 `json:"completion_tokens"`
			InputTokens      uint32 `json:"input_tokens"`
			OutputTokens     uint32 `json:"output_tokens"`
		} `json:"usage"`
	}
	if json.Unmarshal(body, &resp) == nil && resp.Usage != nil {
		u.Input = resp.Usage.PromptTokens + resp.Usage.InputTokens
		u.Output = resp.Usage.CompletionTokens + resp.Usage.OutputTokens
	}
	return u
}

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
