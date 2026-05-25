// Package wsproxy demonstrates the embedded WebSocket proxy pattern using
// up.RegisterWithGroup, with a complete implementation for the OpenAI Responses
// API WebSocket mode at /v1/responses.
//
// # Protocol: OpenAI Responses API WebSocket mode (/v1/responses)
//
// The client sends one message to start a response:
//
//	{"type":"response.create","model":"gpt-4.1","input":[...],"max_output_tokens":N}
//
// The server streams events back:
//
//	{"type":"response.created", ...}
//	{"type":"response.output_item.delta", "delta":"Hi"}   // text chunks
//	{"type":"response.output_item.done",  ...}
//	{"type":"response.completed", "response":{"usage":{"input_tokens":N,"output_tokens":M}}}
//
// The SessionTap taps every frame:
//   - Client → Upstream: extracts model name from response.create
//   - Upstream → Client: extracts token usage from response.completed
//
// # Architecture
//
//	Client ──WS──► Envoy (port 10000, upgrade route)
//	                  │  ──► ws-proxy-local cluster (127.0.0.1:10001)
//	                  │              │
//	                  │      WSProxy.ServeHTTP
//	                  │              │  ──► wss://api.openai.com/v1/responses
//	                  │              │      Authorization: Bearer $OPENAI_API_KEY
//	                  │
//	Normal HTTP ──► upstream cluster (TLS + ws-auth upstream filter)
package wsproxy

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"time"

	"github.com/coder/websocket"

	"github.com/dio/transit/up"
)

const ExtensionName = "ws-proxy"

// Config is the JSON config for the ws-proxy filter.
type Config struct {
	// UpstreamURL is the upstream wss:// base URL.
	// Default: "wss://api.openai.com"
	UpstreamURL string `json:"upstream_url"`

	// AuthHeader is the HTTP header name injected when dialing upstream.
	// Default: "authorization"
	AuthHeader string `json:"auth_header"`

	// AuthValue is the header value. Supports ${ENV_VAR} expansion.
	// Default: "Bearer ${OPENAI_API_KEY}"
	AuthValue string `json:"auth_value"`

	// ShutdownTimeout for graceful shutdown. Default: "5s".
	ShutdownTimeout string `json:"shutdown_timeout"`

	// ListenAddress is the loopback address for the embedded proxy server.
	// Default: "127.0.0.1:10001". Must match the ws-proxy-local cluster in envoy.yaml.
	ListenAddress string `json:"listen_addr"`

	// OTELEndpoint enables actor-side OTLP/gRPC metrics export when set.
	// Requires github.com/dio/logging + github.com/tetratelabs/telemetry.
	// Example: "127.0.0.1:4317".
	OTELEndpoint string `json:"otel_endpoint"`

	// OTELExportInterval controls actor-side metric export cadence.
	// Default: "1s".
	OTELExportInterval string `json:"otel_export_interval"`
}

func parseConfig(raw []byte) Config {
	cfg := Config{
		UpstreamURL:   "wss://api.openai.com",
		AuthHeader:    "authorization",
		AuthValue:     "Bearer ${OPENAI_API_KEY}",
		ListenAddress: "127.0.0.1:10001",
	}
	if len(raw) > 0 {
		json.Unmarshal(raw, &cfg) //nolint:errcheck
	}
	return cfg
}

// WSProxy is an http.Handler that proxies WebSocket connections to an upstream
// provider, tapping frames for model extraction and token counting.
type WSProxy struct {
	upstreamURL string
	authHeader  string
	authValue   string
	log         *slog.Logger

	// OnClientFrame is called for each text frame the client sends.
	// Runs in the pump goroutine — must be fast and non-blocking.
	OnClientFrame func(websocket.MessageType, []byte)

	// OnUpstreamFrame is called for each text frame received from upstream.
	// Runs in the pump goroutine — must be fast and non-blocking.
	OnUpstreamFrame func(websocket.MessageType, []byte)
}

// NewProxy creates a WSProxy for direct use in tests or without the Envoy factory.
func NewProxy(upstreamURL, authHeader, authValue string) *WSProxy {
	return &WSProxy{
		upstreamURL: upstreamURL,
		authHeader:  authHeader,
		authValue:   authValue,
		log:         slog.Default(),
	}
}

// ServeHTTP accepts a WebSocket upgrade from Envoy, dials the upstream, and
// runs the bidirectional frame pump with per-session tapping.
func (p *WSProxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	log := slog.Default()
	if p.log != nil {
		log = p.log
	}

	clientConn, err := websocket.Accept(w, r, &websocket.AcceptOptions{
		InsecureSkipVerify: true, // Envoy handles downstream TLS
	})
	if err != nil {
		log.Error("ws-proxy: accept failed", "err", err)
		return
	}
	defer clientConn.CloseNow()

	upstreamHeader := http.Header{}
	if p.authHeader != "" && p.authValue != "" {
		upstreamHeader.Set(p.authHeader, p.authValue)
	}

	ctx := r.Context()
	upstreamURL := p.upstreamURL + r.URL.Path
	upstreamConn, _, err := websocket.Dial(ctx, upstreamURL, &websocket.DialOptions{
		HTTPHeader: upstreamHeader,
	})
	if err != nil {
		log.Error("ws-proxy: upstream dial failed", "url", upstreamURL, "err", err)
		clientConn.Close(websocket.StatusInternalError, "upstream unavailable")
		return
	}
	defer upstreamConn.CloseNow()

	tap := NewSessionTap()
	start := time.Now()
	log.Info("ws-proxy: session started", "path", r.URL.Path)

	errc := make(chan error, 2)

	// Client → Upstream
	go func() {
		for {
			msgType, data, err := clientConn.Read(ctx)
			if err != nil {
				errc <- fmt.Errorf("client read: %w", err)
				return
			}
			if msgType == websocket.MessageText {
				tap.FeedClient(data)
				if p.OnClientFrame != nil {
					p.OnClientFrame(msgType, data)
				}
			}
			if err := upstreamConn.Write(ctx, msgType, data); err != nil {
				errc <- fmt.Errorf("upstream write: %w", err)
				return
			}
		}
	}()

	// Upstream → Client
	go func() {
		for {
			msgType, data, err := upstreamConn.Read(ctx)
			if err != nil {
				errc <- fmt.Errorf("upstream read: %w", err)
				return
			}
			if msgType == websocket.MessageText {
				tap.FeedUpstream(data)
				if p.OnUpstreamFrame != nil {
					p.OnUpstreamFrame(msgType, data)
				}
			}
			if err := clientConn.Write(ctx, msgType, data); err != nil {
				errc <- fmt.Errorf("client write: %w", err)
				return
			}
		}
	}()

	firstErr := <-errc

	u := tap.Usage()
	recordActorSession(ctx, log, r.URL.Path, tap.Model(), u.InputTokens, u.OutputTokens, time.Since(start), firstErr)
}

