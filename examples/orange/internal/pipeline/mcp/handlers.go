package mcp

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/dio/transit/examples/orange/internal/config"
)

const (
	methodInitialize          = "initialize"
	methodNotificationsInit   = "notifications/initialized"
	methodToolsList           = "tools/list"
	methodPromptsList         = "prompts/list"
	methodResourcesList       = "resources/list"
	methodToolsCall           = "tools/call"
	heartbeatMethod           = "ping"
	toolSeparator             = "__"
	egressPathPrefix          = "/_orange_mcp_egress/"
	defaultRouteName          = "default"
	maxBackendResponseBody    = 8 << 20
	defaultBackendHTTPTimeout = 30 * time.Second
)

type configProvider func() *config.Config

type recordSink func(record)

type handlerOptions struct {
	egressURL string
	config    configProvider
	crypto    sessionCrypto
	client    *http.Client
	records   recordSink
}

type handler struct {
	options handlerOptions
}

type mcpRoute struct {
	Backends         map[string]config.MCPServer
	OptionalBackends map[string]bool
}

func newHandler(opts handlerOptions) *handler {
	if opts.config == nil {
		opts.config = config.Get
	}
	if opts.crypto == nil {
		opts.crypto = newSessionCrypto("orange-mcp-test-session-key")
	}
	if opts.client == nil {
		opts.client = &http.Client{Timeout: defaultBackendHTTPTimeout}
	}
	return &handler{options: opts}
}

func (h *handler) egressURL() string {
	return h.options.egressURL
}

func (h *handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == "/ready" {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	switch r.Method {
	case http.MethodPost:
		h.servePOST(w, r)
	case http.MethodGet:
		h.serveGET(w, r)
	case http.MethodDelete:
		h.serveDELETE(w, r)
	default:
		writeJSON(w, http.StatusMethodNotAllowed, localError("orange.mcp_method_not_allowed", "method is not supported"))
	}
}

func (h *handler) servePOST(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	req, raw, err := readRPCRequest(r.Body)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, localError("orange.mcp_invalid_jsonrpc", "invalid JSON-RPC request"))
		return
	}
	route := h.routeForRequest(r)
	requestID := requestIDFor(r)
	rec := record{
		Method:    req.Method,
		Route:     route,
		RequestID: requestID,
		Start:     start,
	}
	defer func() { h.publishRecord(rec.finish(time.Since(start))) }()
	w.Header().Set(headerMethod, req.Method)

	var session sessionEnvelope
	if req.Method != methodInitialize {
		token := r.Header.Get(sessionIDHeader)
		if token == "" {
			rec.Outcome = "error"
			rec.ErrorClass = "missing_session"
			writeJSON(w, http.StatusBadRequest, rpcErrorResponse(req.ID, -32001, "missing mcp-session-id"))
			return
		}
		session, err = decodeSecureSessionID(h.options.crypto, token, h.subjectForRequest(r))
		if err != nil {
			rec.Outcome = "error"
			rec.ErrorClass = "invalid_session"
			writeJSON(w, http.StatusBadRequest, rpcErrorResponse(req.ID, -32001, "invalid mcp-session-id"))
			return
		}
		route = session.Route
		rec.Route = route
		rec.PublicSessionHash = hashPublicToken(token)
	}

	switch req.Method {
	case methodInitialize:
		h.handleInitialize(w, r, req, raw, route, requestID, &rec)
	case methodToolsList, methodPromptsList, methodResourcesList:
		h.handleList(w, r, req, raw, session, requestID, &rec)
	case methodToolsCall:
		h.handleToolsCall(w, r, req, session, requestID, &rec)
	case "":
		h.handleClientResponse(w, r, req, raw, session, requestID, &rec)
	default:
		h.handleSingleBackendBroadcast(w, r, req, raw, session, requestID, &rec)
	}
}

