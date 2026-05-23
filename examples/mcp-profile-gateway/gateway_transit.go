package mcpprofilegateway

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"

	mcpprofilerouter "github.com/dio/transit/examples/mcp-profile-router"
	"github.com/dio/transit/up"
	"github.com/envoyproxy/envoy/source/extensions/dynamic_modules/sdk/go/shared"
)

type transitRequest struct {
	method  string
	path    string
	headers [][2]string
}

// NewTransitFilter exposes the Envoy dynamic-module implementation. Catalog
// forwarding uses HTTPCallout so routing, retries, TLS, DNS, and telemetry stay
// under Envoy rather than bypassing it through Go's net/http client.
func NewTransitFilter(gateway *Gateway) (up.HandlerFunc, up.RequestBodyHandlerFunc) {
	handler := func(_ *up.Writer, r *up.Request) {
		path := stripQuery(r.Path)
		switch {
		case path == "/healthz" || path == "/dump":
		case strings.HasPrefix(path, "/mcp/s/"):
		case strings.HasPrefix(path, "/mcp/"):
		default:
			return
		}
		if r.Context != nil {
			*r.Context = transitRequest{
				method:  r.Method,
				path:    r.Path,
				headers: r.AllHeaders(),
			}
		}
	}

	body := func(w *up.Writer, chunk *up.BodyChunk) {
		if !chunk.EndStream || chunk.Context == nil {
			return
		}
		req, ok := (*chunk.Context).(transitRequest)
		if !ok || req.path == "" {
			return
		}
		if strings.HasPrefix(stripQuery(req.path), "/mcp/s/") {
			gateway.calloutCatalogServerTransit(w, req, chunk.Data)
			return
		}
		if strings.HasPrefix(stripQuery(req.path), "/mcp/") {
			gateway.calloutProfileTransit(w, req, chunk.Data)
			return
		}
		gateway.serveNetHTTPInTransit(w, req, chunk.Data)
	}
	return handler, body
}

// RegisterTransitFilter is the API used by the .so entrypoint. Keeping it in
// the package lets unit tests exercise the net/http implementation without
// importing the Envoy ABI implementation.
func RegisterTransitFilter(name string, config Config) {
	handler, body := NewTransitFilter(New(config))
	up.RegisterWithMutableBody(name, handler, body, nil)
}

func (g *Gateway) calloutCatalogServerTransit(w *up.Writer, r transitRequest, body []byte) {
	serverID, server, ok := g.catalogServerFromTransitPath(r.path)
	if !ok {
		writeTransitJSON(w, http.StatusNotFound, errorResponse(nil, -32004, "unknown catalog server: %s", serverID))
		return
	}
	req, err := g.catalogCalloutRequest(r, body, serverID, server)
	if err != nil {
		g.record(serverID, server, "error: "+err.Error())
		writeTransitJSON(w, http.StatusBadGateway, errorResponse(nil, -32003, "catalog gateway failed: %s", serverID))
		return
	}
	_, err = w.HTTPCallout(req, func(result up.HTTPCalloutResult, headers [][2]shared.UnsafeEnvoyBuffer, respBody []shared.UnsafeEnvoyBuffer) {
		if result != up.HTTPCalloutSuccess {
			g.record(serverID, server, fmt.Sprintf("error: callout result %d", result))
			writeTransitJSON(w, http.StatusBadGateway, errorResponse(nil, -32003, "catalog gateway failed: %s", serverID))
			return
		}
		g.record(serverID, server, "ok")
		status, responseHeaders := responseFromCalloutHeaders(headers)
		w.SendLocalResponse(status, calloutBodyBytes(respBody), responseHeaders...)
	})
	if err != nil {
		g.record(serverID, server, "error: "+err.Error())
		writeTransitJSON(w, http.StatusBadGateway, errorResponse(nil, -32003, "catalog gateway failed: %s", serverID))
	}
}