// SessionTap extracts the model name and token usage from OpenAI Responses frames.
// Created fresh for each WebSocket session. Exported for unit testing.
type SessionTap struct {
	model  string
	input  uint32
	output uint32
}

// NewSessionTap creates a new SessionTap.
func NewSessionTap() *SessionTap { return &SessionTap{} }

// FeedClient taps the response.create frame (first client message) to extract
// the model name. All subsequent client frames are ignored once model is known.
func (t *SessionTap) FeedClient(data []byte) {
	if t.model != "" {
		return
	}
	if !bytes.Contains(data, []byte("response.create")) {
		return
	}
	var f struct {
		Type  string `json:"type"`
		Model string `json:"model"`
	}
	if json.Unmarshal(data, &f) == nil && f.Type == "response.create" && f.Model != "" {
		t.model = f.Model
	}
}

// FeedUpstream taps the response.completed frame (final server event) to extract
// token usage. All other upstream frames are ignored.
func (t *SessionTap) FeedUpstream(data []byte) {
	if !bytes.Contains(data, []byte("response.completed")) {
		return
	}
	var f struct {
		Type     string `json:"type"`
		Response struct {
			Usage struct {
				InputTokens  uint32 `json:"input_tokens"`
				OutputTokens uint32 `json:"output_tokens"`
			} `json:"usage"`
		} `json:"response"`
	}
	if json.Unmarshal(data, &f) == nil && f.Type == "response.completed" {
		t.input = f.Response.Usage.InputTokens
		t.output = f.Response.Usage.OutputTokens
	}
}

// Model returns the model name extracted from response.create, or "" if not yet seen.
func (t *SessionTap) Model() string { return t.model }

// TokenUsage holds the token counts from the completed response.
type TokenUsage struct {
	InputTokens  uint32
	OutputTokens uint32
}

// Usage returns the token usage extracted from response.completed.
func (t *SessionTap) Usage() TokenUsage {
	return TokenUsage{InputTokens: t.input, OutputTokens: t.output}
}

// ResolveEnv expands ${ENV_VAR} references in v using os.Expand.
// Returns v unchanged if the variable is unset.
func ResolveEnv(v string) string { return resolveEnv(v) }

func resolveEnv(v string) string {
	return os.Expand(v, func(key string) string {
		if val := os.Getenv(key); val != "" {
			return val
		}
		return "${" + key + "}" // leave unexpanded
	})
}

// Register wires the ws-proxy filter into Envoy via up.RegisterWithGroup.
// The filter itself is a no-op for regular HTTP — it only starts the embedded
// WebSocket proxy server via the Group goroutine.
// ws-auth (upstream filter) is registered separately in auth.go's init().
func Register() {
	cfg := parseConfig(nil) // defaults; runtime overrides via WSPROXY_* env vars for e2e

	// Allow e2e to override without recompiling.
	if v := os.Getenv("WSPROXY_LISTEN_ADDR"); v != "" {
		cfg.ListenAddress = v
	}
	if v := os.Getenv("WSPROXY_UPSTREAM_URL"); v != "" {
		cfg.UpstreamURL = v
	}
	if v := os.Getenv("WSPROXY_AUTH_VALUE"); v != "" {
		cfg.AuthValue = v
	}
	if v := os.Getenv("WSPROXY_SESSION_LOG"); v != "" {
		InitSessionLog(v)
	}

	proxy := &WSProxy{
		upstreamURL: cfg.UpstreamURL,
		authHeader:  cfg.AuthHeader,
		authValue:   resolveEnv(cfg.AuthValue),
		log:         slog.Default(),
	}

	g := up.NewGroup()
	g.Add(
		func() error {
			ln, err := net.Listen("tcp", cfg.ListenAddress)
			if err != nil {
				fmt.Fprintf(os.Stderr, "ws-proxy: listen %s: %v\n", cfg.ListenAddress, err)
				return err
			}
			fmt.Fprintf(os.Stderr, "ws-proxy: listening on %s\n", ln.Addr())
			srv := &http.Server{Handler: proxy}
			return srv.Serve(ln)
		},
		func() {}, // context cancel via g.Stop is sufficient; Serve returns on listener close
	)

	// No-op HTTP filter: presence in the filter chain starts the embedded server
	// (via the group) but does not alter normal HTTP requests.
	up.RegisterWithGroup(ExtensionName, g, func(w *up.Writer, r *up.Request) {})
}
