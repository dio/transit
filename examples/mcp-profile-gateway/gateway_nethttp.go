package mcpprofilegateway

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
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
	return mux
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
