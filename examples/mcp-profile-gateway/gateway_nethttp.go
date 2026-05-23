package mcpprofilegateway

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"sort"
	"strings"

	mcpprofilerouter "github.com/dio/transit/examples/mcp-profile-router"
)

// Handler exposes the pure Go net/http implementation used by unit tests and
// local debugging. Envoy dynamic modules use NewTransitFilter in
// gateway_transit.go so outbound catalog traffic stays on Envoy-managed egress.
func (g *Gateway) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("GET /dump", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, g.Dump())
	})
	mux.HandleFunc("POST /mcp/s/{server}", g.handleCatalogServerNetHTTP)
	mux.HandleFunc("POST /mcp/{profile}", g.handleProfileNetHTTP)
	return mux
}

func (g *Gateway) handleProfileNetHTTP(w http.ResponseWriter, r *http.Request) {
	profileID := r.PathValue("profile")
	profile, ok := g.config.Profiles[profileID]
	if !ok {
		writeJSON(w, http.StatusNotFound, errorResponse(nil, -32004, "unknown profile: %s", profileID))
		return
	}
	if !profileAuthorized(profile.APIKey, r.Header.Get) {
		writeJSON(w, http.StatusUnauthorized, errorResponse(nil, -32001, "unauthorized profile: %s", profileID))
		return
	}
	body, err := io.ReadAll(r.Body)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, errorResponse(nil, -32700, "invalid request body"))
		return
	}
	defer func() { _ = r.Body.Close() }()
	var req mcpprofilerouter.JSONRPCRequest
	if err := json.Unmarshal(body, &req); err != nil {
		writeJSON(w, http.StatusBadRequest, errorResponse(nil, -32700, "invalid JSON-RPC request"))
		return
	}
	switch req.Method {
	case mcpprofilerouter.MethodInitialize:
		g.initializeProfileNetHTTP(w, r, req.ID, body, profileID, profile)
	case mcpprofilerouter.MethodToolsList:
		sessionID := r.Header.Get(mcpprofilerouter.SessionIDHeader)
		var session profileSession
		if sessionID != "" {
			var ok bool
			session, ok = decodeProfileSession(profileID, sessionID)
			if !ok {
				writeJSON(w, http.StatusBadRequest, errorResponse(req.ID, -32010, "invalid MCP session ID"))
				return
			}
		}
		result := g.listProfileToolsNetHTTP(r, req.ID, profile, session)
		writeJSON(w, http.StatusOK, mcpprofilerouter.JSONRPCResponse{
			JSONRPC: "2.0",
			ID:      cloneRaw(req.ID),
			Result:  result,
		})
	case mcpprofilerouter.MethodToolsCall:
		sessionID := r.Header.Get(mcpprofilerouter.SessionIDHeader)
		if sessionID == "" {
			writeJSON(w, http.StatusBadRequest, errorResponse(req.ID, -32010, "missing MCP session ID"))
			return
		}
		session, ok := decodeProfileSession(profileID, sessionID)
		if !ok {
			writeJSON(w, http.StatusBadRequest, errorResponse(req.ID, -32010, "invalid MCP session ID"))
			return
		}
		g.callProfileToolNetHTTP(w, r, req.ID, req.Params, profile, session)
	default:
		writeJSON(w, http.StatusBadRequest, errorResponse(req.ID, -32601, "unsupported profile method: %s", req.Method))
	}
}

