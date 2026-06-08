package mcp

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/dio/transit/examples/orange/internal/config"
)

func TestHandlerInitializeRequiresAllProfileBackends(t *testing.T) {
	crypto := newSessionCrypto("test")
	var records []record
	var gotHeaders []http.Header
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotHeaders = append(gotHeaders, r.Header.Clone())
		switch r.Header.Get(headerBackend) {
		case "aws-knowledge":
			w.Header().Set(sessionIDHeader, "aws-session")
			writeBackendRPC(t, w, `1`, `{"capabilities":{"tools":{"listChanged":true}}}`)
		case "github":
			w.Header().Set(sessionIDHeader, "github-session")
			writeBackendRPC(t, w, `1`, `{"capabilities":{"tools":{"listChanged":false}}}`)
		default:
			t.Fatalf("unexpected backend %q", r.Header.Get(headerBackend))
		}
	}))
	t.Cleanup(backend.Close)

	snap := testMCPSnapshot("aws-knowledge", "github")
	h := newHandler(handlerOptions{
		egressURL: backend.URL,
		config:    func() *config.ConfigSnapshot { return snap },
		crypto:    crypto,
		records:   func(r record) { records = append(records, r) },
	})

	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`)))

	require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())
	publicSession := rr.Header().Get(sessionIDHeader)
	require.NotEmpty(t, publicSession)
	session, err := decodeSecureSessionID(crypto, publicSession, "")
	require.NoError(t, err)
	require.Equal(t, "default", session.Route)
	require.Len(t, session.Backends, 2)
	assert.Equal(t, "aws-knowledge", session.Backends[0].Backend)
	assert.Equal(t, "aws-session", session.Backends[0].SessionID)
	assert.True(t, session.Backends[0].Capabilities.Tools)
	assert.True(t, session.Backends[0].Capabilities.ToolsListChanged)
	assert.Equal(t, "github", session.Backends[1].Backend)
	assert.Equal(t, "github-session", session.Backends[1].SessionID)
	assert.True(t, session.Backends[1].Capabilities.Tools)

	require.Len(t, gotHeaders, 2)
	for _, hdr := range gotHeaders {
		assert.Equal(t, "default", hdr.Get(headerRoute))
		assert.Equal(t, methodInitialize, hdr.Get(headerMethod))
		assert.NotEmpty(t, hdr.Get(headerRequestID))
	}
	require.Len(t, records, 1)
	assert.Equal(t, "success", records[0].Outcome)
	assert.Equal(t, 2, records[0].LegCount)
	assert.Equal(t, 0, records[0].FailedLegs)
}

func TestHandlerInitializeRejectsPartialProfile(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Header.Get(headerBackend) {
		case "aws-knowledge":
			w.Header().Set(sessionIDHeader, "aws-session")
			writeBackendRPC(t, w, `1`, `{"capabilities":{"tools":{"listChanged":true}}}`)
		case "github":
			http.Error(w, "failed", http.StatusBadGateway)
		default:
			t.Fatalf("unexpected backend %q", r.Header.Get(headerBackend))
		}
	}))
	t.Cleanup(backend.Close)

	snap := testMCPSnapshot("aws-knowledge", "github")
	h := newHandler(handlerOptions{
		egressURL: backend.URL,
		config:    func() *config.ConfigSnapshot { return snap },
		crypto:    newSessionCrypto("test"),
	})

	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`)))

	require.Equal(t, http.StatusBadGateway, rr.Code)
	assert.Empty(t, rr.Header().Get(sessionIDHeader))
	assert.Contains(t, rr.Body.String(), "MCP profile initialize failed")
}

