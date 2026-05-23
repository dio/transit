package mcpprofilerouter

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
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
		ID:            "9b3f7d0a80c4aa6d-67261ca9ea3dadb2",
		Name:          "kiwi",
		APIKey:        "profile-key",
		TimeoutMillis: 500,
		Servers: map[string]Server{
			"github-server": {URL: github.URL, Prefix: "github", Credential: "Bearer github-token", EnabledTools: map[string]bool{"search": true}},
			"kiwi-server":   {URL: kiwi.URL, Prefix: "kiwi", Credential: "Bearer kiwi-token", EnabledTools: map[string]bool{"search_flights": true}},
		},
	}).Handler())
	t.Cleanup(agg.Close)

	sessionID := initialize(t, agg.URL)
	list := postRPC(t, agg.URL, profilePath, sessionID, JSONRPCRequest{
		JSONRPC: "2.0",
		ID:      json.RawMessage(`2`),
		Method:  MethodToolsList,
	})
	must.Nil(list.Error)
	raw, err := json.Marshal(list.Result)
	must.NoError(err)
	must.JSONEq(`{
	  "tools": [
	    {"name":"github.search","description":"Tool search from github","inputSchema":{"additionalProperties":true,"type":"object"}},
	    {"name":"kiwi.search_flights","description":"Tool search_flights from kiwi","inputSchema":{"additionalProperties":true,"type":"object"}}
	  ]
	}`, string(raw))

	call := postRPC(t, agg.URL, profilePath, sessionID, JSONRPCRequest{
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

	disabled := postRPC(t, agg.URL, profilePath, sessionID, JSONRPCRequest{
		JSONRPC: "2.0",
		ID:      json.RawMessage(`"call-2"`),
		Method:  MethodToolsCall,
		Params: mustRaw(CallToolParams{
			Name: "github.repo_read",
		}),
	})
	must.NotNil(disabled.Error)
	must.Contains(disabled.Error.Message, "disabled tool")
	must.Zero(githubBackend(t, github.URL).CallCalls["repo_read"])

	dump := getJSON[AggregatorDump](t, agg.URL+"/dump")
	must.Equal("9b3f7d0a80c4aa6d-67261ca9ea3dadb2", dump.ProfileID)
	must.Equal("kiwi", dump.ProfileName)
	must.True(dump.PublicAuth.Configured)
	must.True(dump.Servers["github-server"].CredentialConfigured)
	must.True(dump.Servers["kiwi-server"].CredentialConfigured)
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
		ID:     "9b3f7d0a80c4aa6d-67261ca9ea3dadb2",
		Name:   "kiwi",
		APIKey: "profile-key",
		Servers: map[string]Server{
			"github": {URL: github.URL, Prefix: "github", Credential: "Bearer github-token"},
		},
	}).Handler())
	t.Cleanup(agg.Close)

	req := JSONRPCRequest{JSONRPC: "2.0", ID: json.RawMessage(`1`), Method: MethodInitialize}
	body, err := json.Marshal(req)
	must.NoError(err)
	resp, err := http.Post(agg.URL+profilePath, "application/json", bytes.NewReader(body)) //nolint:noctx
	must.NoError(err)
	t.Cleanup(func() { _ = resp.Body.Close() })
	must.Equal(http.StatusUnauthorized, resp.StatusCode)

	must.Zero(backend.Dump().ListCalls)
}

func TestAggregator_CatalogEndpointDirectlyProxiesOneServer(t *testing.T) {
	must := require.New(t)

	awsBackend := NewBackend("aws-knowledge", []string{"aws____read_documentation", "aws____search_documentation"}, "")
	aws := httptest.NewServer(awsBackend.Handler())
	t.Cleanup(aws.Close)
	kiwiBackend := NewBackend("kiwi-flight-search", []string{"search-flight"}, "")
	kiwi := httptest.NewServer(kiwiBackend.Handler())
	t.Cleanup(kiwi.Close)

	agg := httptest.NewServer(NewAggregator(Profile{
		ID:   "9b3f7d0a80c4aa6d-67261ca9ea3dadb2",
		Name: "kiwi",
		Servers: map[string]Server{
			"aws-knowledge":      {URL: aws.URL, Prefix: "aws-knowledge"},
			"kiwi-flight-search": {URL: kiwi.URL, Prefix: "kiwi-flight-search"},
		},
	}).Handler())
	t.Cleanup(agg.Close)

	sessionID := initializeAt(t, agg.URL, "/mcp/s/aws-knowledge")
	list := postRPC(t, agg.URL, "/mcp/s/aws-knowledge", sessionID, JSONRPCRequest{
		JSONRPC: "2.0",
		ID:      json.RawMessage(`2`),
		Method:  MethodToolsList,
	})
	must.Nil(list.Error)
	raw, err := json.Marshal(list.Result)
	must.NoError(err)
	must.JSONEq(`{
	  "tools": [
	    {"name":"aws____read_documentation","description":"Tool aws____read_documentation from aws-knowledge","inputSchema":{"additionalProperties":true,"type":"object"}},
	    {"name":"aws____search_documentation","description":"Tool aws____search_documentation from aws-knowledge","inputSchema":{"additionalProperties":true,"type":"object"}}
	  ]
	}`, string(raw))

	call := postRPC(t, agg.URL, "/mcp/s/aws-knowledge", sessionID, JSONRPCRequest{
		JSONRPC: "2.0",
		ID:      json.RawMessage(`"call-1"`),
		Method:  MethodToolsCall,
		Params: mustRaw(CallToolParams{
			Name:      "aws____read_documentation",
			Arguments: map[string]any{"url": "https://docs.aws.amazon.com/"},
		}),
	})
	must.Nil(call.Error)
	must.Equal(1, awsBackend.Dump().CallCalls["aws____read_documentation"])
	must.Zero(kiwiBackend.Dump().ListCalls)
	must.Empty(kiwiBackend.Dump().CallCalls)
}

