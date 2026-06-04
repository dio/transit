package send_test

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/dio/transit/examples/orange/internal/send"
	"github.com/dio/transit/up"
	"github.com/dio/transit/up/testutil"
)

func newWriter() (*up.Writer, *testutil.FakeFilterHandle) {
	h := testutil.NewFilterHandle()
	return up.NewWriter(h), h
}

// errorResp mirrors the OpenAI error envelope for test assertions.
type errorResp struct {
	Error struct {
		Message string  `json:"message"`
		Type    string  `json:"type"`
		Param   *string `json:"param"`
		Code    string  `json:"code"`
	} `json:"error"`
}

func TestError_statusAndJSONBody(t *testing.T) {
	w, h := newWriter()
	send.Error(w, http.StatusBadRequest, send.InvalidRequestError, "orange.model_required", "missing model field")

	require.Len(t, h.LocalResponses, 1)
	resp := h.LocalResponses[0]
	require.Equal(t, uint32(http.StatusBadRequest), resp.Status)

	var body errorResp
	require.NoError(t, json.Unmarshal(resp.Body, &body))
	require.Equal(t, "missing model field", body.Error.Message)
	require.Equal(t, "invalid_request_error", body.Error.Type)
	require.Equal(t, "orange.model_required", body.Error.Code)
	require.Nil(t, body.Error.Param)
}

func TestError_contentTypePrepended(t *testing.T) {
	w, h := newWriter()
	send.Error(w, http.StatusNotFound, send.InvalidRequestError, "orange.model_not_found", "unknown model")

	resp := h.LocalResponses[0]
	require.Equal(t, [2]string{"content-type", "application/json"}, resp.Headers[0])
}

func TestError_extraHeadersAppendedAfterContentType(t *testing.T) {
	w, h := newWriter()
	send.Error(w, http.StatusTooManyRequests, send.RateLimitError, "rate_limit_exceeded", "slow down",
		[2]string{"retry-after", "60"},
		[2]string{"x-request-id", "abc123"},
	)

	resp := h.LocalResponses[0]
	require.Len(t, resp.Headers, 3)
	require.Equal(t, [2]string{"content-type", "application/json"}, resp.Headers[0])
	require.Equal(t, [2]string{"retry-after", "60"}, resp.Headers[1])
	require.Equal(t, [2]string{"x-request-id", "abc123"}, resp.Headers[2])
}

func TestErrorf_formatsMessage(t *testing.T) {
	w, h := newWriter()
	send.Errorf(w, http.StatusNotFound, send.InvalidRequestError, "orange.model_not_found",
		"no upstream configured for model %s", "gpt-4o")

	require.Len(t, h.LocalResponses, 1)
	resp := h.LocalResponses[0]
	require.Equal(t, uint32(http.StatusNotFound), resp.Status)

	var body errorResp
	require.NoError(t, json.Unmarshal(resp.Body, &body))
	require.Equal(t, "no upstream configured for model gpt-4o", body.Error.Message)
	require.Equal(t, "invalid_request_error", body.Error.Type)
	require.Equal(t, "orange.model_not_found", body.Error.Code)
	require.Equal(t, [2]string{"content-type", "application/json"}, resp.Headers[0])
}

func TestJSON_statusBodyAndContentType(t *testing.T) {
	w, h := newWriter()

	err := send.JSON(w, http.StatusOK, map[string]any{
		"object": "list",
		"data":   []string{"gpt-4o-mini"},
	})
	require.NoError(t, err)

	require.Len(t, h.LocalResponses, 1)
	resp := h.LocalResponses[0]
	require.Equal(t, uint32(http.StatusOK), resp.Status)
	require.Equal(t, [2]string{"content-type", "application/json"}, resp.Headers[0])

	var got struct {
		Object string   `json:"object"`
		Data   []string `json:"data"`
	}
	require.NoError(t, json.Unmarshal(resp.Body, &got))
	require.Equal(t, "list", got.Object)
	require.Equal(t, []string{"gpt-4o-mini"}, got.Data)
}

func TestJSON_extraHeadersAppendedAfterContentType(t *testing.T) {
	w, h := newWriter()

	err := send.JSON(w, http.StatusOK, map[string]string{"ok": "true"},
		[2]string{"cache-control", "no-store"},
		[2]string{"x-request-id", "abc123"},
	)
	require.NoError(t, err)

	resp := h.LocalResponses[0]
	require.Len(t, resp.Headers, 3)
	require.Equal(t, [2]string{"content-type", "application/json"}, resp.Headers[0])
	require.Equal(t, [2]string{"cache-control", "no-store"}, resp.Headers[1])
	require.Equal(t, [2]string{"x-request-id", "abc123"}, resp.Headers[2])
}

func TestJSON_marshalErrorDoesNotSendPartialResponse(t *testing.T) {
	w, h := newWriter()

	err := send.JSON(w, http.StatusOK, map[string]any{"bad": func() {}})
	require.Error(t, err)
	require.Empty(t, h.LocalResponses)
}
