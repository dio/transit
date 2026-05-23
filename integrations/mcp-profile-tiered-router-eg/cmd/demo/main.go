package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"sort"
	"strings"
	"sync"
	"time"
)

const (
	methodInitialize = "initialize"
	methodToolsList  = "tools/list"
	methodToolsCall  = "tools/call"

	sessionIDHeader       = "mcp-session-id"
	protocolVersionHeader = "mcp-protocol-version"
	protocolVersion       = "2025-06-18"
)

type jsonRPCRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type jsonRPCResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Result  any             `json:"result,omitempty"`
	Error   *jsonRPCError   `json:"error,omitempty"`
}

type jsonRPCError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

type tool struct {
	Name        string         `json:"name"`
	Description string         `json:"description,omitempty"`
	InputSchema map[string]any `json:"inputSchema"`
}

type callToolParams struct {
	Name      string         `json:"name"`
	Arguments map[string]any `json:"arguments,omitempty"`
}

type fakeBackend struct {
	server       string
	tools        []string
	expectedAuth string

	mu        sync.Mutex
	sessions  map[string]bool
	listCalls int
	callCalls map[string]int
	lastAuth  string
}

type catalogServer struct {
	url        string
	credential string
}

type catalogRouter struct {
	shard   string
	servers map[string]catalogServer
	client  *http.Client

	mu       sync.Mutex
	requests map[string]int
}

func main() {
	log.SetFlags(log.LstdFlags | log.Lmicroseconds)
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	switch os.Args[1] {
	case "placeholder":
		runPlaceholder(os.Args[2:])
	case "fake-mcp":
		runFakeMCP(os.Args[2:])
	case "catalog-router":
		runCatalogRouter(os.Args[2:])
	default:
		usage()
		os.Exit(2)
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, "usage: mcp-profile-tiered-router-demo <placeholder|fake-mcp|catalog-router> [flags]")
}

func runPlaceholder(args []string) {
	fs := flag.NewFlagSet("placeholder", flag.ExitOnError)
	addr := fs.String("addr", ":8080", "listen address")
	_ = fs.Parse(args)
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusNotFound, errorResponse(nil, -32004, "placeholder backend reached: %s %s", r.Method, r.URL.Path))
	})
	log.Printf("placeholder listening on %s", *addr)
	log.Fatal(http.ListenAndServe(*addr, mux))
}

func runFakeMCP(args []string) {
	fs := flag.NewFlagSet("fake-mcp", flag.ExitOnError)
	server := fs.String("server", "", "logical MCP server name")
	addr := fs.String("addr", ":8080", "listen address")
	_ = fs.Parse(args)
	if *server == "" {
		log.Fatal("--server is required")
	}
	backend := newFakeBackend(*server)
	log.Printf("fake-mcp %s listening on %s", *server, *addr)
	log.Fatal(http.ListenAndServe(*addr, backend.handler()))
}

func runCatalogRouter(args []string) {
	fs := flag.NewFlagSet("catalog-router", flag.ExitOnError)
	shard := fs.String("shard", "", "catalog shard: a or b")
	addr := fs.String("addr", ":8080", "listen address")
	_ = fs.Parse(args)
	router := newCatalogRouter(*shard)
	log.Printf("catalog-router shard %s listening on %s", *shard, *addr)
	log.Fatal(http.ListenAndServe(*addr, router.handler()))
}

func newFakeBackend(server string) *fakeBackend {
	toolsByServer := map[string][]string{
		"kiwi":          {"search-flight"},
		"aws-knowledge": {"aws____read_documentation"},
		"microsoft":     {"search_docs"},
		"github":        {"search"},
	}
	authByServer := map[string]string{
		"kiwi":          "Bearer kiwi-token",
		"aws-knowledge": "Bearer aws-token",
		"microsoft":     "Bearer microsoft-token",
		"github":        "Bearer github-token",
	}
	tools := append([]string(nil), toolsByServer[server]...)
	sort.Strings(tools)
	return &fakeBackend{
		server:       server,
		tools:        tools,
		expectedAuth: authByServer[server],
		sessions:     map[string]bool{},
		callCalls:    map[string]int{},
	}
}

