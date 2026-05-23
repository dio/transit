package mcpprofilerouter

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"
)

type Aggregator struct {
	profile         Profile
	client          *http.Client
	configErr       error
	serverForPrefix map[string]string

	mu      sync.Mutex
	servers map[string]ServerDump
}

func NewAggregator(profile Profile) *Aggregator {
	timeout := time.Duration(profile.TimeoutMillis) * time.Millisecond
	if timeout <= 0 {
		timeout = 800 * time.Millisecond
	}
	serverForPrefix, err := profileServerPrefixes(profile)
	return &Aggregator{
		profile:         profile,
		client:          &http.Client{Timeout: timeout},
		configErr:       err,
		serverForPrefix: serverForPrefix,
		servers:         make(map[string]ServerDump),
	}
}

func (a *Aggregator) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("GET /dump", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, a.Dump())
	})
	mux.HandleFunc("POST /mcp/s/{server}", a.handleCatalogServer)
	mux.HandleFunc("POST /mcp/{profile}", a.handleProfile)
	return mux
}

func (a *Aggregator) Dump() AggregatorDump {
	a.mu.Lock()
	defer a.mu.Unlock()
	return AggregatorDump{
		ProfileID:   a.profileID(),
		ProfileName: a.profile.Name,
		PublicAuth: PublicAuthDump{
			Type:       publicAuthType(a.profile.APIKey),
			Configured: a.profile.APIKey != "",
		},
		TimeoutMillis: a.timeoutMillis(),
		Servers:       copyServerDumps(a.servers),
	}
}

func (a *Aggregator) handleProfile(w http.ResponseWriter, r *http.Request) {
	defer func() { _ = r.Body.Close() }()
	if a.configErr != nil {
		writeJSON(w, http.StatusInternalServerError, errorResponse(nil, -32012, "invalid profile config: %s", a.configErr))
		return
	}
	if r.PathValue("profile") != a.profileID() {
		writeJSON(w, http.StatusNotFound, errorResponse(nil, -32004, "unknown profile"))
		return
	}
	if !a.authorized(r) {
		writeJSON(w, http.StatusUnauthorized, errorResponse(nil, -32001, "invalid profile credentials"))
		return
	}
	var req JSONRPCRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, errorResponse(nil, -32700, "invalid JSON-RPC request"))
		return
	}
	switch req.Method {
	case MethodInitialize:
		sessionID, result, err := a.initializeBackends(r.Context(), req.ID)
		if err != nil {
			writeJSON(w, http.StatusOK, errorResponse(req.ID, -32011, "initialize failed: %s", err))
			return
		}
		w.Header().Set(SessionIDHeader, sessionID)
		w.Header().Set(ProtocolVersionHeader, ProtocolVersion)
		writeJSON(w, http.StatusOK, response(req.ID, result))
	case MethodToolsList:
		session, ok := a.sessionFromRequest(r)
		if !ok {
			writeJSON(w, http.StatusBadRequest, errorResponse(req.ID, -32010, "missing or invalid MCP session ID"))
			return
		}
		result, ok := a.listTools(r.Context(), req.ID, session)
		if !ok {
			writeJSON(w, http.StatusOK, errorResponse(req.ID, -32002, "no MCP servers returned tools"))
			return
		}
		writeJSON(w, http.StatusOK, response(req.ID, result))
	case MethodToolsCall:
		session, ok := a.sessionFromRequest(r)
		if !ok {
			writeJSON(w, http.StatusBadRequest, errorResponse(req.ID, -32010, "missing or invalid MCP session ID"))
			return
		}
		result, errResp := a.callTool(r.Context(), req.ID, req.Params, session)
		if errResp != nil {
			writeJSON(w, http.StatusOK, *errResp)
			return
		}
		writeJSON(w, http.StatusOK, response(req.ID, result))
	default:
		writeJSON(w, http.StatusOK, errorResponse(req.ID, -32601, "unsupported method: %s", req.Method))
	}
}

