package mcpprofilegateway

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	mcpprofilerouter "github.com/dio/transit/examples/mcp-profile-router"
	"github.com/stretchr/testify/require"
)

func TestGateway_forwardsPublicCatalogServerToOwningL2(t *testing.T) {
	var gotPath, gotCredRef, gotCredEnvelope string
	l2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotCredRef = r.Header.Get("x-mcp-credential-ref")
		gotCredEnvelope = r.Header.Get("x-mcp-credential-envelope")
		w.Header().Set(mcpprofilerouter.SessionIDHeader, "l2-session")
		writeJSON(w, http.StatusOK, mcpprofilerouter.JSONRPCResponse{
			JSONRPC: "2.0",
			ID:      json.RawMessage(`1`),
			Result: mcpprofilerouter.InitializeResult{
				ProtocolVersion: mcpprofilerouter.ProtocolVersion,
				Capabilities:    map[string]any{"tools": map[string]any{}},
				ServerInfo:      mcpprofilerouter.Implementation{Name: "l2-aws"},
			},
		})
	}))
	defer l2.Close()

	gateway := New(Config{
		CatalogServers: map[string]CatalogServer{
			"aws-knowledge": {URL: l2.URL},
		},
	})
	server := httptest.NewServer(gateway.Handler())
	defer server.Close()

	resp := postRPCRaw(t, server.URL+"/mcp/s/aws-knowledge", "", mcpprofilerouter.JSONRPCRequest{
		JSONRPC: "2.0",
		ID:      json.RawMessage(`1`),
		Method:  mcpprofilerouter.MethodInitialize,
		Params: mustRaw(t, mcpprofilerouter.InitializeParams{
			ProtocolVersion: mcpprofilerouter.ProtocolVersion,
			ClientInfo:      mcpprofilerouter.Implementation{Name: "gateway-test"},
		}),
	}, map[string]string{
		"x-mcp-credential-ref":      "profile/aws/user",
		"x-mcp-credential-envelope": "opaque",
	})
	defer func() { _ = resp.Body.Close() }()

	require.Equal(t, http.StatusOK, resp.StatusCode)
	require.Equal(t, "/mcp/s/aws-knowledge", gotPath)
	require.Empty(t, gotCredRef)
	require.Empty(t, gotCredEnvelope)
	require.Equal(t, "l2-session", resp.Header.Get(mcpprofilerouter.SessionIDHeader))
}

func TestGateway_unknownCatalogServer(t *testing.T) {
	gateway := New(Config{
		CatalogServers: map[string]CatalogServer{
			"github": {URL: "http://127.0.0.1:1"},
		},
	})
	server := httptest.NewServer(gateway.Handler())
	defer server.Close()

	resp := postRPCRaw(t, server.URL+"/mcp/s/aws-knowledge", "", mcpprofilerouter.JSONRPCRequest{
		JSONRPC: "2.0",
		ID:      json.RawMessage(`1`),
		Method:  mcpprofilerouter.MethodToolsList,
	}, nil)
	defer func() { _ = resp.Body.Close() }()
	require.Equal(t, http.StatusNotFound, resp.StatusCode)
}

func TestValidateConfig(t *testing.T) {
	require.NoError(t, ValidateConfig(Config{
		CatalogServers: map[string]CatalogServer{
			"aws-knowledge": {URL: "http://127.0.0.1:8080"},
			"github":        {URL: "http://127.0.0.1:8081"},
		},
		Profiles: map[string]Profile{
			"profile": {
				Name: "profile",
				Servers: map[string]ProfileServer{
					"aws-knowledge": {URL: "http://127.0.0.1:8080", Prefix: "aws"},
					"github":        {URL: "http://127.0.0.1:8081", Prefix: "github"},
				},
			},
		},
	}))
	require.Error(t, ValidateConfig(Config{}))
	require.Error(t, ValidateConfig(Config{CatalogServers: map[string]CatalogServer{"bad/id": {URL: "http://127.0.0.1:8080"}}}))
	require.Error(t, ValidateConfig(Config{CatalogServers: map[string]CatalogServer{"github": {URL: "127.0.0.1:8080"}}}))
	require.Error(t, ValidateConfig(Config{
		CatalogServers: map[string]CatalogServer{"github": {URL: "http://127.0.0.1:8080"}},
		Profiles: map[string]Profile{
			"profile": {
				Name: "profile",
				Servers: map[string]ProfileServer{
					"a": {URL: "http://127.0.0.1:8080", Prefix: "dup"},
					"b": {URL: "http://127.0.0.1:8081", Prefix: "dup"},
				},
			},
		},
	}))
}

func postRPCRaw(t *testing.T, url, sessionID string, req mcpprofilerouter.JSONRPCRequest, headers map[string]string) *http.Response {
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
	for name, value := range headers {
		httpReq.Header.Set(name, value)
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
