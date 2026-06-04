// Package mcp hosts the Orange-managed MCP streamable-HTTP/SSE sidecar.
package mcp

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/dio/transit/examples/orange/internal/config"
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

	sessionIDHeader   = "mcp-session-id"
	lastEventIDHeader = "Last-Event-Id"
	headerSubject     = "x-orange-subject"
)

func init() {
	handler := newHandler(handlerOptions{
		egressURL: resolveEgressURL(),
		config:    config.Get,
		crypto:    resolveSessionCrypto(),
	})
	sc := newSidecar(handler, sidecarOptions{
		listenAddr:      resolveListenAddr(),
		shutdownTimeout: 5 * time.Second,
		egressURL:       handler.egressURL(),
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

func resolveSessionCrypto() sessionCrypto {
	keys := strings.Split(os.Getenv(envSessionKeys), ",")
	var primary string
	var fallbacks []string
	for _, key := range keys {
		key = strings.TrimSpace(key)
		if key == "" {
			continue
		}
		if primary == "" {
			primary = key
			continue
		}
		fallbacks = append(fallbacks, key)
	}
	if primary == "" {
		primary = "orange-mcp-dev-session-key"
		fmt.Fprintln(os.Stderr, "orange-mcp: WARNING: ORANGE_MCP_SESSION_KEYS unset; using development session key")
	}
	return newSessionCrypto(primary, fallbacks...)
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("content-type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
