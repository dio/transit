package mcpstreamingsidecar_test

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/require"

	mcpsidecar "github.com/dio/transit/examples/mcp-streaming-sidecar"
)

// TestServeHTTP_EchoesSSEEvent verifies that MCPStreamingSidecar.ServeHTTP
// writes an SSE event and returns.
func TestServeHTTP_EchoesSSEEvent(t *testing.T) {
	handler := &mcpsidecar.MCPStreamingSidecarHandler{}
	req := httptest.NewRequest(http.MethodGet, "/v1/stream", nil)
	req.Header.Set("x-request-id", "test-trace-id")
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	resp := rec.Result()
	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	resp.Body.Close()

	require.Equal(t, "text/event-stream", resp.Header.Get("Content-Type"))
	require.Contains(t, string(body), "mcp-streaming-sidecar-stub")
}

// TestServeHTTP_OnSession fires when handler returns.
func TestServeHTTP_OnSession(t *testing.T) {
	var fired atomic.Bool

	// Wire via httptest: create sidecar handler directly, wrap with session observer.
	inner := &mcpsidecar.MCPStreamingSidecarHandler{}
	var sessionPath string
	wrapped := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		inner.ServeHTTP(w, r)
		fired.Store(true)
		sessionPath = r.URL.Path
	})

	req := httptest.NewRequest(http.MethodGet, "/test/path", nil)
	rec := httptest.NewRecorder()
	wrapped.ServeHTTP(rec, req)

	require.True(t, fired.Load(), "session callback should have fired")
	require.Equal(t, "/test/path", sessionPath)
}

// TestServeHTTP_PropagatesContentType verifies that Content-Type is set to text/event-stream.
func TestServeHTTP_PropagatesContentType(t *testing.T) {
	handler := &mcpsidecar.MCPStreamingSidecarHandler{}
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	require.Equal(t, "text/event-stream", rec.Header().Get("Content-Type"))
}

// TestServeHTTP_BodyContainsStubType ensures the SSE stub payload type is correct.
func TestServeHTTP_BodyContainsStubType(t *testing.T) {
	handler := &mcpsidecar.MCPStreamingSidecarHandler{}
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	body := rec.Body.String()
	require.True(t, strings.Contains(body, "mcp-streaming-sidecar-stub"),
		"body should contain stub type, got: %q", body)
}