func (b *fakeBackend) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("GET /dump", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, b.dump())
	})
	mux.HandleFunc("POST /mcp", b.handleMCP)
	mux.HandleFunc("POST /_egress/mcp", b.handleMCP)
	return mux
}

func (b *fakeBackend) handleMCP(w http.ResponseWriter, r *http.Request) {
	defer func() { _ = r.Body.Close() }()
	var req jsonRPCRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, errorResponse(nil, -32700, "invalid JSON-RPC request"))
		return
	}
	switch req.Method {
	case methodInitialize:
		sessionID := b.createSession()
		w.Header().Set(sessionIDHeader, sessionID)
		w.Header().Set(protocolVersionHeader, protocolVersion)
		writeJSON(w, http.StatusOK, response(req.ID, map[string]any{
			"protocolVersion": protocolVersion,
			"capabilities":    map[string]any{"tools": map[string]any{}},
			"serverInfo":      map[string]any{"name": "fake-mcp-" + b.server, "version": "dev"},
		}))
	case methodToolsList:
		b.recordList(r.Header.Get("authorization"))
		writeJSON(w, http.StatusOK, response(req.ID, map[string]any{"tools": b.toolList()}))
	case methodToolsCall:
		var params callToolParams
		if err := json.Unmarshal(req.Params, &params); err != nil || params.Name == "" {
			writeJSON(w, http.StatusOK, errorResponse(req.ID, -32602, "invalid tools/call params"))
			return
		}
		b.recordCall(params.Name, r.Header.Get("authorization"))
		writeJSON(w, http.StatusOK, response(req.ID, map[string]any{
			"content": []map[string]any{{
				"type": "text",
				"text": b.server + "." + params.Name,
			}},
			"structuredContent": map[string]any{
				"server":  b.server,
				"tool":    params.Name,
				"auth_ok": b.expectedAuth == "" || r.Header.Get("authorization") == b.expectedAuth,
				"args":    params.Arguments,
			},
		}))
	default:
		writeJSON(w, http.StatusOK, errorResponse(req.ID, -32601, "unsupported method: %s", req.Method))
	}
}

func (b *fakeBackend) createSession() string {
	sessionID := fmt.Sprintf("session-%s-%d", b.server, time.Now().UnixNano())
	b.mu.Lock()
	defer b.mu.Unlock()
	b.sessions[sessionID] = true
	return sessionID
}

func (b *fakeBackend) toolList() []tool {
	out := make([]tool, 0, len(b.tools))
	for _, name := range b.tools {
		out = append(out, tool{
			Name:        name,
			Description: "Tool " + name + " from " + b.server,
			InputSchema: map[string]any{
				"type":                 "object",
				"additionalProperties": true,
			},
		})
	}
	return out
}

func (b *fakeBackend) recordList(auth string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.listCalls++
	b.lastAuth = auth
}

func (b *fakeBackend) recordCall(tool, auth string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.callCalls[tool]++
	b.lastAuth = auth
}

func (b *fakeBackend) dump() map[string]any {
	b.mu.Lock()
	defer b.mu.Unlock()
	return map[string]any{
		"id":              b.server,
		"list_calls":      b.listCalls,
		"call_calls":      copyIntMap(b.callCalls),
		"auth_configured": b.expectedAuth != "",
		"last_auth_ok":    b.expectedAuth == "" || b.lastAuth == b.expectedAuth,
	}
}