func TestHandlerInitializeAllFailed(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "failed", http.StatusBadGateway)
	}))
	t.Cleanup(backend.Close)

	snap := testMCPSnapshot("aws-knowledge", "github")
	h := newHandler(handlerOptions{
		egressURL: backend.URL,
		config:    func() *config.ConfigSnapshot { return snap },
		crypto:    newSessionCrypto("test"),
	})

	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`)))

	require.Equal(t, http.StatusBadGateway, rr.Code)
	assert.Contains(t, rr.Body.String(), "MCP profile initialize failed")
}

func TestHandlerInitializeRouteFromPath(t *testing.T) {
	crypto := newSessionCrypto("test")
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "s/aws-knowledge", r.Header.Get(headerRoute))
		require.Equal(t, "aws-knowledge", r.Header.Get(headerBackend))
		w.Header().Set(sessionIDHeader, "aws-session")
		writeBackendRPC(t, w, `1`, `{"capabilities":{"tools":{"listChanged":true}}}`)
	}))
	t.Cleanup(backend.Close)

	snap := testMCPSnapshot("aws-knowledge")
	h := newHandler(handlerOptions{
		egressURL: backend.URL,
		config:    func() *config.ConfigSnapshot { return snap },
		crypto:    crypto,
	})

	// Use the s/<server> path prefix which routes directly to one server.
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodPost, "/mcp/s/aws-knowledge", strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`)))

	require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())
	publicSession := rr.Header().Get(sessionIDHeader)
	require.NotEmpty(t, publicSession)
	session, err := decodeSecureSessionID(crypto, publicSession, "")
	require.NoError(t, err)
	assert.Equal(t, "s/aws-knowledge", session.Route)
}

func TestHandlerToolsListMerge(t *testing.T) {
	crypto := newSessionCrypto("test")
	publicSession := mustSession(t, crypto, sessionEnvelope{
		Route: "default",
		Backends: []backendSession{
			{Backend: "aws-knowledge", SessionID: "aws-session"},
			{Backend: "github", SessionID: "github-session"},
		},
	})
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.NotEmpty(t, r.Header.Get(sessionIDHeader))
		switch r.Header.Get(headerBackend) {
		case "aws-knowledge":
			writeBackendRPC(t, w, `"list-1"`, `{"tools":[{"name":"read_documentation"}]}`)
		case "github":
			writeBackendRPC(t, w, `"list-1"`, `{"tools":[{"name":"search_repositories"}]}`)
		default:
			t.Fatalf("unexpected backend %q", r.Header.Get(headerBackend))
		}
	}))
	t.Cleanup(backend.Close)

	h := newHandler(handlerOptions{egressURL: backend.URL, crypto: crypto})
	req := httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(`{"jsonrpc":"2.0","id":"list-1","method":"tools/list","params":{}}`))
	req.Header.Set(sessionIDHeader, publicSession)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())
	var resp rpcResponse
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &resp))
	var result struct {
		Tools []struct {
			Name string `json:"name"`
		} `json:"tools"`
	}
	require.NoError(t, json.Unmarshal(resp.Result, &result))
	assert.Equal(t, []string{"aws-knowledge__read_documentation", "github__search_repositories"}, []string{result.Tools[0].Name, result.Tools[1].Name})
}

func TestHandlerToolsCallRoutesToOwningBackend(t *testing.T) {
	crypto := newSessionCrypto("test")
	publicSession := mustSession(t, crypto, sessionEnvelope{
		Route: "default",
		Backends: []backendSession{
			{Backend: "aws-knowledge", SessionID: "aws-session"},
			{Backend: "github", SessionID: "github-session"},
		},
	})
	var callsMu sync.Mutex
	var calls []string
	var gotTool string
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		callsMu.Lock()
		calls = append(calls, r.Header.Get(headerBackend))
		callsMu.Unlock()
		require.Equal(t, "github", r.Header.Get(headerBackend))
		require.Equal(t, "github-session", r.Header.Get(sessionIDHeader))
		var req rpcRequest
		require.NoError(t, json.NewDecoder(r.Body).Decode(&req))
		var params struct {
			Name string `json:"name"`
		}
		require.NoError(t, json.Unmarshal(req.Params, &params))
		gotTool = params.Name
		writeBackendRPC(t, w, `"call-1"`, `{"content":[{"type":"text","text":"ok"}]}`)
	}))
	t.Cleanup(backend.Close)

	h := newHandler(handlerOptions{egressURL: backend.URL, crypto: crypto})
	req := httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(`{"jsonrpc":"2.0","id":"call-1","method":"tools/call","params":{"name":"github__search_repositories","arguments":{"q":"transit"}}}`))
	req.Header.Set(sessionIDHeader, publicSession)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())
	assert.Equal(t, "search_repositories", gotTool)
	assert.Equal(t, []string{"github"}, calls)
}