func (g *Gateway) initializeProfileNetHTTP(w http.ResponseWriter, r *http.Request, id json.RawMessage, body []byte, profileID string, profile Profile) {
	backends := make(map[string]string, len(profile.Servers))
	for _, serverID := range sortedProfileServerIDs(profile.Servers) {
		server := profile.Servers[serverID]
		resp, err := g.forwardProfileServerNetHTTP(r, body, serverID, server, "", false)
		if err != nil {
			if catalog, ok := g.config.CatalogServers[serverID]; ok {
				g.record(serverID, catalog, "error: "+err.Error())
			}
			continue
		}
		respBody, readErr := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if readErr != nil || resp.StatusCode != http.StatusOK {
			if catalog, ok := g.config.CatalogServers[serverID]; ok {
				g.record(serverID, catalog, "error: initialize")
			}
			continue
		}
		if err := decodeInitializeResult(respBody); err != nil {
			if catalog, ok := g.config.CatalogServers[serverID]; ok {
				g.record(serverID, catalog, "error: "+err.Error())
			}
			continue
		}
		if catalog, ok := g.config.CatalogServers[serverID]; ok {
			g.record(serverID, catalog, "initialized")
		}
		backends[serverID] = resp.Header.Get(mcpprofilerouter.SessionIDHeader)
	}
	if len(backends) == 0 {
		writeJSON(w, http.StatusBadGateway, errorResponse(id, -32002, "initialize failed"))
		return
	}
	sessionID, err := encodeProfileSession(profileID, backends)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, errorResponse(id, -32603, "profile gateway failed"))
		return
	}
	w.Header().Set(mcpprofilerouter.SessionIDHeader, sessionID)
	w.Header().Set(mcpprofilerouter.ProtocolVersionHeader, mcpprofilerouter.ProtocolVersion)
	writeJSON(w, http.StatusOK, mcpprofilerouter.JSONRPCResponse{
		JSONRPC: "2.0",
		ID:      cloneRaw(id),
		Result:  syntheticInitializeResult(),
	})
}

func (g *Gateway) handleCatalogServerNetHTTP(w http.ResponseWriter, r *http.Request) {
	serverID := r.PathValue("server")
	server, ok := g.config.CatalogServers[serverID]
	if !ok {
		writeJSON(w, http.StatusNotFound, errorResponse(nil, -32004, "unknown catalog server: %s", serverID))
		return
	}
	resp, err := g.forwardCatalogNetHTTP(r, serverID, server)
	if err != nil {
		g.record(serverID, server, "error: "+err.Error())
		writeJSON(w, http.StatusBadGateway, errorResponse(nil, -32003, "catalog gateway failed: %s", serverID))
		return
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		g.record(serverID, server, "error: "+err.Error())
		writeJSON(w, http.StatusBadGateway, errorResponse(nil, -32003, "catalog gateway failed: %s", serverID))
		return
	}
	g.record(serverID, server, "ok")
	copyNetHTTPResponseHeaders(w.Header(), resp.Header)
	w.WriteHeader(resp.StatusCode)
	_, _ = w.Write(body)
}

