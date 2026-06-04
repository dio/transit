package mcp

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestFilterConstants(t *testing.T) {
	assert.Equal(t, "orange-mcp-egress-match", EgressFilterName)
}

func TestResolveDefaults(t *testing.T) {
	t.Setenv(envListenAddr, "")
	t.Setenv(envEgressURL, "")

	assert.Equal(t, defaultListenAddr, resolveListenAddr())
	assert.Equal(t, defaultEgressURL, resolveEgressURL())
}

func TestResolveEnvOverrides(t *testing.T) {
	t.Setenv(envListenAddr, "127.0.0.1:19004")
	t.Setenv(envEgressURL, "http://127.0.0.1:19005")

	assert.Equal(t, "127.0.0.1:19004", resolveListenAddr())
	assert.Equal(t, "http://127.0.0.1:19005", resolveEgressURL())
}

func TestResolveSessionKeySpecsDefault(t *testing.T) {
	primary, fallbacks := resolveSessionKeySpecs("")
	assert.Equal(t, defaultSessionKey, primary)
	assert.Empty(t, fallbacks)
}

func TestResolveSessionKeySpecsExplicitRotation(t *testing.T) {
	primary, fallbacks := resolveSessionKeySpecs("new-key, old-key, older-key")
	assert.Equal(t, "new-key", primary)
	assert.Equal(t, []string{"old-key", "older-key"}, fallbacks)
}

func TestResolveSessionKeySpecsGenerated(t *testing.T) {
	primary, fallbacks := resolveSessionKeySpecs(generatedKeySpec)
	assert.NotEmpty(t, primary)
	assert.NotEqual(t, generatedKeySpec, primary)
	assert.Empty(t, fallbacks)
	raw, err := base64.RawURLEncoding.DecodeString(primary)
	require.NoError(t, err)
	assert.Len(t, raw, generatedKeyBytes)
}

func TestSidecarReadyAndStop(t *testing.T) {
	sc := newSidecar(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}), sidecarOptions{listenAddr: "127.0.0.1:0", shutdownTimeout: time.Second, egressURL: defaultEgressURL})

	require.NoError(t, sc.Listen())
	require.NotEmpty(t, sc.ListenAddr())

	done := make(chan error, 1)
	go func() { done <- sc.Serve() }()

	select {
	case <-sc.Ready():
	case <-time.After(2 * time.Second):
		t.Fatal("sidecar Ready() did not close in time")
	}

	sc.Stop()

	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("sidecar did not stop in time")
	}
}

func TestHandlerReady(t *testing.T) {
	sc := newSidecar(newHandler(handlerOptions{egressURL: defaultEgressURL}), sidecarOptions{
		listenAddr:      "127.0.0.1:0",
		shutdownTimeout: time.Second,
		egressURL:       defaultEgressURL,
	})
	require.NoError(t, sc.Listen())
	done := make(chan error, 1)
	go func() { done <- sc.Serve() }()
	select {
	case <-sc.Ready():
	case <-time.After(2 * time.Second):
		t.Fatal("sidecar Ready() did not close in time")
	}
	t.Cleanup(func() {
		sc.Stop()
		<-done
	})

	resp, err := http.Get("http://" + sc.ListenAddr() + "/ready")
	require.NoError(t, err)
	defer resp.Body.Close()
	assert.Equal(t, http.StatusNoContent, resp.StatusCode)
}

func TestHandlerRejectsInvalidJSONRPC(t *testing.T) {
	w := &captureResponseWriter{header: http.Header{}}
	newHandler(handlerOptions{egressURL: defaultEgressURL}).ServeHTTP(w, mustRequest(t, http.MethodPost, "/mcp"))

	assert.Equal(t, http.StatusBadRequest, w.status)
	assert.Equal(t, "application/json", w.header.Get("content-type"))
	var body map[string]any
	require.NoError(t, json.Unmarshal(w.body, &body))
	assert.Contains(t, body["error"].(map[string]any)["code"], "orange.mcp_invalid_jsonrpc")
}

type captureResponseWriter struct {
	header http.Header
	status int
	body   []byte
}

func (w *captureResponseWriter) Header() http.Header { return w.header }

func (w *captureResponseWriter) WriteHeader(status int) { w.status = status }

func (w *captureResponseWriter) Write(b []byte) (int, error) {
	w.body = append(w.body, b...)
	return len(b), nil
}

func mustRequest(t *testing.T, method, path string) *http.Request {
	t.Helper()
	req, err := http.NewRequest(method, path, nil)
	require.NoError(t, err)
	return req
}
