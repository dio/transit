package mcpcatalogrouter

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	mcpprofilerouter "github.com/dio/transit/examples/mcp-profile-router"
	"github.com/stretchr/testify/require"
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