func TestAggregator_ToolsListReturnsEmptyWhenAllToolsDisabled(t *testing.T) {
	must := require.New(t)

	backend := NewBackend("github", []string{"search", "repo_read"}, "")
	github := httptest.NewServer(backend.Handler())
	t.Cleanup(github.Close)

	agg := httptest.NewServer(NewAggregator(Profile{
		ID:   "9b3f7d0a80c4aa6d-67261ca9ea3dadb2",
		Name: "kiwi",
		Servers: map[string]Server{
			"github": {URL: github.URL, Prefix: "github", EnabledTools: map[string]bool{}},
		},
	}).Handler())
	t.Cleanup(agg.Close)

	sessionID := initialize(t, agg.URL)
	list := postRPC(t, agg.URL, profilePath, sessionID, JSONRPCRequest{
		JSONRPC: "2.0",
		ID:      json.RawMessage(`2`),
		Method:  MethodToolsList,
	})
	must.Nil(list.Error)
	raw, err := json.Marshal(list.Result)
	must.NoError(err)
	must.JSONEq(`{"tools":[]}`, string(raw))
	must.Equal(1, backend.Dump().ListCalls)
}

func TestAggregator_RouteHeaderFallsBackToServerID(t *testing.T) {
	must := require.New(t)

	var mu sync.Mutex
	var routeHeaders []string
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		routeHeaders = append(routeHeaders, r.Header.Get("x-mcp-server"))
		mu.Unlock()
		defer func() { _ = r.Body.Close() }()
		var req JSONRPCRequest
		must.NoError(json.NewDecoder(r.Body).Decode(&req))
		switch req.Method {
		case MethodInitialize:
			w.Header().Set(SessionIDHeader, "backend-session")
			writeJSON(w, http.StatusOK, response(req.ID, InitializeResult{
				ProtocolVersion: ProtocolVersion,
				Capabilities:    map[string]any{"tools": map[string]any{}},
				ServerInfo:      Implementation{Name: "backend"},
			}))
		case MethodToolsList:
			writeJSON(w, http.StatusOK, response(req.ID, ListToolsResult{Tools: []Tool{}}))
		default:
			writeJSON(w, http.StatusOK, errorResponse(req.ID, -32601, "unsupported method"))
		}
	}))
	t.Cleanup(backend.Close)

	agg := httptest.NewServer(NewAggregator(Profile{
		ID:   "9b3f7d0a80c4aa6d-67261ca9ea3dadb2",
		Name: "kiwi",
		Servers: map[string]Server{
			"aws-knowledge": {URL: backend.URL},
		},
	}).Handler())
	t.Cleanup(agg.Close)

	sessionID := initialize(t, agg.URL)
	list := postRPC(t, agg.URL, profilePath, sessionID, JSONRPCRequest{
		JSONRPC: "2.0",
		ID:      json.RawMessage(`2`),
		Method:  MethodToolsList,
	})
	must.Nil(list.Error)

	mu.Lock()
	defer mu.Unlock()
	must.NotEmpty(routeHeaders)
	for _, got := range routeHeaders {
		must.Equal("aws-knowledge", got)
	}
}

func TestValidateProfileRejectsDuplicatePrefixes(t *testing.T) {
	err := ValidateProfile(Profile{
		ID: "profile",
		Servers: map[string]Server{
			"aws-a": {URL: "http://127.0.0.1:8081", Prefix: "aws"},
			"aws-b": {URL: "http://127.0.0.1:8082", Prefix: "aws"},
		},
	})
	require.Error(t, err)
	require.Contains(t, err.Error(), "duplicate server prefix")
}

func TestSessionRoundTrip(t *testing.T) {
	must := require.New(t)
	raw := encodeCompositeSession("route", "9b3f7d0a80c4aa6d-67261ca9ea3dadb2", "user@example.com", map[string]string{
		"github": "sid-a",
		"kiwi":   "sid-b",
	})
	decoded, err := decodeCompositeSession(raw)
	must.NoError(err)
	must.Equal("route", decoded.Route)
	must.Equal("9b3f7d0a80c4aa6d-67261ca9ea3dadb2", decoded.Profile)
	must.Equal("user@example.com", decoded.Subject)
	must.Equal("sid-a", decoded.Backends["github"])
	must.Equal("sid-b", decoded.Backends["kiwi"])
}

const profilePath = "/mcp/9b3f7d0a80c4aa6d-67261ca9ea3dadb2"

func initialize(t *testing.T, baseURL string) string {
	t.Helper()
	return initializeAt(t, baseURL, profilePath)
}

func initializeAt(t *testing.T, baseURL, path string) string {
	t.Helper()
	resp := postRPCRaw(t, baseURL, path, "", JSONRPCRequest{
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

func postRPC(t *testing.T, baseURL, path, sessionID string, req JSONRPCRequest) JSONRPCResponse {
	t.Helper()
	resp := postRPCRaw(t, baseURL, path, sessionID, req)
	defer resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)
	var out JSONRPCResponse
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&out))
	return out
}

func postRPCRaw(t *testing.T, baseURL, path, sessionID string, req JSONRPCRequest) *http.Response {
	t.Helper()
	body, err := json.Marshal(req)
	require.NoError(t, err)
	httpReq, err := http.NewRequest(http.MethodPost, baseURL+path, bytes.NewReader(body)) //nolint:noctx
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

func githubBackend(t *testing.T, serverURL string) BackendDump {
	t.Helper()
	return getJSON[BackendDump](t, serverURL+"/dump")
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