func (h *handler) handleInitialize(w http.ResponseWriter, r *http.Request, req rpcRequest, raw []byte, routeName, requestID string, rec *record) {
	cfg := h.options.config()
	route, ok := lookupMCPRoute(cfg, routeName)
	if !ok {
		rec.Outcome = "error"
		rec.ErrorClass = "route_not_found"
		writeJSON(w, http.StatusBadRequest, rpcErrorResponse(req.ID, -32602, "unknown MCP route"))
		return
	}

	results := h.fanOut(r.Context(), routeName, route, raw, "", "", req.Method, "", requestID)
	rec.Backends = resultBackends(results)
	rec.LegCount = len(results)
	rec.FailedLegs = countFailed(results)

	entries := make([]backendSession, 0, len(results))
	var first *backendResult
	var requiredFailed bool
	for i := range results {
		if results[i].err != nil || results[i].status < 200 || results[i].status >= 300 {
			if !route.OptionalBackends[results[i].backend] {
				requiredFailed = true
			}
			continue
		}
		if first == nil {
			first = &results[i]
		}
		entries = append(entries, backendSession{
			Backend:      results[i].backend,
			SessionID:    results[i].sessionID,
			Capabilities: capabilitiesFromInitialize(results[i].body),
		})
	}
	w.Header().Set(headerBackendStatus, buildBackendStatusHeader(results, route.OptionalBackends))
	if requiredFailed || len(entries) == 0 {
		rec.Outcome = "error"
		rec.ErrorClass = "profile_initialize_failed"
		writeJSON(w, http.StatusBadGateway, rpcErrorResponse(req.ID, -32002, "MCP profile initialize failed"))
		return
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Backend < entries[j].Backend })
	publicSession, err := encodeSecureSessionID(h.options.crypto, sessionEnvelope{
		Route:    routeName,
		Subject:  h.subjectForRequest(r),
		Backends: entries,
	})
	if err != nil {
		rec.Outcome = "error"
		rec.ErrorClass = "session_encode"
		writeJSON(w, http.StatusInternalServerError, rpcErrorResponse(req.ID, -32603, "failed to encode MCP session"))
		return
	}
	w.Header().Set(sessionIDHeader, publicSession)
	rec.PublicSessionHash = hashPublicToken(publicSession)
	rec.Outcome = "success"
	writeJSONBytes(w, http.StatusOK, rewriteInitializeServerInfo(first.body, req))
}

func (h *handler) handleList(w http.ResponseWriter, r *http.Request, req rpcRequest, raw []byte, session sessionEnvelope, requestID string, rec *record) {
	results := h.fanOutSession(r.Context(), session, raw, req.Method, "", requestID)
	rec.Backends = resultBackends(results)
	rec.LegCount = len(results)
	rec.FailedLegs = countFailed(results)
	cfg := h.options.config()
	route, _ := lookupMCPRoute(cfg, session.Route)
	w.Header().Set(headerBackendStatus, buildBackendStatusHeader(results, route.OptionalBackends))
	if len(results) == 0 || rec.FailedLegs == len(results) {
		rec.Outcome = "error"
		rec.ErrorClass = "all_backends_failed"
		writeJSON(w, http.StatusBadGateway, rpcErrorResponse(req.ID, -32002, "all MCP list backends failed"))
		return
	}

	merged, err := mergeListResponse(req, results)
	if err != nil {
		rec.Outcome = "error"
		rec.ErrorClass = "merge_failed"
		writeJSON(w, http.StatusBadGateway, rpcErrorResponse(req.ID, -32003, "failed to merge MCP list response"))
		return
	}
	rec.Outcome = "success"
	writeJSONBytes(w, http.StatusOK, merged)
}

