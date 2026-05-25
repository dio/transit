package mcpcatalogrouter

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/require"

	mcpprofilerouter "github.com/dio/transit/examples/mcp-profile-router"
)

func TestCatalogRouter_forwardsServerSlugAsRouteHeader(t *testing.T) {
	var gotPath, gotRoute, gotAuth string
	backend := mcpprofilerouter.NewBackend("aws-knowledge", []string{"aws____read_documentation"}, "Bearer aws-token")
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotRoute = r.Header.Get("x-mcp-server")
		gotAuth = r.Header.Get("authorization")
		backend.Handler().ServeHTTP(w, r)
	}))
	defer upstream.Close()

	router := New(Config{
		Servers: map[string]Server{
			"aws-knowledge": {URL: upstream.URL, Credential: "Bearer aws-token"},
		},
	})
	server := httptest.NewServer(router.Handler())
	defer server.Close()

	sessionID := initialize(t, server.URL+"/mcp/s/aws-knowledge")
	list := postRPC(t, server.URL+"/mcp/s/aws-knowledge", sessionID, mcpprofilerouter.JSONRPCRequest{
		JSONRPC: "2.0",
		ID:      json.RawMessage(`2`),
		Method:  mcpprofilerouter.MethodToolsList,
	})

	require.Nil(t, list.Error)
	require.Equal(t, "/mcp", gotPath)
	require.Equal(t, "aws-knowledge", gotRoute)
	require.Equal(t, "Bearer aws-token", gotAuth)
	raw, err := json.Marshal(list.Result)
	require.NoError(t, err)
	var out mcpprofilerouter.ListToolsResult
	require.NoError(t, json.Unmarshal(raw, &out))
	require.Len(t, out.Tools, 1)
	require.Equal(t, "aws____read_documentation", out.Tools[0].Name)
}

func TestCatalogRouter_usesCustomRouteHeader(t *testing.T) {
	var gotRoute string
	backend := mcpprofilerouter.NewBackend("github", []string{"search"}, "")
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotRoute = r.Header.Get("x-custom-server")
		backend.Handler().ServeHTTP(w, r)
	}))
	defer upstream.Close()

	router := New(Config{
		RouteHeader: "x-custom-server",
		Servers: map[string]Server{
			"github": {URL: upstream.URL},
		},
	})
	server := httptest.NewServer(router.Handler())
	defer server.Close()

	_ = initialize(t, server.URL+"/mcp/s/github")
	require.Equal(t, "github", gotRoute)
}

func TestCatalogRouter_unknownServer(t *testing.T) {
	router := New(Config{
		Servers: map[string]Server{
			"github": {URL: "http://127.0.0.1:1"},
		},
	})
	server := httptest.NewServer(router.Handler())
	defer server.Close()

	resp, err := http.Post(server.URL+"/mcp/s/aws-knowledge", "application/json", bytes.NewReader([]byte(`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`)))
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	require.Equal(t, http.StatusNotFound, resp.StatusCode)
}

func TestValidateConfig(t *testing.T) {
	require.NoError(t, ValidateConfig(Config{
		Servers: map[string]Server{
			"github": {URL: "http://127.0.0.1:8080"},
		},
	}))
	require.Error(t, ValidateConfig(Config{}))
	require.Error(t, ValidateConfig(Config{Servers: map[string]Server{"bad/id": {URL: "http://127.0.0.1:8080"}}}))
	require.Error(t, ValidateConfig(Config{Servers: map[string]Server{"github": {URL: "127.0.0.1:8080"}}}))
}

func initialize(t *testing.T, url string) string {
	t.Helper()
	resp := postRPCRaw(t, url, "", mcpprofilerouter.JSONRPCRequest{
		JSONRPC: "2.0",
		ID:      json.RawMessage(`1`),
		Method:  mcpprofilerouter.MethodInitialize,
		Params: mustRaw(t, mcpprofilerouter.InitializeParams{
			ProtocolVersion: mcpprofilerouter.ProtocolVersion,
			ClientInfo:      mcpprofilerouter.Implementation{Name: "mcp-catalog-router-test"},
		}),
	})
	defer func() { _ = resp.Body.Close() }()
	require.Equal(t, http.StatusOK, resp.StatusCode)
	sessionID := resp.Header.Get(mcpprofilerouter.SessionIDHeader)
	require.NotEmpty(t, sessionID)
	return sessionID
}

func postRPC(t *testing.T, url, sessionID string, req mcpprofilerouter.JSONRPCRequest) mcpprofilerouter.JSONRPCResponse {
	t.Helper()
	resp := postRPCRaw(t, url, sessionID, req)
	defer func() { _ = resp.Body.Close() }()
	require.Equal(t, http.StatusOK, resp.StatusCode)
	var out mcpprofilerouter.JSONRPCResponse
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&out))
	return out
}