func (a *Aggregator) handleCatalogServer(w http.ResponseWriter, r *http.Request) {
	defer func() { _ = r.Body.Close() }()
	if a.configErr != nil {
		writeJSON(w, http.StatusInternalServerError, errorResponse(nil, -32012, "invalid profile config: %s", a.configErr))
		return
	}
	serverID := r.PathValue("server")
	server, ok := a.profile.Servers[serverID]
	if !ok {
		writeJSON(w, http.StatusNotFound, errorResponse(nil, -32004, "unknown catalog server"))
		return
	}

	var req JSONRPCRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, errorResponse(nil, -32700, "invalid JSON-RPC request"))
		return
	}

	switch req.Method {
	case MethodInitialize:
		var out InitializeResult
		sessionID, err := a.callServer(r.Context(), serverID, server, "", req.ID, MethodInitialize, req.Params, &out)
		if err != nil {
			writeJSON(w, http.StatusOK, errorResponse(req.ID, -32011, "initialize failed: %s", err))
			return
		}
		a.recordServer(serverID, server, "initialized")
		w.Header().Set(SessionIDHeader, sessionID)
		w.Header().Set(ProtocolVersionHeader, ProtocolVersion)
		writeJSON(w, http.StatusOK, response(req.ID, out))
	case MethodToolsList:
		sessionID := r.Header.Get(SessionIDHeader)
		if sessionID == "" {
			writeJSON(w, http.StatusBadRequest, errorResponse(req.ID, -32010, "missing MCP session ID"))
			return
		}
		var out ListToolsResult
		if err := a.callServerNoSessionResult(r.Context(), serverID, server, sessionID, req.ID, MethodToolsList, nil, &out); err != nil {
			a.recordServer(serverID, server, "error: "+err.Error())
			writeJSON(w, http.StatusOK, errorResponse(req.ID, -32002, "tools/list failed: %s", serverID))
			return
		}
		a.recordServer(serverID, server, "ok")
		writeJSON(w, http.StatusOK, response(req.ID, out))
	case MethodToolsCall:
		sessionID := r.Header.Get(SessionIDHeader)
		if sessionID == "" {
			writeJSON(w, http.StatusBadRequest, errorResponse(req.ID, -32010, "missing MCP session ID"))
			return
		}
		var out CallToolResult
		if err := a.callServerNoSessionResult(r.Context(), serverID, server, sessionID, req.ID, MethodToolsCall, req.Params, &out); err != nil {
			a.recordServer(serverID, server, "error: "+err.Error())
			writeJSON(w, http.StatusOK, errorResponse(req.ID, -32003, "tool backend failed: %s", serverID))
			return
		}
		a.recordServer(serverID, server, "ok")
		writeJSON(w, http.StatusOK, response(req.ID, out))
	default:
		writeJSON(w, http.StatusOK, errorResponse(req.ID, -32601, "unsupported method: %s", req.Method))
	}
}

func (a *Aggregator) initializeBackends(ctx context.Context, id json.RawMessage) (string, InitializeResult, error) {
	backends := make(map[string]string, len(a.profile.Servers))
	for _, serverID := range sortedServerIDs(a.profile.Servers) {
		server := a.profile.Servers[serverID]
		var out InitializeResult
		sessionID, err := a.callServer(ctx, serverID, server, "", id, MethodInitialize, InitializeParams{
			ProtocolVersion: ProtocolVersion,
			Capabilities:    map[string]any{},
			ClientInfo:      Implementation{Name: "mcp-profile-router-aggregator", Version: "dev"},
		}, &out)
		if err != nil {
			return "", InitializeResult{}, fmt.Errorf("%s: %w", serverID, err)
		}
		backends[serverID] = sessionID
		a.recordServer(serverID, server, "initialized")
	}
	sessionID := encodeCompositeSession("mcp-profile-router", a.profileID(), "", backends)
	return sessionID, InitializeResult{
		ProtocolVersion: ProtocolVersion,
		Capabilities: map[string]any{
			"tools": map[string]any{},
		},
		ServerInfo: Implementation{Name: "mcp-profile-router", Version: "dev"},
	}, nil
}