func (g *Gateway) calloutProfileTransit(w *up.Writer, r transitRequest, body []byte) {
	profileID, profile, ok := g.profileFromTransitPath(r.path)
	if !ok {
		writeTransitJSON(w, http.StatusNotFound, errorResponse(nil, -32004, "unknown profile: %s", profileID))
		return
	}
	if !profileAuthorized(profile.APIKey, func(name string) string { return headerValue(r.headers, name) }) {
		writeTransitJSON(w, http.StatusUnauthorized, errorResponse(nil, -32001, "unauthorized profile: %s", profileID))
		return
	}
	var rpcReq mcpprofilerouter.JSONRPCRequest
	if err := json.Unmarshal(body, &rpcReq); err != nil {
		writeTransitJSON(w, http.StatusBadRequest, errorResponse(nil, -32700, "invalid JSON-RPC request"))
		return
	}
	switch rpcReq.Method {
	case mcpprofilerouter.MethodInitialize:
		g.initializeProfileTransit(w, r, rpcReq.ID, body, profileID, profile)
	case mcpprofilerouter.MethodToolsList:
		g.listProfileToolsTransit(w, r, rpcReq.ID, profileID, profile)
	default:
		writeTransitJSON(w, http.StatusBadRequest, errorResponse(rpcReq.ID, -32601, "unsupported profile method: %s", rpcReq.Method))
	}
}

func (g *Gateway) initializeProfileTransit(w *up.Writer, r transitRequest, id json.RawMessage, body []byte, profileID string, profile Profile) {
	reqs, serverIDs, err := g.profileInitializeCalloutRequests(r, body, profile)
	if err != nil {
		writeTransitJSON(w, http.StatusInternalServerError, errorResponse(id, -32603, "profile gateway failed"))
		return
	}
	err = w.HTTPCalloutAllSettled(reqs, func(responses []up.HTTPCalloutAllSettledResponse) {
		backends := make(map[string]string, len(responses))
		for i, resp := range responses {
			serverID := serverIDs[i]
			catalogServer := g.config.CatalogServers[serverID]
			if resp.Failed() {
				g.record(serverID, catalogServer, "error: initialize")
				continue
			}
			status, _ := responseFromCalloutHeaders(resp.Headers)
			if status != http.StatusOK {
				g.record(serverID, catalogServer, fmt.Sprintf("error: status %d", status))
				continue
			}
			if err := decodeInitializeResult(calloutBodyBytes(resp.Body)); err != nil {
				g.record(serverID, catalogServer, "error: "+err.Error())
				continue
			}
			g.record(serverID, catalogServer, "initialized")
			backends[serverID] = calloutHeaderValue(resp.Headers, mcpprofilerouter.SessionIDHeader)
		}
		if len(backends) == 0 {
			writeTransitJSON(w, http.StatusBadGateway, errorResponse(id, -32002, "initialize failed"))
			return
		}
		sessionID, err := encodeProfileSession(profileID, backends)
		if err != nil {
			writeTransitJSON(w, http.StatusInternalServerError, errorResponse(id, -32603, "profile gateway failed"))
			return
		}
		writeTransitJSON(w, http.StatusOK, mcpprofilerouter.JSONRPCResponse{
			JSONRPC: "2.0",
			ID:      cloneRaw(id),
			Result:  syntheticInitializeResult(),
		}, [2]string{mcpprofilerouter.SessionIDHeader, sessionID}, [2]string{mcpprofilerouter.ProtocolVersionHeader, mcpprofilerouter.ProtocolVersion})
	})
	if err != nil {
		writeTransitJSON(w, http.StatusBadGateway, errorResponse(id, -32002, "initialize failed"))
	}
}

func (g *Gateway) listProfileToolsTransit(w *up.Writer, r transitRequest, id json.RawMessage, profileID string, profile Profile) {
	var session profileSession
	sessionID := headerValue(r.headers, mcpprofilerouter.SessionIDHeader)
	if sessionID != "" {
		var ok bool
		session, ok = decodeProfileSession(profileID, sessionID)
		if !ok {
			writeTransitJSON(w, http.StatusBadRequest, errorResponse(id, -32010, "invalid MCP session ID"))
			return
		}
	}
	reqs, serverIDs, err := g.profileToolsListCalloutRequests(r, id, profile, session)
	if err != nil {
		writeTransitJSON(w, http.StatusInternalServerError, errorResponse(id, -32603, "profile gateway failed"))
		return
	}
	err = w.HTTPCalloutAllSettled(reqs, func(responses []up.HTTPCalloutAllSettledResponse) {
		merged := make([]mcpprofilerouter.Tool, 0)
		for i, resp := range responses {
			serverID := serverIDs[i]
			profileServer := profile.Servers[serverID]
			catalogServer := g.config.CatalogServers[serverID]
			if resp.Failed() {
				g.record(serverID, catalogServer, "error: tools/list")
				continue
			}
			status, _ := responseFromCalloutHeaders(resp.Headers)
			if status != http.StatusOK {
				g.record(serverID, catalogServer, fmt.Sprintf("error: status %d", status))
				continue
			}
			tools, err := mergeToolsResult(serverID, profileServer, calloutBodyBytes(resp.Body))
			if err != nil {
				g.record(serverID, catalogServer, "error: "+err.Error())
				continue
			}
			g.record(serverID, catalogServer, "ok")
			merged = append(merged, tools...)
		}
		sortTools(merged)
		writeTransitJSON(w, http.StatusOK, mcpprofilerouter.JSONRPCResponse{
			JSONRPC: "2.0",
			ID:      cloneRaw(id),
			Result:  mcpprofilerouter.ListToolsResult{Tools: merged},
		})
	})
	if err != nil {
		writeTransitJSON(w, http.StatusBadGateway, errorResponse(id, -32002, "tools/list failed"))
	}
}

