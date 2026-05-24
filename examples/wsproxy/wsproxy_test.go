// Package wsproxy_test contains unit tests for the wsproxy filter.
package wsproxy_test

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"

	wsproxy "github.com/dio/transit/examples/wsproxy"
)

func makeResponseCompleted(inputTokens, outputTokens int64) []byte {
	b, _ := json.Marshal(map[string]any{
		"type": "response.completed",
		"response": map[string]any{
			"id": "resp_test",
			"usage": map[string]any{
				"input_tokens":  inputTokens,
				"output_tokens": outputTokens,
				"total_tokens":  inputTokens + outputTokens,
			},
		},
	})
	return b
}

func TestSessionTap_ExtractsUsage(t *testing.T) {
	tap := wsproxy.NewSessionTap()
	frame := makeResponseCompleted(100, 42)
	out := tap.FeedUpstream(frame)
	require.Equal(t, frame, out, "frame must be returned unchanged")
	in, out2, turns := tap.Counts()
	require.Equal(t, int64(100), in)
	require.Equal(t, int64(42), out2)
	require.Equal(t, int64(1), turns)
}

func TestSessionTap_AccumulatesAcrossMultipleTurns(t *testing.T) {
	tap := wsproxy.NewSessionTap()
	tap.FeedUpstream(makeResponseCompleted(10, 5))
	tap.FeedUpstream(makeResponseCompleted(20, 8))
	tap.FeedUpstream(makeResponseCompleted(30, 12))
	in, out, turns := tap.Counts()
	require.Equal(t, int64(60), in)
	require.Equal(t, int64(25), out)
	require.Equal(t, int64(3), turns)
}

func TestSessionTap_FastPath_SkipsOtherFrames(t *testing.T) {
	tap := wsproxy.NewSessionTap()
	frames := [][]byte{
		[]byte(`{"type":"response.output_item.delta","delta":{"text":"hello"}}`),
		[]byte(`{"type":"response.created","response":{"id":"resp_1"}}`),
		[]byte(`{"type":"response.output_item.done"}`),
	}
	for _, f := range frames {
		tap.FeedUpstream(f)
	}
	in, out, turns := tap.Counts()
	require.Equal(t, int64(0), in)
	require.Equal(t, int64(0), out)
	require.Equal(t, int64(0), turns)
}

func TestSessionTap_WrongTypeSameSubstring(t *testing.T) {
	// Frame contains the substring "response.completed" in a field value
	// but type is something else — must not update counts.
	tap := wsproxy.NewSessionTap()
	frame := []byte(`{"type":"response.output_item.delta","note":"see response.completed for usage"}`)
	tap.FeedUpstream(frame)
	in, out, turns := tap.Counts()
	require.Equal(t, int64(0), in)
	require.Equal(t, int64(0), out)
	require.Equal(t, int64(0), turns)
}

func TestSessionTap_MalformedJSON_ForwardsFrame(t *testing.T) {
	tap := wsproxy.NewSessionTap()
	frame := []byte(`{"type":"response.completed","response":INVALID}`)
	out := tap.FeedUpstream(frame)
	require.Equal(t, frame, out, "malformed frame must be forwarded unchanged")
	_, _, turns := tap.Counts()
	require.Equal(t, int64(0), turns, "malformed frame must not update counts")
}

func TestSessionTap_FeedDownstream_Passthrough(t *testing.T) {
	tap := wsproxy.NewSessionTap()
	frame := []byte(`{"type":"response.create","model":"gpt-4.1","input":[]}`)
	out := tap.FeedDownstream(frame)
	require.Equal(t, frame, out)
}
