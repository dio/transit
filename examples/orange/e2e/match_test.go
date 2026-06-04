package e2e

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"
)

// errorResponse mirrors the OpenAI-style envelope sent by the send package.
type errorResponse struct {
	Error struct {
		Message string `json:"message"`
		Type    string `json:"type"`
		Code    string `json:"code"`
	} `json:"error"`
}

func decodeErrorResponse(t *testing.T, r io.Reader) errorResponse {
	t.Helper()
	var got errorResponse
	require.NoError(t, json.NewDecoder(r).Decode(&got))
	return got
}

func proxyRequest(t *testing.T, method, path, body string) *http.Response {
	t.Helper()
	var bodyR io.Reader
	if body != "" {
		bodyR = bytes.NewBufferString(body)
	}
	req, err := http.NewRequest(method, proxyURL+path, bodyR)
	require.NoError(t, err)
	req.Header.Set("content-type", "application/json")
	resp, err := testClient.Do(req)
	require.NoError(t, err)
	return resp
}

// TestMatch_missingModel verifies that a POST body without a "model" field
// returns 400 with the orange.model_required code.
func TestMatch_missingModel(t *testing.T) {
	resp := proxyRequest(t, http.MethodPost, "/v1/chat/completions", `{"messages":[]}`)
	defer resp.Body.Close()
	require.Equal(t, http.StatusBadRequest, resp.StatusCode)

	got := decodeErrorResponse(t, resp.Body)
	require.Equal(t, "orange.model_required", got.Error.Code)
}

// TestMatch_unknownModelBody verifies the 404 error body for an unknown model,
// in addition to the status check already in TestChatCompletion_unknownModel.
func TestMatch_unknownModelBody(t *testing.T) {
	resp := proxyRequest(t, http.MethodPost, "/v1/chat/completions", `{"model":"no-such-model","messages":[]}`)
	defer resp.Body.Close()
	require.Equal(t, http.StatusNotFound, resp.StatusCode)

	got := decodeErrorResponse(t, resp.Body)
	require.Equal(t, "orange.model_not_found", got.Error.Code)
}

// TestMatch_notFoundPath verifies that a POST to an unregistered path returns
// 404 with the orange.not_found code.
func TestMatch_notFoundPath(t *testing.T) {
	resp := proxyRequest(t, http.MethodPost, "/v1/unknown", `{}`)
	defer resp.Body.Close()
	require.Equal(t, http.StatusNotFound, resp.StatusCode)

	got := decodeErrorResponse(t, resp.Body)
	require.Equal(t, "orange.not_found", got.Error.Code)
}

func TestMatch_getV1Models(t *testing.T) {
	resp := proxyRequest(t, http.MethodGet, "/v1/models", "")
	defer resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)
	require.Contains(t, resp.Header.Get("content-type"), "application/json")

	var got struct {
		Object string `json:"object"`
		Data   []struct {
			ID       string         `json:"id"`
			Object   string         `json:"object"`
			OwnedBy  string         `json:"owned_by"`
			Metadata map[string]any `json:"metadata,omitempty"`
		} `json:"data"`
	}
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&got))
	require.Equal(t, "list", got.Object)
	require.Len(t, got.Data, 1)
	require.Equal(t, "gpt-4o-mini", got.Data[0].ID)
	require.Equal(t, "model", got.Data[0].Object)
	require.Equal(t, "github_models", got.Data[0].OwnedBy)
	require.Equal(t, map[string]any{"tier": "fast"}, got.Data[0].Metadata)
}

func TestMatch_postV1ModelsNotFound(t *testing.T) {
	resp := proxyRequest(t, http.MethodPost, "/v1/models", `{}`)
	defer resp.Body.Close()
	require.Equal(t, http.StatusNotFound, resp.StatusCode)

	got := decodeErrorResponse(t, resp.Body)
	require.Equal(t, "orange.not_found", got.Error.Code)
}

// TestMatch_wrongMethod verifies that a GET on a known path returns 404
// (the router only registers POST).
func TestMatch_wrongMethod(t *testing.T) {
	resp := proxyRequest(t, http.MethodGet, "/v1/chat/completions", "")
	defer resp.Body.Close()
	require.Equal(t, http.StatusNotFound, resp.StatusCode)

	got := decodeErrorResponse(t, resp.Body)
	require.Equal(t, "orange.not_found", got.Error.Code)
}

// TestMatch_missingModelMessages verifies the same 400/model_required
// behaviour on the /v1/messages path.
func TestMatch_missingModelMessages(t *testing.T) {
	resp := proxyRequest(t, http.MethodPost, "/v1/messages", `{"messages":[]}`)
	defer resp.Body.Close()
	require.Equal(t, http.StatusBadRequest, resp.StatusCode)

	got := decodeErrorResponse(t, resp.Body)
	require.Equal(t, "orange.model_required", got.Error.Code)
}