func (g *Gateway) catalogCalloutRequest(r transitRequest, body []byte, serverID string, server CatalogServer) (up.HTTPCalloutRequest, error) {
	u, err := url.Parse(catalogURL(server.URL, serverID))
	if err != nil {
		return up.HTTPCalloutRequest{}, err
	}
	headers := [][2]string{
		{":method", http.MethodPost},
		{":path", u.RequestURI()},
		{":scheme", u.Scheme},
		{"host", u.Host},
		{"content-type", headerOrDefault(r.headers, "content-type", "application/json")},
		{"accept", headerOrDefault(r.headers, "accept", "application/json")},
		{"x-mcp-credential-ref", ""},
		{"x-mcp-credential-envelope", ""},
	}
	if sessionID := headerValue(r.headers, mcpprofilerouter.SessionIDHeader); sessionID != "" {
		headers = append(headers, [2]string{mcpprofilerouter.SessionIDHeader, sessionID})
	}
	if protocol := headerValue(r.headers, mcpprofilerouter.ProtocolVersionHeader); protocol != "" {
		headers = append(headers, [2]string{mcpprofilerouter.ProtocolVersionHeader, protocol})
	}
	return up.HTTPCalloutRequest{
		Cluster:       catalogCluster(server),
		Headers:       headers,
		Body:          body,
		TimeoutMillis: timeoutMillis(g.config.TimeoutMillis),
	}, nil
}

func (g *Gateway) profileInitializeCalloutRequests(r transitRequest, body []byte, profile Profile) ([]up.HTTPCalloutRequest, []string, error) {
	return g.profileCalloutRequests(r, body, profile, profileSession{})
}

func (g *Gateway) profileToolsListCalloutRequests(r transitRequest, id json.RawMessage, profile Profile, session profileSession) ([]up.HTTPCalloutRequest, []string, error) {
	body, err := toolsListRequestBody(id)
	if err != nil {
		return nil, nil, err
	}
	return g.profileCalloutRequests(r, body, profile, session)
}

func (g *Gateway) profileCalloutRequests(r transitRequest, body []byte, profile Profile, session profileSession) ([]up.HTTPCalloutRequest, []string, error) {
	serverIDs := sortedProfileServerIDs(profile.Servers)
	sessionActive := len(session.Backends) > 0
	if sessionActive {
		serverIDs = sortedStringKeys(session.Backends)
	}
	reqs := make([]up.HTTPCalloutRequest, 0, len(serverIDs))
	outIDs := make([]string, 0, len(serverIDs))
	for _, serverID := range serverIDs {
		profileServer, ok := profile.Servers[serverID]
		if !ok {
			continue
		}
		catalogServer := g.config.CatalogServers[serverID]
		u, err := url.Parse(catalogURL(profileServer.URL, serverID))
		if err != nil {
			return nil, nil, err
		}
		headers := [][2]string{
			{":method", http.MethodPost},
			{":path", u.RequestURI()},
			{":scheme", u.Scheme},
			{"host", u.Host},
			{"content-type", "application/json"},
			{"accept", "application/json"},
		}
		if sessionActive {
			if backendSessionID := session.Backends[serverID]; backendSessionID != "" {
				headers = append(headers, [2]string{mcpprofilerouter.SessionIDHeader, backendSessionID})
			}
		}
		if protocol := headerValue(r.headers, mcpprofilerouter.ProtocolVersionHeader); protocol != "" {
			headers = append(headers, [2]string{mcpprofilerouter.ProtocolVersionHeader, protocol})
		}
		if profileServer.CredentialRef != "" {
			headers = append(headers, [2]string{"x-mcp-credential-ref", profileServer.CredentialRef})
		}
		if profileServer.CredentialEnvelope != "" {
			headers = append(headers, [2]string{"x-mcp-credential-envelope", profileServer.CredentialEnvelope})
		}
		reqs = append(reqs, up.HTTPCalloutRequest{
			Cluster:       catalogCluster(catalogServer),
			Headers:       headers,
			Body:          body,
			TimeoutMillis: timeoutMillis(g.config.TimeoutMillis),
		})
		outIDs = append(outIDs, serverID)
	}
	return reqs, outIDs, nil
}