func (h *handler) handleToolsCall(w http.ResponseWriter, r *http.Request, req rpcRequest, session sessionEnvelope, requestID string, rec *record) {
	backend, stripped, rewritten, err := rewriteToolCall(req)
	if err != nil {
		rec.Outcome = "error"
		rec.ErrorClass = "invalid_tool"
		writeJSON(w, http.StatusBadRequest, rpcErrorResponse(req.ID, -32602, "invalid prefixed tool name"))
		return
	}
	entry, ok := session.backend(backend)
	if !ok {
		rec.Outcome = "error"
		rec.ErrorClass = "backend_not_in_session"
		writeJSON(w, http.StatusBadRequest, rpcErrorResponse(req.ID, -32602, "tool backend is not in session"))
		return
	}
	rec.Backends = []string{backend}
	rec.SelectedBackend = backend
	rec.Tool = backend + toolSeparator + stripped
	w.Header().Set(headerTool, rec.Tool)
	result := h.doBackend(r.Context(), session.Route, backend, rewritten, entry.SessionID, "", req.Method, stripped, requestID, http.MethodPost)
	rec.LegCount = 1
	if result.err != nil || result.status < 200 || result.status >= 300 {
		rec.FailedLegs = 1
		rec.Outcome = "error"
		rec.ErrorClass = "backend_failed"
		writeJSON(w, http.StatusBadGateway, rpcErrorResponse(req.ID, -32002, "MCP tool backend failed"))
		return
	}
	if toolCallIsError(result.body) {
		rec.Outcome = "application_error"
		rec.ErrorClass = "tool_is_error"
	} else {
		rec.Outcome = "success"
	}
	writeJSONBytes(w, http.StatusOK, result.body)
}

func (h *handler) handleClientResponse(w http.ResponseWriter, r *http.Request, req rpcRequest, raw []byte, session sessionEnvelope, requestID string, rec *record) {
	backend := backendFromResponseID(req.ID)
	if backend == "" {
		// Client is responding to a server-initiated request (e.g. the ping we send on SSE open).
		// The ID is a plain string, not a {"backend":"...","id":...} envelope, so there is nothing
		// to forward. We ack and return.
		//
		// This path fires on every heartbeat and will litter log storage at scale. Options:
		//   1. Set rec.Skip = true (requires a skip field on record + a guard in publishRecord)
		//      to suppress these entries entirely from the sink.
		//   2. Set a dedicated rec.Outcome such as "heartbeat" so downstream log pipelines can
		//      cheaply drop or sample them with a filter (e.g. Envoy access log filter on
		//      response_code_details or a log-aggregator drop rule).
		//   3. Add an Envoy access log filter that drops 202 responses on /mcp/* whose
		//      x-mcp-method header is empty, at the proxy layer before they hit storage.
		rec.Outcome = "success"
		writeJSON(w, http.StatusAccepted, map[string]any{"ok": true})
		return
	}
	entry, ok := session.backend(backend)
	if !ok {
		rec.Outcome = "error"
		rec.ErrorClass = "backend_not_in_session"
		writeJSON(w, http.StatusBadRequest, rpcErrorResponse(req.ID, -32602, "response backend is not in session"))
		return
	}
	result := h.doBackend(r.Context(), session.Route, backend, raw, entry.SessionID, "", "client_response", "", requestID, http.MethodPost)
	rec.SelectedBackend = backend
	rec.Backends = []string{backend}
	rec.LegCount = 1
	if result.err != nil || result.status < 200 || result.status >= 300 {
		rec.FailedLegs = 1
		rec.Outcome = "error"
		rec.ErrorClass = "backend_failed"
		writeJSON(w, http.StatusBadGateway, rpcErrorResponse(req.ID, -32002, "MCP response backend failed"))
		return
	}
	rec.Outcome = "success"
	writeJSONBytes(w, http.StatusOK, result.body)
}

