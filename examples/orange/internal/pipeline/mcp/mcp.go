// Package mcp hosts the Orange-managed MCP streamable-HTTP/SSE sidecar.
package mcp

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/dio/transit/examples/orange/internal/config"
	"github.com/dio/transit/examples/orange/internal/observability"
	"github.com/dio/transit/up"
)

// NewSidecar constructs the MCP handler and sidecar. listenAddr overrides
// ORANGE_MCP_LISTEN_ADDR and the compiled-in default; pass "" to use the env
// var / default. Supports TCP ("127.0.0.1:0") and Unix sockets
// ("unix:///tmp/orange-mcp.sock"). The sidecar is not yet bound; call Listen
// then Serve to start it.
func NewSidecar(listenAddr string) (*Sidecar, error) {
	if listenAddr == "" {
		listenAddr = resolveListenAddr()
	}
	handler := newHandler(handlerOptions{
		egressURL: resolveEgressURL(),
		crypto:    resolveSessionCrypto(),
	})
	sc := newSidecar(handler, sidecarOptions{
		listenAddr:      listenAddr,
		shutdownTimeout: 5 * time.Second,
		egressURL:       handler.egressURL(),
	})
	return sc, nil
}

const (
	EgressFilterName = "orange-mcp-egress-match"

	defaultListenAddr = "127.0.0.1:0"
	defaultEgressURL  = "http://127.0.0.1:10005"
	defaultSessionKey = "orange-mcp-dev-session-key"
	generatedKeySpec  = "orange-generated"
	generatedKeyBytes = 32

	envListenAddr  = "ORANGE_MCP_LISTEN_ADDR"
	envEgressURL   = "ORANGE_MCP_EGRESS_URL"
	envSessionKeys = "ORANGE_MCP_SESSION_KEYS"

	headerRoute         = "x-orange-mcp-route"
	headerBackend       = "x-orange-mcp-backend"
	headerMethod        = "x-orange-mcp-method"
	headerRequestID     = "x-orange-mcp-request-id"
	headerTool          = "x-orange-mcp-tool"
	headerSession       = "x-orange-mcp-session"
	headerLastEventID   = "x-orange-mcp-last-event-id"
	headerBackendStatus = "x-orange-mcp-backend-status"

	sessionIDHeader   = "mcp-session-id"
	lastEventIDHeader = "Last-Event-Id"
	headerSubject     = "x-orange-subject"
)

var log = observability.Logger("orange/mcp")

// mcpAppState and mcpSecResolver are the new-system config source for the MCP
// egress handler. When non-nil, egressHandler uses them instead of config.Get().
var (
	mcpAppState    *config.AppState
	mcpSecResolver config.SecretResolver
)

// SetAppState configures the new-system AppState and SecretResolver for the
// MCP egress filter. Call before Envoy initialises the filter.
func SetAppState(s *config.AppState, r config.SecretResolver) {
	mcpAppState = s
	mcpSecResolver = r
}

func init() {
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
	primary, fallbacks := resolveSessionKeySpecs(os.Getenv(envSessionKeys))
	return newSessionCrypto(primary, fallbacks...)
}

func resolveSessionKeySpecs(raw string) (string, []string) {
	keys := strings.Split(raw, ",")
	var primary string
	fallbacks := make([]string, 0, len(keys))
	for _, key := range keys {
		key = strings.TrimSpace(key)
		if key == "" {
			continue
		}
		key = expandSessionKeySpec(key)
		if primary == "" {
			primary = key
			continue
		}
		fallbacks = append(fallbacks, key)
	}
	if primary == "" {
		primary = defaultSessionKey
		log.Warn("ORANGE_MCP_SESSION_KEYS unset; using development session key")
	}
	return primary, fallbacks
}

func expandSessionKeySpec(spec string) string {
	if spec != generatedKeySpec {
		return spec
	}
	key, err := generateSessionKey()
	if err != nil {
		log.Error("failed to generate Orange MCP session key", "err", err)
		panic("orange-mcp: failed to generate session key: " + err.Error())
	}
	log.Warn("using generated ephemeral Orange MCP session key; sessions will be invalid after restart")
	return key
}

func generateSessionKey() (string, error) {
	key := make([]byte, generatedKeyBytes)
	if _, err := rand.Read(key); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(key), nil
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("content-type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