func (g *Gateway) serveNetHTTPInTransit(w *up.Writer, r transitRequest, body []byte) {
	httpReq := httptest.NewRequest(r.method, r.path, bytes.NewReader(body))
	for _, h := range r.headers {
		if strings.HasPrefix(h[0], ":") {
			continue
		}
		httpReq.Header.Add(h[0], h[1])
	}
	rec := httptest.NewRecorder()
	g.Handler().ServeHTTP(rec, httpReq)
	resp := rec.Result()
	defer func() { _ = resp.Body.Close() }()

	headers := make([][2]string, 0, len(resp.Header))
	for name, values := range resp.Header {
		for _, value := range values {
			headers = append(headers, [2]string{name, value})
		}
	}
	w.SendLocalResponse(resp.StatusCode, rec.Body.Bytes(), headers...)
}

func (g *Gateway) catalogServerFromTransitPath(path string) (string, CatalogServer, bool) {
	rawID, ok := strings.CutPrefix(stripQuery(path), "/mcp/s/")
	if !ok || rawID == "" {
		return "", CatalogServer{}, false
	}
	serverID, err := url.PathUnescape(rawID)
	if err != nil {
		return rawID, CatalogServer{}, false
	}
	server, ok := g.config.CatalogServers[serverID]
	return serverID, server, ok
}

func (g *Gateway) profileFromTransitPath(path string) (string, Profile, bool) {
	rawID, ok := strings.CutPrefix(stripQuery(path), "/mcp/")
	if !ok || rawID == "" || strings.HasPrefix(rawID, "s/") {
		return "", Profile{}, false
	}
	profileID, err := url.PathUnescape(rawID)
	if err != nil {
		return rawID, Profile{}, false
	}
	profile, ok := g.config.Profiles[profileID]
	return profileID, profile, ok
}

func catalogCluster(server CatalogServer) string {
	if server.Cluster != "" {
		return server.Cluster
	}
	return DefaultCatalogCalloutCluster
}

func timeoutMillis(configured int) uint64 {
	if configured > 0 {
		return uint64(configured)
	}
	return 800
}

func responseFromCalloutHeaders(headers [][2]shared.UnsafeEnvoyBuffer) (int, [][2]string) {
	status := http.StatusOK
	out := make([][2]string, 0, len(headers))
	for _, h := range headers {
		name := h[0].ToString()
		value := h[1].ToString()
		switch {
		case name == ":status":
			if parsed, err := strconv.Atoi(value); err == nil {
				status = parsed
			}
		case strings.HasPrefix(name, ":"):
			continue
		case strings.EqualFold(name, "content-length"):
			continue
		default:
			out = append(out, [2]string{name, value})
		}
	}
	return status, out
}

func calloutBodyBytes(buffers []shared.UnsafeEnvoyBuffer) []byte {
	if len(buffers) == 0 {
		return nil
	}
	var out []byte
	for _, b := range buffers {
		out = append(out, b.ToString()...)
	}
	return out
}

func headerValue(headers [][2]string, name string) string {
	for _, h := range headers {
		if strings.EqualFold(h[0], name) {
			return h[1]
		}
	}
	return ""
}

func headerOrDefault(headers [][2]string, name, fallback string) string {
	if value := headerValue(headers, name); value != "" {
		return value
	}
	return fallback
}

func calloutHeaderValue(headers [][2]shared.UnsafeEnvoyBuffer, name string) string {
	for _, h := range headers {
		if strings.EqualFold(h[0].ToString(), name) {
			return h[1].ToString()
		}
	}
	return ""
}

func writeTransitJSON(w *up.Writer, status int, v any, headers ...[2]string) {
	body, err := json.Marshal(v)
	if err != nil {
		w.SendLocalResponse(http.StatusInternalServerError, []byte(`{"jsonrpc":"2.0","error":{"code":-32603,"message":"internal error"}}`), [2]string{"content-type", "application/json"})
		return
	}
	out := make([][2]string, 0, len(headers)+1)
	out = append(out, [2]string{"content-type", "application/json"})
	out = append(out, headers...)
	w.SendLocalResponse(status, body, out...)
}
