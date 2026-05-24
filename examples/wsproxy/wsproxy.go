// Package wsproxy implements an embedded WebSocket proxy for the OpenAI
// Responses API in WebSocket mode (wss://api.openai.com/v1/responses).
//
// Two transit filters are registered:
//
//   - "wsproxy-auth" — pre-upgrade HTTP auth gate (up.RegisterWithGroup).
//     Validates Authorization header, resolves credential from OPENAI_API_KEY,
//     sets x-wsproxy-cred on the loopback request. Rejects unauthenticated
//     requests with 401 before the WS upgrade happens.
//
//   - The Group goroutine starts an embedded net/http server on loopbackAddr.
//     Envoy routes WebSocket upgrades to a STATIC cluster pointing at that
//     address. The embedded server accepts the downstream WS from Envoy, dials
//     the upstream with the resolved credential, and runs a bidirectional frame
//     pump with selective JSON tapping for usage metering.
//
// Only "response.completed" frames are parsed. All other frames are forwarded
// without JSON decoding (bytes.Contains fast-path).
package wsproxy

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"github.com/coder/websocket"
	"github.com/dio/transit/up"
)

const (
	// FilterName is the Envoy filter name for the auth gate.
	FilterName = "wsproxy-auth"

	// DefaultLoopbackAddr is the address the embedded server listens on.
	// Must match the STATIC cluster in envoy.yaml.
	DefaultLoopbackAddr = "127.0.0.1:19002"

	// DefaultUpstreamURL is the OpenAI Responses API WebSocket endpoint.
	DefaultUpstreamURL = "wss://api.openai.com/v1/responses"

	// DefaultMaxDuration enforces OpenAI's 60-minute session limit.
	DefaultMaxDuration = 60 * time.Minute
)

// Metric IDs — reserved for future use with an access logger or Writer-scoped increment.
var ()

// proxy is the shared state for the embedded WS proxy server.
// Created once at filter config time; accessed from the Group goroutine and
// the auth filter handler (different goroutines, but only the handler reads
// the loopbackAddr/upstreamURL fields after init).
type proxy struct {
	loopbackAddr string
	upstreamURL  string
	maxDuration  time.Duration

	srv    *http.Server
	active sync.Map // sessionID(string) -> *session

	sessionsTotal int64 // atomic, for session IDs
}

func newProxy() *proxy {
	addr := DefaultLoopbackAddr
	if v := os.Getenv("WSPROXY_LOOPBACK_ADDR"); v != "" {
		addr = v
	}
	upstream := DefaultUpstreamURL
	if v := os.Getenv("WSPROXY_UPSTREAM_URL"); v != "" {
		upstream = v
	}
	p := &proxy{
		loopbackAddr: addr,
		upstreamURL:  upstream,
		maxDuration:  DefaultMaxDuration,
	}
	p.srv = &http.Server{
		Addr:    p.loopbackAddr,
		Handler: p,
	}
	return p
}

// serve starts the embedded HTTP server. Blocks until the server stops.
// Called from the Group goroutine.
func (p *proxy) serve(ctx context.Context) {
	ln, err := net.Listen("tcp", p.loopbackAddr)
	if err != nil {
		fmt.Fprintf(os.Stderr, "wsproxy: listen %s: %v\n", p.loopbackAddr, err)
		return
	}
	fmt.Fprintf(os.Stderr, "wsproxy: listening on %s\n", p.loopbackAddr)
	go func() {
		<-ctx.Done()
		// Close all active sessions gracefully.
		p.active.Range(func(_, v any) bool {
			v.(*session).cancel()
			return true
		})
		p.srv.Close()
	}()
	if err := p.srv.Serve(ln); err != nil && err != http.ErrServerClosed {
		fmt.Fprintf(os.Stderr, "wsproxy: server error: %v\n", err)
	}
}