func (a *Aggregator) listTools(ctx context.Context, id json.RawMessage, session compositeSession) (ListToolsResult, bool) {
	type result struct {
		serverID string
		tools    []Tool
		err      error
	}
	serverIDs := sortedServerIDs(a.profile.Servers)
	results := make(chan result, len(serverIDs))
	var wg sync.WaitGroup
	for _, serverID := range serverIDs {
		server := a.profile.Servers[serverID]
		wg.Add(1)
		go func() {
			defer wg.Done()
			var out ListToolsResult
			err := a.callServerNoSessionResult(ctx, serverID, server, session.Backends[serverID], id, MethodToolsList, nil, &out)
			results <- result{serverID: serverID, tools: out.Tools, err: err}
		}()
	}
	wg.Wait()
	close(results)

	merged := make([]Tool, 0)
	healthyBackend := false
	for res := range results {
		server := a.profile.Servers[res.serverID]
		if res.err != nil {
			a.recordServer(res.serverID, server, "error: "+res.err.Error())
			continue
		}
		healthyBackend = true
		a.recordServer(res.serverID, server, "ok")
		for _, tool := range res.tools {
			if !serverToolEnabled(server, tool.Name) {
				continue
			}
			tool.Name = serverPrefix(res.serverID, server) + "." + tool.Name
			merged = append(merged, tool)
		}
	}
	sort.Slice(merged, func(i, j int) bool {
		return merged[i].Name < merged[j].Name
	})
	return ListToolsResult{Tools: merged}, healthyBackend
}

func (a *Aggregator) callTool(ctx context.Context, id json.RawMessage, paramsRaw json.RawMessage, session compositeSession) (CallToolResult, *JSONRPCResponse) {
	var params CallToolParams
	if err := json.Unmarshal(paramsRaw, &params); err != nil || params.Name == "" {
		resp := errorResponse(id, -32602, "invalid tools/call params")
		return CallToolResult{}, &resp
	}
	prefix, backendTool, ok := strings.Cut(params.Name, ".")
	if !ok || backendTool == "" {
		resp := errorResponse(id, -32602, "tool name must be namespaced")
		return CallToolResult{}, &resp
	}
	serverID, server, ok := a.serverByPrefix(prefix)
	if !ok {
		resp := errorResponse(id, -32602, "unknown tool: %s", params.Name)
		return CallToolResult{}, &resp
	}
	if !serverToolEnabled(server, backendTool) {
		resp := errorResponse(id, -32602, "disabled tool: %s", params.Name)
		return CallToolResult{}, &resp
	}
	params.Name = backendTool
	var out CallToolResult
	if err := a.callServerNoSessionResult(ctx, serverID, server, session.Backends[serverID], id, MethodToolsCall, params, &out); err != nil {
		a.recordServer(serverID, server, "error: "+err.Error())
		resp := errorResponse(id, -32003, "tool backend failed: %s", serverID)
		return CallToolResult{}, &resp
	}
	a.recordServer(serverID, server, "ok")
	return out, nil
}

func (a *Aggregator) callServerNoSessionResult(ctx context.Context, serverID string, server Server, sessionID string, id json.RawMessage, method string, params any, out any) error {
	_, err := a.callServer(ctx, serverID, server, sessionID, id, method, params, out)
	return err
}