func (g *Gateway) forwardCatalogNetHTTP(r *http.Request, serverID string, server CatalogServer) (*http.Response, error) {
	defer func() { _ = r.Body.Close() }()
	body, err := io.ReadAll(r.Body)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(r.Context(), http.MethodPost, catalogURL(server.URL, serverID), bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("content-type", r.Header.Get("content-type"))
	if req.Header.Get("content-type") == "" {
		req.Header.Set("content-type", "application/json")
	}
	req.Header.Set("accept", r.Header.Get("accept"))
	if req.Header.Get("accept") == "" {
		req.Header.Set("accept", "application/json")
	}
	if sessionID := r.Header.Get(mcpprofilerouter.SessionIDHeader); sessionID != "" {
		req.Header.Set(mcpprofilerouter.SessionIDHeader, sessionID)
	}
	if protocol := r.Header.Get(mcpprofilerouter.ProtocolVersionHeader); protocol != "" {
		req.Header.Set(mcpprofilerouter.ProtocolVersionHeader, protocol)
	}
	return g.client.Do(req)
}

func (g *Gateway) listProfileToolsNetHTTP(r *http.Request, id json.RawMessage, profile Profile, session profileSession) mcpprofilerouter.ListToolsResult {
	body, err := toolsListRequestBody(id)
	if err != nil {
		return mcpprofilerouter.ListToolsResult{}
	}
	merged := make([]mcpprofilerouter.Tool, 0)
	serverIDs := sortedProfileServerIDs(profile.Servers)
	sessionActive := len(session.Backends) > 0
	if sessionActive {
		serverIDs = sortedStringKeys(session.Backends)
	}
	for _, serverID := range serverIDs {
		server, ok := profile.Servers[serverID]
		if !ok {
			continue
		}
		resp, err := g.forwardProfileServerNetHTTP(r, body, serverID, server, session.Backends[serverID], sessionActive)
		if err != nil {
			if catalog, ok := g.config.CatalogServers[serverID]; ok {
				g.record(serverID, catalog, "error: "+err.Error())
			}
			continue
		}
		respBody, readErr := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if readErr != nil || resp.StatusCode != http.StatusOK {
			if catalog, ok := g.config.CatalogServers[serverID]; ok {
				g.record(serverID, catalog, "error: tools/list")
			}
			continue
		}
		tools, err := mergeToolsResult(serverID, server, respBody)
		if err != nil {
			if catalog, ok := g.config.CatalogServers[serverID]; ok {
				g.record(serverID, catalog, "error: "+err.Error())
			}
			continue
		}
		if catalog, ok := g.config.CatalogServers[serverID]; ok {
			g.record(serverID, catalog, "ok")
		}
		merged = append(merged, tools...)
	}
	sortTools(merged)
	return mcpprofilerouter.ListToolsResult{Tools: merged}
}

func (g *Gateway) callProfileToolNetHTTP(w http.ResponseWriter, r *http.Request, id json.RawMessage, params json.RawMessage, profile Profile, session profileSession) {
	serverID, backendTool, callParams, errCode, errMsg := resolveProfileTool(params, profile)
	if errCode != 0 {
		writeJSON(w, http.StatusOK, errorResponse(id, errCode, "%s", errMsg))
		return
	}
	server := profile.Servers[serverID]
	body, err := toolCallForwardBody(id, backendTool, callParams)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, errorResponse(id, -32603, "profile gateway failed"))
		return
	}
	resp, err := g.forwardProfileServerNetHTTP(r, body, serverID, server, session.Backends[serverID], true)
	if err != nil {
		if catalog, ok := g.config.CatalogServers[serverID]; ok {
			g.record(serverID, catalog, "error: "+err.Error())
		}
		writeJSON(w, http.StatusBadGateway, errorResponse(id, -32003, "tool backend failed: %s", serverID))
		return
	}
	defer func() { _ = resp.Body.Close() }()
	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		if catalog, ok := g.config.CatalogServers[serverID]; ok {
			g.record(serverID, catalog, "error: tools/call")
		}
		writeJSON(w, http.StatusBadGateway, errorResponse(id, -32003, "tool backend failed: %s", serverID))
		return
	}
	if catalog, ok := g.config.CatalogServers[serverID]; ok {
		g.record(serverID, catalog, "ok")
	}
	copyNetHTTPResponseHeaders(w.Header(), resp.Header)
	w.WriteHeader(resp.StatusCode)
	_, _ = w.Write(respBody)
}

func (g *Gateway) forwardProfileServerNetHTTP(r *http.Request, body []byte, serverID string, server ProfileServer, backendSessionID string, useBackendSession bool) (*http.Response, error) {
	req, err := http.NewRequestWithContext(r.Context(), http.MethodPost, catalogURL(server.URL, serverID), bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("content-type", "application/json")
	req.Header.Set("accept", "application/json")
	if useBackendSession && backendSessionID != "" {
		req.Header.Set(mcpprofilerouter.SessionIDHeader, backendSessionID)
	}
	if protocol := r.Header.Get(mcpprofilerouter.ProtocolVersionHeader); protocol != "" {
		req.Header.Set(mcpprofilerouter.ProtocolVersionHeader, protocol)
	}
	if server.CredentialRef != "" {
		req.Header.Set("x-mcp-credential-ref", server.CredentialRef)
	}
	if server.CredentialEnvelope != "" {
		req.Header.Set("x-mcp-credential-envelope", server.CredentialEnvelope)
	}
	return g.client.Do(req)
}

func sortedStringKeys(values map[string]string) []string {
	out := make([]string, 0, len(values))
	for k := range values {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func copyNetHTTPResponseHeaders(dst, src http.Header) {
	for name, values := range src {
		if strings.EqualFold(name, "content-length") {
			continue
		}
		for _, value := range values {
			dst.Add(name, value)
		}
	}
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("content-type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
