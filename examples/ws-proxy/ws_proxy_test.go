// Package wsproxy_test contains unit tests for the ws-proxy filter.
package wsproxy_test

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"

	wsproxy "github.com/dio/transit/examples/ws-proxy"
)

func makeResponseCompleted(inputTokens, outputTokens uint32) []byte {
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

func makeResponseCreate(model string) []byte {
	b, _ := json.Marshal(map[string]any{
		"type":  "response.create",
		"model": model,
		"input": []any{},
	})
	return b
}

// TestSessionTap_ExtractsModel verifies FeedClient extracts model from response.create.
func TestSessionTap_ExtractsModel(t *testing.T) {
	tap := wsproxy.NewSessionTap()
	require.Equal(t, "", tap.Model())
	tap.FeedClient(makeResponseCreate("gpt-4.1"))
	require.Equal(t, "gpt-4.1", tap.Model())
}

// TestSessionTap_ModelOnlyExtractedOnce verifies subsequent FeedClient calls are ignored.
func TestSessionTap_ModelOnlyExtractedOnce(t *testing.T) {
	tap := wsproxy.NewSessionTap()
	tap.FeedClient(makeResponseCreate("gpt-4.1"))
	tap.FeedClient(makeResponseCreate("gpt-4o"))
	require.Equal(t, "gpt-4.1", tap.Model(), "model must not be overwritten")
}

// TestSessionTap_ExtractsUsageFromResponseCompleted verifies FeedUpstream.
func TestSessionTap_ExtractsUsageFromResponseCompleted(t *testing.T) {
	tap := wsproxy.NewSessionTap()
	tap.FeedUpstream(makeResponseCompleted(100, 42))
	u := tap.Usage()
	require.Equal(t, uint32(100), u.InputTokens)
	require.Equal(t, uint32(42), u.OutputTokens)
}

// TestSessionTap_FastPath_SkipsOtherFrames verifies frames without the keyword
// are not parsed.
func TestSessionTap_FastPath_SkipsOtherUpstreamFrames(t *testing.T) {
	tap := wsproxy.NewSessionTap()
	frames := [][]byte{
		[]byte(`{"type":"response.output_item.delta","delta":{"text":"hello"}}`),
		[]byte(`{"type":"response.created","response":{"id":"resp_1"}}`),
		[]byte(`{"type":"response.output_item.done"}`),
	}
	for _, f := range frames {
		tap.FeedUpstream(f)
	}
	u := tap.Usage()
	require.Equal(t, uint32(0), u.InputTokens)
	require.Equal(t, uint32(0), u.OutputTokens)
}

// TestSessionTap_WrongTypeSameSubstring verifies substring match alone isn't enough.
func TestSessionTap_WrongTypeSameSubstring(t *testing.T) {
	tap := wsproxy.NewSessionTap()
	// Contains "response.completed" in a field value but type is different.
	frame := []byte(`{"type":"response.output_item.delta","note":"see response.completed for usage"}`)
	tap.FeedUpstream(frame)
	u := tap.Usage()
	require.Equal(t, uint32(0), u.InputTokens)
}

// TestSessionTap_MalformedJSON_DoesNotPanic verifies graceful handling.
func TestSessionTap_MalformedJSON_DoesNotPanic(t *testing.T) {
	tap := wsproxy.NewSessionTap()
	tap.FeedUpstream([]byte(`{"type":"response.completed","response":INVALID}`))
	u := tap.Usage()
	require.Equal(t, uint32(0), u.InputTokens, "malformed frame must not update counts")

	tap.FeedClient([]byte(`{"type":"response.create","model":INVALID}`))
	require.Equal(t, "", tap.Model(), "malformed client frame must not set model")
}

// TestResolveEnv verifies ${VAR} expansion.
func TestResolveEnv(t *testing.T) {
	t.Setenv("WS_PROXY_TEST_KEY", "sk-real")
	got := wsproxy.ResolveEnv("Bearer ${WS_PROXY_TEST_KEY}")
	require.Equal(t, "Bearer sk-real", got)

	// Unset var: leave unexpanded.
	got2 := wsproxy.ResolveEnv("Bearer ${WS_PROXY_UNSET}")
	require.Equal(t, "Bearer ${WS_PROXY_UNSET}", got2)
}