func (h *handler) handleSingleBackendBroadcast(w http.ResponseWriter, r *http.Request, req rpcRequest, raw []byte, session sessionEnvelope, requestID string, rec *record) {
	results := h.fanOutSession(r.Context(), session, raw, req.Method, "", requestID)
	rec.Backends = resultBackends(results)
	rec.LegCount = len(results)
	rec.FailedLegs = countFailed(results)
	cfg := h.options.config()
	route, _ := lookupMCPRoute(cfg, session.Route)
	w.Header().Set(headerBackendStatus, buildBackendStatusHeader(results, route.OptionalBackends))
	for _, result := range results {
		if result.err == nil && result.status >= 200 && result.status < 300 {
			rec.Outcome = "success"
			writeJSONBytes(w, http.StatusOK, result.body)
			return
		}
	}
	rec.Outcome = "error"
	rec.ErrorClass = "all_backends_failed"
	writeJSON(w, http.StatusBadGateway, rpcErrorResponse(req.ID, -32002, "all MCP backends failed"))
}

func (h *handler) serveGET(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	token := r.Header.Get(sessionIDHeader)
	rec := record{Method: http.MethodGet, RequestID: requestIDFor(r), Start: start, PublicSessionHash: hashPublicToken(token)}
	defer func() { h.publishRecord(rec.finish(time.Since(start))) }()
	if token == "" {
		rec.Outcome = "error"
		rec.ErrorClass = "missing_session"
		writeJSON(w, http.StatusBadRequest, localError("orange.mcp_missing_session", "missing mcp-session-id"))
		return
	}
	session, err := decodeSecureSessionID(h.options.crypto, token, h.subjectForRequest(r))
	if err != nil {
		rec.Outcome = "error"
		rec.ErrorClass = "invalid_session"
		writeJSON(w, http.StatusBadRequest, localError("orange.mcp_invalid_session", "invalid mcp-session-id"))
		return
	}
	rec.Route = session.Route
	rec.Backends = session.backendNames()

	lastEvent := eventEnvelope{}
	if raw := r.Header.Get(lastEventIDHeader); raw != "" {
		lastEvent, _ = decodeSecureLastEventID(h.options.crypto, raw)
	}

	w.Header().Set(sessionIDHeader, token)
	w.Header().Set("content-type", "text/event-stream")
	w.Header().Set("cache-control", "no-cache")
	w.WriteHeader(http.StatusAccepted)

	heartbeat := &sseEvent{Event: "message", Messages: []jsonrpcMessage{jsonrpcMessage(`{"jsonrpc":"2.0","method":"ping","id":"orange-mcp-heartbeat"}`)}}
	heartbeat.writeAndMaybeFlush(w)

	h.streamBackendEvents(r.Context(), w, session, lastEvent, rec.RequestID)
	rec.Outcome = "success"
}

func (h *handler) streamBackendEvents(ctx context.Context, w io.Writer, session sessionEnvelope, lastEvent eventEnvelope, requestID string) {
	var mu sync.Mutex
	var wg sync.WaitGroup
	for _, entry := range session.Backends {
		entry := entry
		wg.Add(1)
		go func() {
			defer wg.Done()
			backendLastEvent := ""
			if lastEvent.Backend == entry.Backend {
				backendLastEvent = lastEvent.EventID
			}
			result := h.doBackend(ctx, session.Route, entry.Backend, nil, entry.SessionID, backendLastEvent, http.MethodGet, "", requestID, http.MethodGet)
			if result.err != nil || result.status < 200 || result.status >= 300 || len(result.body) == 0 {
				return
			}
			parser := newSSEParser(bytes.NewReader(result.body))
			for {
				ev, err := parser.next()
				if ev != nil {
					if ev.ID != "" {
						if encoded, encErr := encodeSecureLastEventID(h.options.crypto, eventEnvelope{Backend: entry.Backend, EventID: ev.ID}); encErr == nil {
							ev.ID = encoded
						}
					}
					for i, msg := range ev.Messages {
						ev.Messages[i] = rewriteServerRequestID(msg, entry.Backend)
					}
					mu.Lock()
					ev.writeAndMaybeFlush(w)
					mu.Unlock()
				}
				if err != nil {
					return
				}
			}
		}()
	}
	wg.Wait()
}