func postRPCRaw(t *testing.T, url, sessionID string, req mcpprofilerouter.JSONRPCRequest) *http.Response {
	t.Helper()
	body, err := json.Marshal(req)
	require.NoError(t, err)
	httpReq, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(body)) //nolint:noctx
	require.NoError(t, err)
	httpReq.Header.Set("content-type", "application/json")
	httpReq.Header.Set("accept", "application/json")
	if sessionID != "" {
		httpReq.Header.Set(mcpprofilerouter.SessionIDHeader, sessionID)
	}
	resp, err := http.DefaultClient.Do(httpReq)
	require.NoError(t, err)
	return resp
}

func mustRaw(t *testing.T, v any) json.RawMessage {
	t.Helper()
	raw, err := json.Marshal(v)
	require.NoError(t, err)
	return raw
}

func TestCatalogRouter_healthz(t *testing.T) {
	router := New(Config{
		Servers: map[string]Server{
			"s1": {URL: "http://127.0.0.1:1"},
		},
	})
	server := httptest.NewServer(router.Handler())
	defer server.Close()

	resp, err := http.Get(server.URL + "/healthz") //nolint:noctx
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	require.Equal(t, http.StatusNoContent, resp.StatusCode)
	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	require.Empty(t, body)
}

func TestCatalogRouter_dump_recordsState(t *testing.T) {
	backend := mcpprofilerouter.NewBackend("s1", []string{"tool1"}, "")
	upstream := httptest.NewServer(backend.Handler())
	defer upstream.Close()

	router := New(Config{
		Servers: map[string]Server{
			"s1": {URL: upstream.URL},
		},
	})
	server := httptest.NewServer(router.Handler())
	defer server.Close()

	_ = initialize(t, server.URL+"/mcp/s/s1")

	resp, err := http.Get(server.URL + "/dump") //nolint:noctx
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	require.Equal(t, http.StatusOK, resp.StatusCode)

	var dump map[string]ServerDump
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&dump))
	entry, ok := dump["s1"]
	require.True(t, ok, "expected key s1 in dump")
	require.Equal(t, upstream.URL, entry.Target)
	require.False(t, entry.CredentialConfigured)
}

func TestCatalogRouter_backendUnreachable(t *testing.T) {
	router := New(Config{
		Servers: map[string]Server{
			"dead": {URL: "http://127.0.0.1:1"},
		},
	})
	server := httptest.NewServer(router.Handler())
	defer server.Close()

	resp := postRPCRaw(t, server.URL+"/mcp/s/dead", "", mcpprofilerouter.JSONRPCRequest{
		JSONRPC: "2.0",
		ID:      json.RawMessage(`1`),
		Method:  mcpprofilerouter.MethodInitialize,
	})
	defer func() { _ = resp.Body.Close() }()
	require.Equal(t, http.StatusBadGateway, resp.StatusCode)

	var out mcpprofilerouter.JSONRPCResponse
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&out))
	require.NotNil(t, out.Error)
	require.Equal(t, -32003, out.Error.Code)
}

func TestCatalogRouter_responseHeadersForwarded(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("X-Custom", "yes")
		w.Header().Set("Content-Length", "3")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("abc"))
	}))
	defer upstream.Close()

	router := New(Config{
		Servers: map[string]Server{
			"svc": {URL: upstream.URL},
		},
	})

	// Use recorder directly so net/http transport cannot re-inject Content-Length.
	body, err := json.Marshal(mcpprofilerouter.JSONRPCRequest{
		JSONRPC: "2.0",
		ID:      json.RawMessage(`1`),
		Method:  mcpprofilerouter.MethodInitialize,
	})
	require.NoError(t, err)
	req := httptest.NewRequest(http.MethodPost, "/mcp/s/svc", bytes.NewReader(body))
	req.Header.Set("content-type", "application/json")
	req.Header.Set("accept", "application/json")
	rec := httptest.NewRecorder()
	router.Handler().ServeHTTP(rec, req)

	require.Equal(t, "yes", rec.Header().Get("X-Custom"))
	require.Empty(t, rec.Header().Get("Content-Length"))
}

func TestLoadConfigFromEnv_missing(t *testing.T) {
	t.Setenv(ConfigEnv, "")
	_, err := LoadConfigFromEnv()
	require.Error(t, err)
	require.Contains(t, err.Error(), ConfigEnv)
}

func TestLoadConfigFromEnv_invalidJSON(t *testing.T) {
	t.Setenv(ConfigEnv, "{bad json")
	_, err := LoadConfigFromEnv()
	require.Error(t, err)
}

func TestLoadConfigFromEnv_valid(t *testing.T) {
	t.Setenv(ConfigEnv, `{"servers":{"s1":{"url":"http://127.0.0.1:8080"}}}`)
	cfg, err := LoadConfigFromEnv()
	require.NoError(t, err)
	require.Equal(t, "http://127.0.0.1:8080", cfg.Servers["s1"].URL)
}

func TestSortedServerIDs(t *testing.T) {
	ids := SortedServerIDs(map[string]Server{
		"c": {URL: "http://127.0.0.1:1"},
		"a": {URL: "http://127.0.0.1:2"},
		"b": {URL: "http://127.0.0.1:3"},
	})
	require.Equal(t, []string{"a", "b", "c"}, ids)
}