func (a *Aggregator) callServer(ctx context.Context, serverID string, server Server, sessionID string, id json.RawMessage, method string, params any, out any) (string, error) {
	body, err := json.Marshal(JSONRPCRequest{
		JSONRPC: "2.0",
		ID:      cloneRaw(id),
		Method:  method,
		Params:  mustRaw(params),
	})
	if err != nil {
		return "", err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(server.URL, "/")+"/mcp", bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("content-type", "application/json")
	req.Header.Set("accept", "application/json")
	req.Header.Set(a.routeHeader(), serverPrefix(serverID, server))
	if server.Credential != "" {
		req.Header.Set("authorization", server.Credential)
	}
	if sessionID != "" {
		req.Header.Set(SessionIDHeader, sessionID)
	}
	req.Header.Set(ProtocolVersionHeader, ProtocolVersion)
	resp, err := a.client.Do(req)
	if err != nil {
		return "", err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("status %d", resp.StatusCode)
	}
	var rpc JSONRPCResponse
	if err := json.NewDecoder(resp.Body).Decode(&rpc); err != nil {
		return "", err
	}
	if rpc.Error != nil {
		return "", errors.New(rpc.Error.Message)
	}
	raw, err := json.Marshal(rpc.Result)
	if err != nil {
		return "", err
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return "", err
	}
	return resp.Header.Get(SessionIDHeader), nil
}

func (a *Aggregator) authorized(r *http.Request) bool {
	if a.profile.APIKey == "" {
		return true
	}
	return r.Header.Get("authorization") == "Bearer "+a.profile.APIKey
}

func (a *Aggregator) recordServer(id string, server Server, state string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.servers[id] = ServerDump{
		Target:               server.URL,
		Prefix:               serverPrefix(id, server),
		CredentialConfigured: server.Credential != "",
		LastToolsList:        state,
	}
}

func (a *Aggregator) sessionFromRequest(r *http.Request) (compositeSession, bool) {
	sessionID := r.Header.Get(SessionIDHeader)
	if sessionID == "" {
		return compositeSession{}, false
	}
	session, err := decodeCompositeSession(sessionID)
	if err != nil || session.Profile != a.profileID() {
		return compositeSession{}, false
	}
	return session, true
}

func (a *Aggregator) timeoutMillis() int {
	if a.profile.TimeoutMillis > 0 {
		return a.profile.TimeoutMillis
	}
	return 800
}

func (a *Aggregator) routeHeader() string {
	if a.profile.RouteHeader != "" {
		return a.profile.RouteHeader
	}
	return "x-mcp-server"
}

type AggregatorDump struct {
	ProfileID     string                `json:"profile_id"`
	ProfileName   string                `json:"profile_name"`
	PublicAuth    PublicAuthDump        `json:"public_auth"`
	TimeoutMillis int                   `json:"timeout_millis"`
	Servers       map[string]ServerDump `json:"servers"`
}

type PublicAuthDump struct {
	Type       string `json:"type"`
	Configured bool   `json:"configured"`
}

type ServerDump struct {
	Target               string `json:"target"`
	Prefix               string `json:"prefix"`
	CredentialConfigured bool   `json:"credential_configured"`
	LastToolsList        string `json:"last_tools_list,omitempty"`
}

func (a *Aggregator) profileID() string {
	if a.profile.ID != "" {
		return a.profile.ID
	}
	return a.profile.Name
}

func (a *Aggregator) serverByPrefix(prefix string) (string, Server, bool) {
	serverID, ok := a.serverForPrefix[prefix]
	if !ok {
		return "", Server{}, false
	}
	server, ok := a.profile.Servers[serverID]
	return serverID, server, ok
}

func serverPrefix(serverID string, server Server) string {
	if server.Prefix != "" {
		return server.Prefix
	}
	return serverID
}

func serverToolEnabled(server Server, name string) bool {
	if server.EnabledTools == nil {
		return true
	}
	return server.EnabledTools[name]
}

func ValidateProfile(profile Profile) error {
	if profile.ID == "" && profile.Name == "" {
		return fmt.Errorf("profile id or name is required")
	}
	if len(profile.Servers) == 0 {
		return fmt.Errorf("at least one server is required")
	}
	_, err := profileServerPrefixes(profile)
	return err
}

func profileServerPrefixes(profile Profile) (map[string]string, error) {
	out := make(map[string]string, len(profile.Servers))
	for _, serverID := range sortedServerIDs(profile.Servers) {
		prefix := serverPrefix(serverID, profile.Servers[serverID])
		if existing, ok := out[prefix]; ok {
			return out, fmt.Errorf("duplicate server prefix %q for %q and %q", prefix, existing, serverID)
		}
		out[prefix] = serverID
	}
	return out, nil
}

func publicAuthType(apiKey string) string {
	if apiKey == "" {
		return "none"
	}
	return "api_key"
}

func sortedServerIDs(servers map[string]Server) []string {
	ids := make([]string, 0, len(servers))
	for id := range servers {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}

func copyServerDumps(in map[string]ServerDump) map[string]ServerDump {
	out := make(map[string]ServerDump, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

func mustRaw(value any) json.RawMessage {
	if value == nil {
		return nil
	}
	raw, err := json.Marshal(value)
	if err != nil {
		panic(err)
	}
	return raw
}
