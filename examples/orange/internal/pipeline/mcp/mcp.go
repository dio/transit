// Package mcp hosts the Orange-managed MCP streamable-HTTP/SSE sidecar.
package mcp

import (
	"encoding/json"
	"net/http"
	"os"
	"time"

	"github.com/dio/transit/up"
)

const (
	FilterName       = "orange-mcp"
	EgressFilterName = "orange-mcp-egress-match"

	defaultListenAddr = "127.0.0.1:10004"
	defaultEgressURL  = "http://127.0.0.1:10005"

	envListenAddr  = "ORANGE_MCP_LISTEN_ADDR"
	envEgressURL   = "ORANGE_MCP_EGRESS_URL"
	envSessionKeys = "ORANGE_MCP_SESSION_KEYS"

	headerRoute       = "x-orange-mcp-route"
	headerBackend     = "x-orange-mcp-backend"
	headerMethod      = "x-orange-mcp-method"
	headerRequestID   = "x-orange-mcp-request-id"
	headerTool        = "x-orange-mcp-tool"
	headerSession     = "x-orange-mcp-session"
	headerLastEventID = "x-orange-mcp-last-event-id"
)

func init() {
	handler := &handler{egressURL: resolveEgressURL()}
	sc := newSidecar(handler, sidecarOptions{
		listenAddr:      resolveListenAddr(),
		shutdownTimeout: 5 * time.Second,
		egressURL:       handler.egressURL,
	})

	g := up.NewGroup()
	g.Add(
		func() error { return sc.execute(FilterName) },
		sc.stop,
	)

	up.Register(FilterName, func(*up.Writer, *up.Request) {}, up.WithGroup(g))
	up.Register(EgressFilterName, egressHandler)
}

func resolveListenAddr() string {
	if v := os.Getenv(envListenAddr); v != "" {
		return v
	}
	return defaultListenAddr
}

func resolveEgressURL() string {
	if v := os.Getenv(envEgressURL); v != "" {
		return v
	}
	return defaultEgressURL
}

type handler struct {
	egressURL string
}

func (h *handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == "/ready" {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	writeJSON(w, http.StatusNotImplemented, map[string]any{
		"error": map[string]any{
			"type":    "not_implemented",
			"code":    "orange.mcp_not_implemented",
			"message": "orange-mcp sidecar registration skeleton is enabled; MCP protocol handlers land in the next slice",
		},
	})
}

func egressHandler(w *up.Writer, r *up.Request) {
	// PR 1 only registers the egress filter name. Header validation/stripping
	// and routing metadata are implemented in the egress slice.
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("content-type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
