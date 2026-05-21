// Package ssetap taps an SSE response stream to extract LLM token usage without
// buffering the full body. It supports both Anthropic and OpenAI streaming formats.
//
// Input tokens appear near the START of the stream (Anthropic message_start,
// OpenAI first usage chunk); output tokens appear near the END (message_delta,
// final usage chunk). The filter uses a HeadTail buffer — 8 KB head + 64 KB
// tail — so the middle of a large stream is never stored.
//
// On stream completion the extracted counts are emitted as Envoy counters
// (sse_tap_input_tokens, sse_tap_output_tokens) and as dynamic metadata under
// the "sse_tap" namespace.
//
// The response observer runs on the Envoy worker thread with zero added latency:
// chunks are forwarded to the downstream client as they arrive.
package ssetap

import (
	"bytes"
	"encoding/json"
	"strings"

	"github.com/dio/transit/up"
	"github.com/dio/transit/up/buffer"
)

// ExtensionName is the Envoy filter name.
const ExtensionName = "sse-tap"

// TokenUsage holds token counts extracted from a streaming LLM response.
type TokenUsage struct {
	Input  uint32
	Output uint32
}

// Metric IDs defined once at config time.
var (
	inputTokensID  up.MetricID
	outputTokensID up.MetricID
)

func init() {
	up.RegisterWithConfig(
		ExtensionName,
		func(h up.ConfigHandle) error {
			var err error
			inputTokensID, err = h.DefineCounter("sse_tap_input_tokens")
			if err != nil {
				return err
			}
			outputTokensID, err = h.DefineCounter("sse_tap_output_tokens")
			return err
		},
		func(w *up.Writer, _ *up.Request) {
			w.SetRequestHeader("x-sse-tap", "1")
		},
		tapResponse,
	)
}

// ringState is per-request state stored in chunk.Context across callbacks.
type ringState struct {
	ring *buffer.HeadTail
	skip bool
}

// tapResponse is the response observer. It is called:
//   - Once on response headers (Data == nil, EndStream == false): allocate ring.
//   - Once per body chunk: feed data into ring.
//   - Once with EndStream == true: parse ring and emit metrics.
func tapResponse(w *up.Writer, chunk *up.ResponseChunk) {
	if chunk.Data == nil && !chunk.EndStream {
		// Headers phase: allocate per-request state.
		s := &ringState{}
		if !strings.Contains(chunk.ContentType, "text/event-stream") {
			s.skip = true
		} else {
			s.ring = buffer.NewHeadTail(8*1024, 64*1024)
		}
		*chunk.Context = s
		return
	}

	s, ok := (*chunk.Context).(*ringState)
	if !ok || s == nil || s.skip {
		return
	}

	if len(chunk.Data) > 0 {
		s.ring.Write(chunk.Data)
	}

	if !chunk.EndStream {
		return
	}

	u := ExtractUsage(s.ring.Head(), s.ring.Tail())

	if u.Input > 0 {
		w.IncrementCounter(inputTokensID, uint64(u.Input))
	}
	if u.Output > 0 {
		w.IncrementCounter(outputTokensID, uint64(u.Output))
	}
	w.SetMetadata("sse_tap", "input_tokens", u.Input)
	w.SetMetadata("sse_tap", "output_tokens", u.Output)
}

// ExtractUsage scans head for input tokens and tail for output tokens.
// Handles both OpenAI and Anthropic SSE formats.
// Exported for unit testing independently of Envoy.
func ExtractUsage(head, tail []byte) TokenUsage {
	var u TokenUsage

	// Scan head: Anthropic message_start carries input tokens.
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

	// Scan tail: output tokens (both formats) + OpenAI input if not found in head.
	curEvent = ""
	scanLines(tail, func(line []byte) {
		if !bytes.HasPrefix(line, []byte("data: ")) &&
			!bytes.HasPrefix(line, []byte("event: ")) {
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

		// OpenAI: usage appears in a data chunk.
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

		// Anthropic message_delta: output tokens.
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