func TestHandlerGETHeartbeatAndEventIDRewrite(t *testing.T) {
	crypto := newSessionCrypto("test")
	publicSession := mustSession(t, crypto, sessionEnvelope{
		Route:    "default",
		Backends: []backendSession{{Backend: "github", SessionID: "github-session"}},
	})
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, http.MethodGet, r.Method)
		require.Equal(t, "github-session", r.Header.Get(sessionIDHeader))
		w.Header().Set("content-type", "text/event-stream")
		_, _ = w.Write([]byte("event: message\nid: backend-event-1\ndata: {\"jsonrpc\":\"2.0\",\"method\":\"roots/list\",\"id\":\"server-1\"}\n\n"))
	}))
	t.Cleanup(backend.Close)

	h := newHandler(handlerOptions{egressURL: backend.URL, crypto: crypto})
	req := httptest.NewRequest(http.MethodGet, "/mcp", nil)
	req.Header.Set(sessionIDHeader, publicSession)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	require.Equal(t, http.StatusAccepted, rr.Code, rr.Body.String())
	body := rr.Body.String()
	assert.Contains(t, body, `"method":"ping"`)
	assert.NotContains(t, body, "backend-event-1\n")
	eventToken := firstSSEID(t, body)
	event, err := decodeSecureLastEventID(crypto, eventToken)
	require.NoError(t, err)
	assert.Equal(t, eventEnvelope{Backend: "github", EventID: "backend-event-1"}, event)
	assert.Contains(t, body, `"backend":"github"`)
	assert.Contains(t, body, `"id":"server-1"`)
}

func TestHandlerDELETEBestEffort(t *testing.T) {
	crypto := newSessionCrypto("test")
	publicSession := mustSession(t, crypto, sessionEnvelope{
		Route: "default",
		Backends: []backendSession{
			{Backend: "aws-knowledge", SessionID: "aws-session"},
			{Backend: "github", SessionID: "github-session"},
		},
	})
	var deletes []string
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, http.MethodDelete, r.Method)
		deletes = append(deletes, r.Header.Get(headerBackend))
		if r.Header.Get(headerBackend) == "github" {
			http.Error(w, "fail", http.StatusBadGateway)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(backend.Close)

	h := newHandler(handlerOptions{egressURL: backend.URL, crypto: crypto})
	req := httptest.NewRequest(http.MethodDelete, "/mcp", nil)
	req.Header.Set(sessionIDHeader, publicSession)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	require.Equal(t, http.StatusOK, rr.Code)
	assert.ElementsMatch(t, []string{"aws-knowledge", "github"}, deletes)
}

// testMCPSnapshot builds a minimal ConfigSnapshot with all named servers in
// snap.Global.Servers. The route "default" maps to all servers (no Profiles
// map, so lookupMCPRoute falls back to the default-route shortcut).
func testMCPSnapshot(servers ...string) *config.ConfigSnapshot {
	serverMap := make(map[string]*config.ServerRecord, len(servers))
	for _, s := range servers {
		serverMap[s] = &config.ServerRecord{Endpoint: "https://" + s + ".example.test"}
	}
	return &config.ConfigSnapshot{
		Global: &config.GlobalConfig{
			Servers: serverMap,
		},
	}
}


func writeBackendRPC(t *testing.T, w http.ResponseWriter, id, result string) {
	t.Helper()
	w.Header().Set("content-type", "application/json")
	_, err := w.Write([]byte(`{"jsonrpc":"2.0","id":` + id + `,"result":` + result + `}`))
	require.NoError(t, err)
}

func mustSession(t *testing.T, crypto sessionCrypto, e sessionEnvelope) string {
	t.Helper()
	token, err := encodeSecureSessionID(crypto, e)
	require.NoError(t, err)
	return token
}

func firstSSEID(t *testing.T, body string) string {
	t.Helper()
	for _, line := range strings.Split(body, "\n") {
		if strings.HasPrefix(line, "id: ") {
			return strings.TrimPrefix(line, "id: ")
		}
	}
	t.Fatalf("no SSE id line in:\n%s", body)
	return ""
}

func TestRewriteServerRequestID(t *testing.T) {
	msg := rewriteServerRequestID(jsonrpcMessage(`{"jsonrpc":"2.0","method":"roots/list","id":"server-1"}`), "github")
	require.True(t, bytes.Contains(msg, []byte(`"backend":"github"`)))
	require.True(t, bytes.Contains(msg, []byte(`"id":"server-1"`)))
}