func (h *handler) serveDELETE(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	token := r.Header.Get(sessionIDHeader)
	rec := record{Method: http.MethodDelete, RequestID: requestIDFor(r), Start: start, PublicSessionHash: hashPublicToken(token)}
	defer func() { h.publishRecord(rec.finish(time.Since(start))) }()
	if token == "" {
		rec.Outcome = "error"
		rec.ErrorClass = "missing_session"
		writeJSON(w, http.StatusBadRequest, localError("orange.mcp_missing_session", "missing mcp-session-id"))
		return
	}
	session, err := decodeSecureSessionID(h.options.crypto, token, h.subjectForRequest(r))
	if err != nil {
		rec.Outcome = "error"
		rec.ErrorClass = "invalid_session"
		writeJSON(w, http.StatusBadRequest, localError("orange.mcp_invalid_session", "invalid mcp-session-id"))
		return
	}
	rec.Route = session.Route
	rec.Backends = session.backendNames()
	for _, entry := range session.Backends {
		_ = h.doBackend(r.Context(), session.Route, entry.Backend, nil, entry.SessionID, "", http.MethodDelete, "", rec.RequestID, http.MethodDelete)
	}
	rec.LegCount = len(session.Backends)
	rec.Outcome = "success"
	w.WriteHeader(http.StatusOK)
}

func (h *handler) fanOut(ctx context.Context, routeName string, route mcpRoute, body []byte, sessionID, lastEventID, method, tool, requestID string) []backendResult {
	backends := sortedConfigBackends(route)
	results := make([]backendResult, len(backends))
	var wg sync.WaitGroup
	for i, backendName := range backends {
		wg.Add(1)
		go func(i int, backendName string) {
			defer wg.Done()
			results[i] = h.doBackend(ctx, routeName, backendName, body, sessionID, lastEventID, method, tool, requestID, http.MethodPost)
		}(i, backendName)
	}
	wg.Wait()
	return results
}

func (h *handler) fanOutSession(ctx context.Context, session sessionEnvelope, body []byte, method, tool, requestID string) []backendResult {
	results := make([]backendResult, len(session.Backends))
	var wg sync.WaitGroup
	for i, entry := range session.Backends {
		wg.Add(1)
		go func(i int, entry backendSession) {
			defer wg.Done()
			results[i] = h.doBackend(ctx, session.Route, entry.Backend, body, entry.SessionID, "", method, tool, requestID, http.MethodPost)
		}(i, entry)
	}
	wg.Wait()
	return results
}

func (h *handler) doBackend(ctx context.Context, routeName, backendName string, body []byte, sessionID, lastEventID, method, tool, requestID, httpMethod string) backendResult {
	result := backendResult{backend: backendName}
	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}
	target, err := backendEgressURL(h.options.egressURL, backendName)
	if err != nil {
		result.err = err
		return result
	}
	req, err := http.NewRequestWithContext(ctx, httpMethod, target, reader)
	if err != nil {
		result.err = err
		return result
	}
	req.Header.Set("content-type", "application/json")
	req.Header.Set("accept", "text/event-stream, application/json")
	req.Header.Set(headerRoute, routeName)
	req.Header.Set(headerBackend, backendName)
	req.Header.Set(headerMethod, method)
	req.Header.Set(headerRequestID, requestID)
	if tool != "" {
		req.Header.Set(headerTool, tool)
	}
	if sessionID != "" {
		req.Header.Set(sessionIDHeader, sessionID)
		req.Header.Set(headerSession, "present")
	}
	if lastEventID != "" {
		req.Header.Set(lastEventIDHeader, lastEventID)
		req.Header.Set(headerLastEventID, "present")
	}
	resp, err := h.options.client.Do(req)
	if err != nil {
		result.err = err
		return result
	}
	defer func() { _ = resp.Body.Close() }()
	result.status = resp.StatusCode
	result.sessionID = resp.Header.Get(sessionIDHeader)
	result.body, result.err = io.ReadAll(io.LimitReader(resp.Body, maxBackendResponseBody))
	return result
}

