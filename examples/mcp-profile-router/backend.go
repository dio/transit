package mcpprofilerouter

import (
	"encoding/json"
	"net/http"
	"sort"
	"sync"
)

type Backend struct {
	ID           string
	Tools        []string
	ExpectedAuth string

	mu        sync.Mutex
	sessions  map[string]bool
	listCalls int
	callCalls map[string]int
	lastAuth  string
}

func NewBackend(id string, tools []string, expectedAuth string) *Backend {
	clean := append([]string(nil), tools...)
	sort.Strings(clean)
	return &Backend{
		ID:           id,
		Tools:        clean,
		ExpectedAuth: expectedAuth,
		sessions:     make(map[string]bool),
		callCalls:    make(map[string]int),
	}
}

func (b *Backend) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("GET /dump", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, b.Dump())
	})
	mux.HandleFunc("POST /mcp", b.handleMCP)
	return mux
}

func (b *Backend) Dump() BackendDump {
	b.mu.Lock()
	defer b.mu.Unlock()
	return BackendDump{
		ID:             b.ID,
		ListCalls:      b.listCalls,
		CallCalls:      copyIntMap(b.callCalls),
		AuthConfigured: b.ExpectedAuth != "",
		LastAuthOK:     b.ExpectedAuth == "" || b.lastAuth == b.ExpectedAuth,
	}
}

func (b *Backend) handleMCP(w http.ResponseWriter, r *http.Request) {
	defer func() { _ = r.Body.Close() }()
	var req JSONRPCRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, errorResponse(nil, -32700, "invalid JSON-RPC request"))
		return
	}
	switch req.Method {
	case MethodInitialize:
		sessionID := b.createSession()
		w.Header().Set(SessionIDHeader, sessionID)
		w.Header().Set(ProtocolVersionHeader, ProtocolVersion)
		writeJSON(w, http.StatusOK, response(req.ID, InitializeResult{
			ProtocolVersion: ProtocolVersion,
			Capabilities: map[string]any{
				"tools": map[string]any{},
			},
			ServerInfo: Implementation{Name: "mcp-profile-router-" + b.ID, Version: "dev"},
		}))
	case MethodToolsList:
		if !b.validSession(r) {
			writeJSON(w, http.StatusBadRequest, errorResponse(req.ID, -32010, "missing or invalid MCP session ID"))
			return
		}
		b.recordList(r.Header.Get("authorization"))
		writeJSON(w, http.StatusOK, response(req.ID, ListToolsResult{Tools: b.toolList()}))
	case MethodToolsCall:
		if !b.validSession(r) {
			writeJSON(w, http.StatusBadRequest, errorResponse(req.ID, -32010, "missing or invalid MCP session ID"))
			return
		}
		var params CallToolParams
		if err := json.Unmarshal(req.Params, &params); err != nil || params.Name == "" {
			writeJSON(w, http.StatusOK, errorResponse(req.ID, -32602, "invalid tools/call params"))
			return
		}
		b.recordCall(params.Name, r.Header.Get("authorization"))
		writeJSON(w, http.StatusOK, response(req.ID, CallToolResult{
			Content: []ContentBlock{{
				Type: "text",
				Text: b.ID + "." + params.Name,
			}},
			StructuredContent: map[string]any{
				"server":  b.ID,
				"tool":    params.Name,
				"auth_ok": b.ExpectedAuth == "" || r.Header.Get("authorization") == b.ExpectedAuth,
				"args":    params.Arguments,
			},
		}))
	default:
		writeJSON(w, http.StatusOK, errorResponse(req.ID, -32601, "unsupported method: %s", req.Method))
	}
}

func (b *Backend) createSession() string {
	sessionID := newSessionID(b.ID)
	b.mu.Lock()
	defer b.mu.Unlock()
	b.sessions[sessionID] = true
	return sessionID
}

func (b *Backend) validSession(r *http.Request) bool {
	sessionID := r.Header.Get(SessionIDHeader)
	if sessionID == "" {
		return false
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.sessions[sessionID]
}

func (b *Backend) toolList() []Tool {
	out := make([]Tool, 0, len(b.Tools))
	for _, name := range b.Tools {
		out = append(out, Tool{
			Name:        name,
			Description: "Tool " + name + " from " + b.ID,
			InputSchema: map[string]any{
				"type":                 "object",
				"additionalProperties": true,
			},
		})
	}
	return out
}

func (b *Backend) recordList(auth string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.listCalls++
	b.lastAuth = auth
}

func (b *Backend) recordCall(tool, auth string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.callCalls[tool]++
	b.lastAuth = auth
}

type BackendDump struct {
	ID             string         `json:"id"`
	ListCalls      int            `json:"list_calls"`
	CallCalls      map[string]int `json:"call_calls"`
	AuthConfigured bool           `json:"auth_configured"`
	LastAuthOK     bool           `json:"last_auth_ok"`
}

func copyIntMap(in map[string]int) map[string]int {
	out := make(map[string]int, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}
