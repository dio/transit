package translator

import (
	"encoding/json"
	"strconv"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestOpenAIResponsesToOpenAI_RequestHeaders(t *testing.T) {
	o := NewResponsesOpenAIToOpenAITranslator(ProviderConfig{PathPrefix: "v1"})
	hdrs, err := o.RequestHeaders(map[string]string{"content-type": "application/json"})
	require.NoError(t, err)
	require.Nil(t, hdrs)
}

func TestOpenAIResponsesToOpenAI_RequestBody_PathSet(t *testing.T) {
	o := NewResponsesOpenAIToOpenAITranslator(ProviderConfig{PathPrefix: "v1"})
	raw := []byte(`{"model":"gpt-4o","input":"hello"}`)
	hdrs, body, err := o.RequestBody(raw)
	require.NoError(t, err)
	require.Nil(t, body, "no mutation when no model override")
	require.NotEmpty(t, hdrs)
	found := false
	for _, h := range hdrs {
		if h.Name == pathHeaderName {
			require.Equal(t, "/v1/responses", h.Value)
			found = true
		}
	}
	require.True(t, found, "expected :path header set to /v1/responses")
}

func TestOpenAIResponsesToOpenAI_RequestBody_NoPathPrefix(t *testing.T) {
	o := NewResponsesOpenAIToOpenAITranslator(ProviderConfig{})
	raw := []byte(`{"model":"gpt-4o","input":"hello"}`)
	hdrs, _, err := o.RequestBody(raw)
	require.NoError(t, err)
	for _, h := range hdrs {
		if h.Name == pathHeaderName {
			require.Equal(t, "/responses", h.Value)
			return
		}
	}
	t.Fatal("expected :path header")
}

func TestOpenAIResponsesToOpenAI_RequestBody_ModelOverride(t *testing.T) {
	o := NewResponsesOpenAIToOpenAITranslator(ProviderConfig{PathPrefix: "v1", BackendModel: "gpt-4o-2024-11-20"})
	raw := []byte(`{"model":"gpt-4o","input":"hello"}`)
	hdrs, body, err := o.RequestBody(raw)
	require.NoError(t, err)
	require.NotNil(t, body)

	var got struct {
		Model string `json:"model"`
	}
	require.NoError(t, json.Unmarshal(body, &got))
	require.Equal(t, "gpt-4o-2024-11-20", got.Model)

	// content-length header present and correct
	found := false
	for _, h := range hdrs {
		if h.Name == contentLengthHeaderName {
			require.Equal(t, strconv.Itoa(len(body)), h.Value)
			found = true
		}
	}
	require.True(t, found, "expected content-length header when body is mutated")
}

func TestOpenAIResponsesToOpenAI_RequestBody_StreamFlag(t *testing.T) {
	o := NewResponsesOpenAIToOpenAITranslator(ProviderConfig{PathPrefix: "v1"}).(*openAIToOpenAITranslatorV1Responses)
	raw := []byte(`{"model":"gpt-4o","stream":true,"input":"hello"}`)
	_, _, err := o.RequestBody(raw)
	require.NoError(t, err)
	require.True(t, o.stream, "stream flag should be recorded")
}

func TestOpenAIResponsesToOpenAI_RequestBody_PreservesInput(t *testing.T) {
	// Verify that input, instructions, tools, store, previous_response_id are
	// preserved when a model override is applied (the only mutation).
	o := NewResponsesOpenAIToOpenAITranslator(ProviderConfig{BackendModel: "gpt-4o-2024-11-20"})
	raw := []byte(`{"model":"gpt-4o","input":"hello","instructions":"be helpful","store":true,"previous_response_id":"resp_xyz"}`)
	_, body, err := o.RequestBody(raw)
	require.NoError(t, err)
	require.NotNil(t, body)

	var got map[string]any
	require.NoError(t, json.Unmarshal(body, &got))
	require.Equal(t, "gpt-4o-2024-11-20", got["model"])
	require.Equal(t, "hello", got["input"])
	require.Equal(t, "be helpful", got["instructions"])
	require.Equal(t, true, got["store"])
	require.Equal(t, "resp_xyz", got["previous_response_id"])
}

func TestOpenAIResponsesToOpenAI_ResponseHeaders(t *testing.T) {
	o := NewResponsesOpenAIToOpenAITranslator(ProviderConfig{})
	hdrs, err := o.ResponseHeaders(map[string]string{":status": "200", "content-type": "application/json"})
	require.NoError(t, err)
	require.Nil(t, hdrs)
}

func TestOpenAIResponsesToOpenAI_ResponseBody_JSON(t *testing.T) {
	o := NewResponsesOpenAIToOpenAITranslator(ProviderConfig{})
	body := []byte(`{"id":"resp_123","object":"response","output":[{"type":"message","content":[{"type":"text","text":"Hi"}]}],"usage":{"input_tokens":10,"output_tokens":5}}`)
	hdrs, out, err := o.ResponseBody(body, true)
	require.NoError(t, err)
	require.Nil(t, hdrs, "no header mutations for passthrough")
	require.Nil(t, out, "nil means forward original bytes unchanged")
}

func TestOpenAIResponsesToOpenAI_ResponseBody_SSE(t *testing.T) {
	o := NewResponsesOpenAIToOpenAITranslator(ProviderConfig{})
	chunk := []byte("event: response.output_text.delta\ndata: {\"delta\":\"Hello\"}\n\n")
	hdrs, out, err := o.ResponseBody(chunk, false)
	require.NoError(t, err)
	require.Nil(t, hdrs)
	require.Nil(t, out, "SSE chunks forwarded as-is")

	final := []byte("event: response.completed\ndata: {\"usage\":{\"input_tokens\":10,\"output_tokens\":5}}\n\n")
	hdrs, out, err = o.ResponseBody(final, true)
	require.NoError(t, err)
	require.Nil(t, hdrs)
	require.Nil(t, out)
}

func TestOpenAIResponsesToOpenAI_Init(t *testing.T) {
	_, err := New("openai:responses", ProviderConfig{PathPrefix: "v1"})
	require.NoError(t, err)
}

func TestNewForRoute_RespondsEndpoint(t *testing.T) {
	t.Run("openai+responses uses responses translator", func(t *testing.T) {
		tr, err := NewForRoute("openai", "responses", ProviderConfig{PathPrefix: "v1"})
		require.NoError(t, err)
		require.IsType(t, &openAIToOpenAITranslatorV1Responses{}, tr)
	})

	t.Run("openai+chat_completions falls back to schema-only translator", func(t *testing.T) {
		tr, err := NewForRoute("openai", "chat_completions", ProviderConfig{PathPrefix: "v1"})
		require.NoError(t, err)
		require.IsType(t, &openAIToOpenAITranslatorV1ChatCompletion{}, tr)
	})

	t.Run("openai with empty endpoint falls back to schema-only translator", func(t *testing.T) {
		tr, err := NewForRoute("openai", "", ProviderConfig{PathPrefix: "v1"})
		require.NoError(t, err)
		require.IsType(t, &openAIToOpenAITranslatorV1ChatCompletion{}, tr)
	})

	t.Run("unknown schema returns error", func(t *testing.T) {
		_, err := NewForRoute("unknown_schema", "responses", ProviderConfig{})
		require.Error(t, err)
	})
}