// ServeHTTP handles each incoming loopback connection from Envoy.
func (p *proxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// Gate 3 validation: reject non-WS requests before websocket.Accept.
	if r.Header.Get("Upgrade") != "websocket" {
		http.Error(w, `{"error":"WebSocket upgrade required"}`, http.StatusBadRequest)
		return
	}

	cred := r.Header.Get("x-wsproxy-cred")
	if cred == "" {
		http.Error(w, `{"error":"missing credential"}`, http.StatusUnauthorized)
		return
	}

	downstream, err := websocket.Accept(w, r, &websocket.AcceptOptions{
		InsecureSkipVerify: true, // Envoy handles downstream TLS
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "wsproxy: accept: %v\n", err)
		return
	}

	id := fmt.Sprintf("ws-%d", atomic.AddInt64(&p.sessionsTotal, 1))
	ctx, cancel := context.WithTimeout(context.Background(), p.maxDuration)
	sess := &session{id: id, cancel: cancel}
	p.active.Store(id, sess)
	defer func() {
		p.active.Delete(id)
		cancel()
	}()

	// Dial upstream with resolved credential.
	upstream, _, err := websocket.Dial(ctx, p.upstreamURL, &websocket.DialOptions{
		HTTPHeader: http.Header{
			"Authorization": {"Bearer " + cred},
		},
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "wsproxy: dial upstream: %v\n", err)
		downstream.Close(websocket.StatusInternalError, "upstream unavailable")
		return
	}

	start := time.Now()
	tap := &sessionTap{}

	errCh := make(chan error, 2)
	go pump(ctx, downstream, upstream, tap.feedDownstream, errCh)
	go pump(ctx, upstream, downstream, tap.feedUpstream, errCh)

	// Wait for first pump to exit, then cancel so the other pump stops.
	<-errCh
	cancel()
	<-errCh // drain to avoid goroutine leak

	dur := time.Since(start)
	timedOut := ctx.Err() == context.DeadlineExceeded
	in, out, turns := tap.Counts()
	fmt.Fprintf(os.Stderr, "wsproxy: session %s done dur=%s timeout=%v input=%d output=%d turns=%d\n",
		id, dur.Round(time.Millisecond), timedOut, in, out, turns)

	downstream.Close(websocket.StatusNormalClosure, "")
	upstream.Close(websocket.StatusNormalClosure, "")
}

// pump reads frames from src, calls tapFn, writes to dst.
// Sends the first error (or nil on clean close) to errCh.
func pump(ctx context.Context, src, dst *websocket.Conn, tapFn func([]byte) []byte, errCh chan<- error) {
	for {
		typ, data, err := src.Read(ctx)
		if err != nil {
			errCh <- err
			return
		}
		data = tapFn(data)
		if err := dst.Write(ctx, typ, data); err != nil {
			errCh <- err
			return
		}
	}
}

// session holds per-connection cancellation.
type session struct {
	id     string
	cancel context.CancelFunc
}

// sessionTap extracts token usage from response.completed frames.
// feedUpstream and feedDownstream are each called from exactly one goroutine;
// no mutex needed since they touch separate fields.
type sessionTap struct {
	inputTokens  int64
	outputTokens int64
	turns        int64
}

// SessionTap is the exported view of sessionTap for unit tests.
type SessionTap = sessionTap

// NewSessionTap returns a new SessionTap for use in tests.
func NewSessionTap() *SessionTap { return &sessionTap{} }

// Counts returns the accumulated token counts.
func (t *sessionTap) Counts() (input, output, turns int64) {
	return t.inputTokens, t.outputTokens, t.turns
}

// FeedUpstream is exported for testing.
func (t *sessionTap) FeedUpstream(frame []byte) []byte { return t.feedUpstream(frame) }

// FeedDownstream is exported for testing.
func (t *sessionTap) FeedDownstream(frame []byte) []byte { return t.feedDownstream(frame) }

var responseCompletedTag = []byte(`"response.completed"`)

func (t *sessionTap) feedUpstream(frame []byte) []byte {
	if !bytes.Contains(frame, responseCompletedTag) {
		return frame
	}
	var ev struct {
		Type     string `json:"type"`
		Response struct {
			Usage struct {
				InputTokens  int64 `json:"input_tokens"`
				OutputTokens int64 `json:"output_tokens"`
			} `json:"usage"`
		} `json:"response"`
	}
	if err := json.Unmarshal(frame, &ev); err != nil || ev.Type != "response.completed" {
		return frame
	}
	t.inputTokens += ev.Response.Usage.InputTokens
	t.outputTokens += ev.Response.Usage.OutputTokens
	t.turns++
	return frame
}

func (t *sessionTap) feedDownstream(frame []byte) []byte { return frame }

// Register wires both filters into Envoy. Call from init().
func Register() {
	p := newProxy()
	g := up.NewGroup()
	g.AddGoroutine(p.serve)

	// Auth gate: validates the request and injects x-wsproxy-cred before
	// Envoy routes the WS upgrade to the loopback cluster.
	up.RegisterWithGroup(FilterName, g, func(w *up.Writer, r *up.Request) {
		if r.Header("authorization") == "" {
			w.SendLocalResponse(401, []byte(`{"error":"missing authorization"}`))
			return
		}
		cred := os.Getenv("OPENAI_API_KEY")
		if cred == "" {
			w.SendLocalResponse(503, []byte(`{"error":"OPENAI_API_KEY not set"}`))
			return
		}
		w.SetRequestHeader("x-wsproxy-cred", cred)
	})
}