func backendEgressURL(baseURL, backendName string) (string, error) {
	u, err := url.Parse(baseURL)
	if err != nil {
		return "", err
	}
	basePath := strings.TrimRight(u.Path, "/")
	u.Path = basePath + egressPathPrefix + url.PathEscape(backendName)
	return u.String(), nil
}

func (h *handler) routeForRequest(r *http.Request) string {
	if route := routeFromPath(r.URL.Path); route != "" {
		return route
	}
	return defaultRouteName
}

func routeFromPath(path string) string {
	const prefix = "/mcp/"
	if !strings.HasPrefix(path, prefix) {
		return ""
	}
	rest := strings.TrimPrefix(path, prefix)
	if rest == "" {
		return ""
	}
	if serverPath, ok := strings.CutPrefix(rest, "s/"); ok {
		serverSegment, _, _ := strings.Cut(serverPath, "/")
		server, err := url.PathUnescape(serverSegment)
		if err != nil || server == "" {
			return ""
		}
		return "s/" + server
	}
	segment, _, _ := strings.Cut(rest, "/")
	route, err := url.PathUnescape(segment)
	if err != nil {
		return ""
	}
	return route
}

func (h *handler) subjectForRequest(r *http.Request) string {
	return r.Header.Get(headerSubject)
}

func (h *handler) publishRecord(r record) {
	if h.options.records != nil {
		h.options.records(r)
	}
}

type backendResult struct {
	backend   string
	status    int
	sessionID string
	body      []byte
	err       error
}

func lookupMCPRoute(cfg *config.Config, routeName string) (mcpRoute, bool) {
	if cfg == nil || cfg.MCP == nil {
		return mcpRoute{}, false
	}
	if serverName, ok := strings.CutPrefix(routeName, "s/"); ok {
		server, ok := cfg.MCP.Servers[serverName]
		if !ok {
			return mcpRoute{}, false
		}
		return mcpRoute{Backends: map[string]config.MCPServer{serverName: server}}, true
	}
	profile, ok := cfg.MCP.Profiles[routeName]
	if !ok {
		return mcpRoute{}, false
	}
	backends := make(map[string]config.MCPServer, len(profile.Tools))
	optional := make(map[string]bool, len(profile.Tools))
	for serverName, pt := range profile.Tools {
		server, ok := cfg.MCP.Servers[serverName]
		if !ok {
			return mcpRoute{}, false
		}
		backends[serverName] = server
		if pt.Optional {
			optional[serverName] = true
		}
	}
	return mcpRoute{Backends: backends, OptionalBackends: optional}, true
}

func sortedConfigBackends(route mcpRoute) []string {
	names := make([]string, 0, len(route.Backends))
	for name := range route.Backends {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

func resultBackends(results []backendResult) []string {
	out := make([]string, 0, len(results))
	for _, result := range results {
		out = append(out, result.backend)
	}
	return out
}

func countFailed(results []backendResult) int {
	var n int
	for _, result := range results {
		if result.err != nil || result.status < 200 || result.status >= 300 {
			n++
		}
	}
	return n
}

func requestIDFor(r *http.Request) string {
	if id := r.Header.Get("x-request-id"); id != "" {
		return id
	}
	return uuid.NewString()
}

func hashPublicToken(token string) string {
	if token == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:8])
}

func buildBackendStatusHeader(results []backendResult, optional map[string]bool) string {
	parts := make([]string, 0, len(results))
	for _, r := range results {
		status := "ok"
		if r.err != nil || r.status < 200 || r.status >= 300 {
			if optional[r.backend] {
				status = "optional-failed"
			} else {
				status = "failed"
			}
		}
		parts = append(parts, r.backend+"="+status)
	}
	return strings.Join(parts, ",")
}
