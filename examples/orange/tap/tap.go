// Package tap is the streaming and non-streaming response observer for orange.
//
// It extracts LLM token usage from both response shapes:
//   - Streaming (Content-Type: text/event-stream): head+tail ring buffer via
//     up/buffer.HeadTail; parses OpenAI and Anthropic SSE formats.
//   - Non-streaming (Content-Type: application/json): accumulates the full
//     body across chunks; parses the top-level usage object.
//
// On stream completion the extracted counts are emitted as Envoy counters
// (orange_input_tokens, orange_output_tokens) and as dynamic metadata under
// the "orange_tap" namespace.
//
// The observer runs on the Envoy worker thread with zero added latency:
// chunks are forwarded to the downstream client as they arrive.
package tap

import (
	"strings"

	"github.com/dio/transit/up"
	"github.com/dio/transit/up/buffer"
)

// ExtensionName is the Envoy filter name.
const ExtensionName = "orange-tap"

var (
	inputTokensID  up.MetricID
	outputTokensID up.MetricID
)

func init() {
	up.Register(
		ExtensionName,
		func(_ *up.Writer, _ *up.Request) {},
		up.WithConfig(func(h up.ConfigHandle) error {
			var err error
			inputTokensID, err = h.DefineCounter("orange_input_tokens")
			if err != nil {
				return err
			}
			outputTokensID, err = h.DefineCounter("orange_output_tokens")
			return err
		}),
		up.WithResponse(tapResponse),
	)
}

// streamState is per-request state stored in chunk.Context across callbacks.
type streamState struct {
	ring      *buffer.HeadTail // non-nil on the streaming path
	buf       []byte           // accumulator on the non-streaming path
	streaming bool
	skip      bool
}

// tapResponse is the response observer. It is called:
//   - Once on response headers (Data == nil, EndStream == false): allocate state.
//   - Once per body chunk: feed data into ring or buf.
//   - Once with EndStream == true: extract usage and emit metrics.
func tapResponse(w *up.Writer, chunk *up.ResponseChunk) {
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

	var u TokenUsage
	if s.streaming {
		u = ExtractUsageFromSSE(s.ring.Head(), s.ring.Tail())
	} else {
		u = ExtractUsageFromJSON(s.buf)
	}

	if u.Input > 0 {
		w.IncrementCounter(inputTokensID, uint64(u.Input))
	}
	if u.Output > 0 {
		w.IncrementCounter(outputTokensID, uint64(u.Output))
	}
	w.SetMetadata("orange_tap", "input_tokens", u.Input)
	w.SetMetadata("orange_tap", "output_tokens", u.Output)
}
