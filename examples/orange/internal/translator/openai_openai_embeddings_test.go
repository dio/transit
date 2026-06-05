package translator

import (
	"encoding/json"
	"strconv"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestOpenAIEmbeddingsTranslator_RequestBody(t *testing.T) {
	t.Run("sets path header", func(t *testing.T) {
		tr := &openAIToOpenAITranslatorV1Embeddings{path: "/v1/embeddings"}
		raw := []byte(`{"model":"text-embedding-3-small","input":"hello"}`)
		hdrs, body, err := tr.RequestBody(raw)
		require.NoError(t, err)
		require.Nil(t, body)
		require.NotEmpty(t, hdrs)
		found := false
		for _, h := range hdrs {
			if h.Name == pathHeaderName {
				require.Equal(t, "/v1/embeddings", h.Value)
				found = true
			}
		}
		require.True(t, found, "expected :path header")
	})

	t.Run("overrides model when backendModel set", func(t *testing.T) {
		tr := &openAIToOpenAITranslatorV1Embeddings{backendModel: "text-embedding-3-large", path: "/v1/embeddings"}
		raw := []byte(`{"model":"text-embedding-3-small","input":"hello"}`)
		hdrs, body, err := tr.RequestBody(raw)
		require.NoError(t, err)
		require.NotNil(t, body)
		var got struct {
			Model string `json:"model"`
		}
		require.NoError(t, json.Unmarshal(body, &got))
		require.Equal(t, "text-embedding-3-large", got.Model)
		// content-length header present
		found := false
		for _, h := range hdrs {
			if h.Name == contentLengthHeaderName {
				require.Equal(t, strconv.Itoa(len(body)), h.Value)
				found = true
			}
		}
		require.True(t, found, "expected content-length header")
	})

	t.Run("registered under openai:embeddings", func(t *testing.T) {
		tr, err := NewForRoute("openai", "embeddings", ProviderConfig{PathPrefix: "/v1"})
		require.NoError(t, err)
		require.NotNil(t, tr)
	})
}

func TestOpenAIEmbeddingsTranslator_ResponseBody(t *testing.T) {
	tr := &openAIToOpenAITranslatorV1Embeddings{}
	hdrs, body, err := tr.ResponseBody([]byte(`{"object":"list"}`), true)
	require.NoError(t, err)
	require.Nil(t, hdrs)
	require.Nil(t, body)
}