func newCatalogRouter(shard string) *catalogRouter {
	serversByShard := map[string]map[string]catalogServer{
		"a": {
			"kiwi":          {url: "http://mcp-kiwi.transit-dataplane.svc.cluster.local:8080", credential: "Bearer kiwi-token"},
			"aws-knowledge": {url: "http://mcp-aws-knowledge.transit-dataplane.svc.cluster.local:8080", credential: "Bearer aws-token"},
		},
		"b": {
			"microsoft": {url: "http://mcp-microsoft.transit-dataplane.svc.cluster.local:8080", credential: "Bearer microsoft-token"},
			"github":    {url: "http://mcp-github.transit-dataplane.svc.cluster.local:8080", credential: "Bearer github-token"},
		},
	}
	servers := serversByShard[shard]
	if len(servers) == 0 {
		log.Fatalf("--shard must be a or b, got %q", shard)
	}
	return &catalogRouter{
		shard:    shard,
		servers:  servers,
		client:   &http.Client{Timeout: 2 * time.Second},
		requests: map[string]int{},
	}
}

func (c *catalogRouter) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("GET /dump", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, c.dump())
	})
	mux.HandleFunc("POST /mcp/s/{server}", c.handleServer)
	return mux
}

func (c *catalogRouter) handleServer(w http.ResponseWriter, r *http.Request) {
	serverID := r.PathValue("server")
	server, ok := c.servers[serverID]
	if !ok {
		writeJSON(w, http.StatusNotFound, errorResponse(nil, -32004, "unknown catalog server on shard %s: %s", c.shard, serverID))
		return
	}
	c.record(serverID)
	defer func() { _ = r.Body.Close() }()
	body, err := io.ReadAll(r.Body)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, errorResponse(nil, -32700, "read body: %v", err))
		return
	}
	req, err := http.NewRequestWithContext(r.Context(), http.MethodPost, strings.TrimRight(server.url, "/")+"/mcp", bytes.NewReader(body))
	if err != nil {
		writeJSON(w, http.StatusBadGateway, errorResponse(nil, -32003, "catalog request failed: %v", err))
		return
	}
	req.Header.Set("content-type", headerOr(r.Header, "content-type", "application/json"))
	req.Header.Set("accept", headerOr(r.Header, "accept", "application/json"))
	req.Header.Set("x-mcp-server", serverID)
	if server.credential != "" {
		req.Header.Set("authorization", server.credential)
	}
	if sessionID := r.Header.Get(sessionIDHeader); sessionID != "" {
		req.Header.Set(sessionIDHeader, sessionID)
	}
	if protocol := r.Header.Get(protocolVersionHeader); protocol != "" {
		req.Header.Set(protocolVersionHeader, protocol)
	}
	resp, err := c.client.Do(req)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, errorResponse(nil, -32003, "catalog backend failed: %s", serverID))
		return
	}
	defer func() { _ = resp.Body.Close() }()
	for name, values := range resp.Header {
		if strings.EqualFold(name, "content-length") {
			continue
		}
		for _, value := range values {
			w.Header().Add(name, value)
		}
	}
	w.WriteHeader(resp.StatusCode)
	_, _ = io.Copy(w, resp.Body)
}

func (c *catalogRouter) record(serverID string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.requests[serverID]++
}

func (c *catalogRouter) dump() map[string]any {
	c.mu.Lock()
	defer c.mu.Unlock()
	return map[string]any{
		"shard":    c.shard,
		"requests": copyIntMap(c.requests),
	}
}

func response(id json.RawMessage, result any) jsonRPCResponse {
	return jsonRPCResponse{JSONRPC: "2.0", ID: cloneRaw(id), Result: result}
}

func errorResponse(id json.RawMessage, code int, format string, args ...any) jsonRPCResponse {
	return jsonRPCResponse{
		JSONRPC: "2.0",
		ID:      cloneRaw(id),
		Error: &jsonRPCError{
			Code:    code,
			Message: fmt.Sprintf(format, args...),
		},
	}
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("content-type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func headerOr(h http.Header, name, fallback string) string {
	if value := h.Get(name); value != "" {
		return value
	}
	return fallback
}

func cloneRaw(raw json.RawMessage) json.RawMessage {
	if len(raw) == 0 {
		return nil
	}
	return append(json.RawMessage(nil), raw...)
}

func copyIntMap(in map[string]int) map[string]int {
	out := make(map[string]int, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}
