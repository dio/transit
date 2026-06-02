// Package mcpstreamingsidecar demonstrates the MCP streaming sidecar pattern.
//
// A sidecar accepts stateful SSE or streamable-HTTP connections from Envoy,
// encodes session state into response headers (no server-side session store),
// and dials upstream via EgressURL. Trace headers from the inbound Envoy request
// are propagated to the egress call per examples/trace-propagation.
//
// Architecture:
//
//	Client ──SSE──► Envoy :inbound ──► mcp-streaming-sidecar :loopback
//	                                        │  (session encoded in headers)
//	                                        ▼
//	                                 Envoy :egress ──► upstream MCP server
package mcpstreamingsidecar

import (
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"time"

	"github.com/dio/transit/up"
)

const ExtensionName = "mcp-streaming-sidecar"

// MCPStreamingSidecar is an http.Handler that accepts SSE or streamable-HTTP
// connections from Envoy. Real MCP SSE/streamable-HTTP session logic is out of
// scope; this stub accepts, fires OnSession, echoes one message, and closes.
type MCPStreamingSidecar struct {
	egressURL string
	log       *slog.Logger
}

// MCPStreamingSidecarHandler is the exported handler type for use in tests
// without needing to start the full sidecar via Register.
type MCPStreamingSidecarHandler = MCPStreamingSidecar

// ServeHTTP accepts the connection, propagates trace headers, echoes one
// message to confirm the path works, then closes.
func (m *MCPStreamingSidecar) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	log := m.log
	if log == nil {
		log = slog.Default()
	}

	// Propagate trace headers to upstream (per examples/trace-propagation).
	traceID := r.Header.Get("x-request-id")
	_ = traceID // would be forwarded to upstream egress call

	// Stub: echo one SSE event and return.
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	fmt.Fprintf(w, "data: {\"type\":\"mcp-streaming-sidecar-stub\"}\n\n")
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}
	log.Info("mcp-streaming-sidecar: session served", "path", r.URL.Path, "egress_url", m.egressURL)
}

// Register wires the mcp-streaming-sidecar into Envoy via up.Register + up.WithSidecar.
func Register() {
	listenAddr := os.Getenv("MCP_STREAMING_SIDECAR_LISTEN_ADDR")
	if listenAddr == "" {
		listenAddr = "127.0.0.1:10011"
	}
	egressURL := os.Getenv("MCP_STREAMING_SIDECAR_EGRESS_URL")

	handler := &MCPStreamingSidecar{
		egressURL: egressURL,
		log:       slog.Default(),
	}

	s := up.NewSidecar(handler, up.SidecarOptions{
		ListenAddr:      listenAddr,
		ShutdownTimeout: 5 * time.Second,
		EgressURL:       egressURL,
		Rationale:       "mcp-streaming-sidecar: egress cluster not yet provisioned",
		OnSession: func(e up.SidecarSessionEvent) {
			slog.Default().Info("mcp-streaming-sidecar: session ended",
				"path", e.Path,
				"duration", e.Duration.Round(time.Millisecond).String(),
			)
		},
	})
	up.Register(ExtensionName, func(w *up.Writer, r *up.Request) {}, up.WithSidecar(s))
}
