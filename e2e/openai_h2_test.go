package e2e

import (
	"bytes"
	"io"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

const transitOpenAIH2InvalidKey = "sk-transit-local-h2-invalid"

func TestOpenAIClusterProvidedH2MinimalUpstreamFilter(t *testing.T) {
	if os.Getenv("RUN_TRANSIT_OPENAI_H2_E2E") != "1" {
		t.Skip("set RUN_TRANSIT_OPENAI_H2_E2E=1 to run the OpenAI H2 provider probe")
	}

	resp := sendTransitOpenAIChat(t, openAIClusterProvidedAddr)
	body := compactTransitOpenAIBody(resp.body)
	require.Equal(t, http.StatusUnauthorized, resp.status, body)
	require.Contains(t, resp.body, "invalid_api_key", body)
	require.NotContains(t, strings.ToLower(resp.body), "<html>", body)
	require.NotContains(t, strings.ToLower(resp.contentType), "text/html", body)
	require.NotContains(t, strings.ToLower(resp.server), "cloudflare", body)
}

type transitOpenAIResponse struct {
	status      int
	contentType string
	server      string
	body        string
}

func sendTransitOpenAIChat(t *testing.T, proxyURL string) transitOpenAIResponse {
	t.Helper()
	client := &http.Client{Timeout: 30 * time.Second}
	body := []byte(`{"model":"gpt-4o-mini","messages":[{"role":"user","content":"say pong"}],"max_tokens":1}`)
	req, err := http.NewRequest(http.MethodPost, proxyURL+"/v1/chat/completions", bytes.NewReader(body))
	require.NoError(t, err)
	req.Header.Set("content-type", "application/json")
	req.Header.Set("authorization", "Bearer "+transitOpenAIH2InvalidKey)
	resp, err := client.Do(req)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	data, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	return transitOpenAIResponse{
		status:      resp.StatusCode,
		contentType: resp.Header.Get("content-type"),
		server:      resp.Header.Get("server"),
		body:        string(data),
	}
}

func compactTransitOpenAIBody(body string) string {
	body = strings.Join(strings.Fields(body), " ")
	if len(body) > 500 {
		return body[:500] + "..."
	}
	return body
}
