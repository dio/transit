package mcpprofilerouter

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestAggregator_SessionToolsListAndCall(t *testing.T) {
	must := require.New(t)

	github := httptest.NewServer(NewBackend("github", []string{"search", "repo_read"}, "Bearer github-token").Handler())
	t.Cleanup(github.Close)
	kiwi := httptest.NewServer(NewBackend("kiwi", []string{"search_flights"}, "Bearer kiwi-token").Handler())
	t.Cleanup(kiwi.Close)

	agg := httptest.NewServer(NewAggregator(Profile{
		Name:          "engineering",
		APIKey:        "profile-key",
		TimeoutMillis: 500,
		Servers: map[string]Server{
			"github": {URL: github.URL, Prefix: "github", Credential: "Bearer github-token"},
			"kiwi":   {URL: kiwi.URL, Prefix: "kiwi", Credential: "Bearer kiwi-token"},
		},
	}).Handler())
	t.Cleanup(agg.Close)

	sessionID := initialize(t, agg.URL)
	list := postRPC(t, agg.URL, sessionID, JSONRPCRequest{
		JSONRPC: "2.0",
		ID:      json.RawMessage(`2`),
		Method:  MethodToolsList,
	})
	must.Nil(list.Error)
	raw, err := json.Marshal(list.Result)
	must.NoError(err)
	must.JSONEq(`{
	  "tools": [
	    {"name":"github.repo_read","description":"Tool repo_read from github","inputSchema":{"additionalProperties":true,"type":"object"}},
	    {"name":"github.search","description":"Tool search from github","inputSchema":{"additionalProperties":true,"type":"object"}},
	    {"name":"kiwi.search_flights","description":"Tool search_flights from kiwi","inputSchema":{"additionalProperties":true,"type":"object"}}
	  ]
	}`, string(raw))

	call := postRPC(t, agg.URL, sessionID, JSONRPCRequest{
		JSONRPC: "2.0",
		ID:      json.RawMessage(`"call-1"`),
		Method:  MethodToolsCall,
		Params: mustRaw(CallToolParams{
			Name:      "github.search",
			Arguments: map[string]any{"query": "transit"},
		}),
	})
	must.Nil(call.Error)
	raw, err = json.Marshal(call.Result)
	must.NoError(err)
	must.JSONEq(`{
	  "content": [{"type":"text","text":"github.search"}],
	  "structuredContent": {
	    "server": "github",
	    "tool": "search",
	    "auth_ok": true,
	    "args": {"query":"transit"}
	  }
	}`, string(raw))

	dump := getJSON[AggregatorDump](t, agg.URL+"/dump")
	must.Equal("engineering", dump.Profile)
	must.True(dump.PublicAuth.Configured)
	must.True(dump.Servers["github"].CredentialConfigured)
	must.True(dump.Servers["kiwi"].CredentialConfigured)
	raw, err = json.Marshal(dump)
	must.NoError(err)
	must.NotContains(string(raw), "github-token")
	must.NotContains(string(raw), "kiwi-token")
}

func TestAggregator_RejectsMissingProfileAuthBeforeBackendCalls(t *testing.T) {
	must := require.New(t)

	backend := NewBackend("github", []string{"search"}, "Bearer github-token")
	github := httptest.NewServer(backend.Handler())
	t.Cleanup(github.Close)

	agg := httptest.NewServer(NewAggregator(Profile{
		Name:   "engineering",
		APIKey: "profile-key",
		Servers: map[string]Server{
			"github": {URL: github.URL, Prefix: "github", Credential: "Bearer github-token"},
		},
	}).Handler())
	t.Cleanup(agg.Close)

	req := JSONRPCRequest{JSONRPC: "2.0", ID: json.RawMessage(`1`), Method: MethodInitialize}
	body, err := json.Marshal(req)
	must.NoError(err)
	resp, err := http.Post(agg.URL+"/mcp/profiles/engineering", "application/json", bytes.NewReader(body)) //nolint:noctx
	must.NoError(err)
	t.Cleanup(func() { _ = resp.Body.Close() })
	must.Equal(http.StatusUnauthorized, resp.StatusCode)

	must.Zero(backend.Dump().ListCalls)
}

func TestSessionRoundTrip(t *testing.T) {
	must := require.New(t)
	raw := encodeCompositeSession("route", "engineering", "user@example.com", map[string]string{
		"github": "sid-a",
		"kiwi":   "sid-b",
	})
	decoded, err := decodeCompositeSession(raw)
	must.NoError(err)
	must.Equal("route", decoded.Route)
	must.Equal("engineering", decoded.Profile)
	must.Equal("user@example.com", decoded.Subject)
	must.Equal("sid-a", decoded.Backends["github"])
	must.Equal("sid-b", decoded.Backends["kiwi"])
}

func initialize(t *testing.T, baseURL string) string {
	t.Helper()
	resp := postRPCRaw(t, baseURL, "", JSONRPCRequest{
		JSONRPC: "2.0",
		ID:      json.RawMessage(`1`),
		Method:  MethodInitialize,
		Params: mustRaw(InitializeParams{
			ProtocolVersion: ProtocolVersion,
			ClientInfo:      Implementation{Name: "test"},
		}),
	})
	defer resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)
	sessionID := resp.Header.Get(SessionIDHeader)
	require.NotEmpty(t, sessionID)
	return sessionID
}

func postRPC(t *testing.T, baseURL, sessionID string, req JSONRPCRequest) JSONRPCResponse {
	t.Helper()
	resp := postRPCRaw(t, baseURL, sessionID, req)
	defer resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)
	var out JSONRPCResponse
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&out))
	return out
}

func postRPCRaw(t *testing.T, baseURL, sessionID string, req JSONRPCRequest) *http.Response {
	t.Helper()
	body, err := json.Marshal(req)
	require.NoError(t, err)
	httpReq, err := http.NewRequest(http.MethodPost, baseURL+"/mcp/profiles/engineering", bytes.NewReader(body)) //nolint:noctx
	require.NoError(t, err)
	httpReq.Header.Set("content-type", "application/json")
	httpReq.Header.Set("accept", "application/json")
	httpReq.Header.Set("authorization", "Bearer profile-key")
	if sessionID != "" {
		httpReq.Header.Set(SessionIDHeader, sessionID)
	}
	resp, err := http.DefaultClient.Do(httpReq)
	require.NoError(t, err)
	return resp
}

func getJSON[T any](t *testing.T, url string) T {
	t.Helper()
	resp, err := http.Get(url) //nolint:noctx
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)
	var out T
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&out))
	return out
}
