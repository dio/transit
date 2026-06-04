package ws

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/coder/websocket"

	"github.com/dio/transit/examples/orange/internal/config"
	"github.com/dio/transit/up"
)

const (
	FilterName = "orange-ws"

	// maxSessionDuration is the OpenAI-documented 60-minute connection limit.
	maxSessionDuration = 60 * time.Minute

	// firstFrameTimeout bounds the wait for the initial response.create frame.
	firstFrameTimeout = 30 * time.Second

	// Internal header names written by the sidecar on the egress upgrade.
	// Consumed and stripped by orange-ws-egress-match.
	headerProvider     = "x-orange-ws-provider"
	headerKind         = "x-orange-ws-kind"
	headerModel        = "x-orange-ws-model"
	headerBackendModel = "x-orange-ws-backend-model"

	// Trace context headers forwarded from the inbound upgrade to the egress upgrade.
	headerTraceParent = "traceparent"
	headerTraceState  = "tracestate"
	headerRequestID   = "x-request-id"

	// closeReasonNormal is used when both sides close cleanly.
	closeReasonNormal    = "normal"
	closeReasonUpstream  = "upstream_close"
	closeReasonParseErr  = "parse_error"
	closeReasonLookupErr = "lookup_error"
	closeReasonDeadline  = "deadline"
)

func init() {
	handler := &orangeWSHandler{
		egressURL: resolveEgressURL(),
		log:       slog.Default(),
	}

	sc, err := newWSSidecar(handler, wsSidecarOptions{
		listenAddr:      resolveListenAddr(),
		shutdownTimeout: 5 * time.Second,
		egressURL:       handler.egressURL,
	})
	if err != nil {
		// If we fail to create the sidecar struct, log and bail. The filter still
		// registers so Envoy does not crash; sessions will fail at accept time.
		fmt.Fprintf(os.Stderr, "orange-ws: sidecar init error: %v\n", err)
	}

	g := up.NewGroup()
	name := FilterName
	g.Add(
		func() error { return sc.execute(name) },
		sc.stop,
	)
	up.Register(FilterName, func(*up.Writer, *up.Request) {}, up.WithGroup(g))
}

func resolveListenAddr() string {
	if v := os.Getenv("ORANGE_WS_LISTEN_ADDR"); v != "" {
		return v
	}
	return "127.0.0.1:10002"
}

func resolveEgressURL() string {
	if v := os.Getenv("ORANGE_WS_EGRESS_URL"); v != "" {
		return v
	}
	return "ws://127.0.0.1:10003"
}

// listenForSidecar opens a TCP or Unix domain socket listener based on the
// addr prefix:
//   - "unix:///path" or "unix://path" → net.Listen("unix", path)
//   - anything else → net.Listen("tcp", addr)
func listenForSidecar(addr string) (net.Listener, error) {
	if strings.HasPrefix(addr, "unix://") {
		path := strings.TrimPrefix(addr, "unix://")
		return net.Listen("unix", path)
	}
	return net.Listen("tcp", addr)
}

// dialOptionsForEgress returns the URL and DialOptions to use when dialing the
// Envoy egress listener. For ws+unix:// URLs the DialOptions carry a custom
// http.Client that routes over the Unix socket; the returned URL is a plain
// ws://localhost path that the underlying http.Client ignores for dialing.
func dialOptionsForEgress(egressURL string) (wsURL string, opts *websocket.DialOptions) {
	if strings.HasPrefix(egressURL, "ws+unix://") {
		socketPath := strings.TrimPrefix(egressURL, "ws+unix://")
		t := &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				return (&net.Dialer{}).DialContext(ctx, "unix", socketPath)
			},
		}
		return "ws://localhost", &websocket.DialOptions{
			HTTPClient: &http.Client{Transport: t},
		}
	}
	// TCP case: pass the egressURL directly; DialOptions uses the default transport.
	return egressURL, &websocket.DialOptions{}
}

// wsSidecarOptions configures a wsSidecar.
type wsSidecarOptions struct {
	listenAddr      string
	shutdownTimeout time.Duration
	egressURL       string // non-empty means egress-via-Envoy (required for orange-ws)
}

// wsSidecar manages the embedded HTTP server lifecycle for orange-ws.
// It mirrors up.Sidecar but calls listenForSidecar so that UDS is supported.
type wsSidecar struct {
	handler  http.Handler
	opts     wsSidecarOptions
	ready    chan struct{} // closed after net.Listen, before Serve
	started  chan struct{} // closed when execute sets srv+ln, or returns with error
	mu       sync.Mutex
	srv      *http.Server
	ln       net.Listener
	stopOnce sync.Once
	resolved string
}

func newWSSidecar(h http.Handler, opts wsSidecarOptions) (*wsSidecar, error) {
	if opts.shutdownTimeout == 0 {
		opts.shutdownTimeout = 5 * time.Second
	}
	return &wsSidecar{
		handler: h,
		opts:    opts,
		ready:   make(chan struct{}),
		started: make(chan struct{}),
	}, nil
}

// Ready returns a channel closed after net.Listen succeeds. ListenAddr() is
// valid after Ready() closes.
func (s *wsSidecar) Ready() <-chan struct{} { return s.ready }

// ListenAddr returns the resolved bind address. Empty before Ready() closes.
func (s *wsSidecar) ListenAddr() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.resolved
}

func (s *wsSidecar) execute(name string) error {
	ln, err := listenForSidecar(s.opts.listenAddr)
	if err != nil {
		fmt.Fprintf(os.Stderr, "orange-ws: listen %s: %v\n", s.opts.listenAddr, err)
		close(s.started)
		return err
	}

	s.mu.Lock()
	s.ln = ln
	s.resolved = ln.Addr().String()
	s.srv = &http.Server{Handler: s.handler}
	s.mu.Unlock()

	close(s.ready)
	close(s.started)

	if s.opts.egressURL == "" {
		fmt.Fprintf(os.Stderr, "orange-ws: WARNING: no egress URL set; this sidecar will dial providers directly (not allowed in orange v1)\n")
	}
	fmt.Fprintf(os.Stderr, "orange-ws: sidecar %s listening on %s\n", name, s.resolved)

	return s.srv.Serve(ln)
}

func (s *wsSidecar) stop() {
	s.stopOnce.Do(func() {
		<-s.started
		s.mu.Lock()
		srv := s.srv
		ln := s.ln
		s.mu.Unlock()
		if srv != nil {
			ctx, cancel := context.WithTimeout(context.Background(), s.opts.shutdownTimeout)
			defer cancel()
			srv.Shutdown(ctx) //nolint:errcheck
		}
		if ln != nil {
			ln.Close() //nolint:errcheck
		}
	})
}

// orangeWSHandler is the http.Handler for per-WebSocket-session logic.
type orangeWSHandler struct {
	egressURL string
	log       *slog.Logger
	onTurn    func(TurnRecord)    // optional; called from upstream pump goroutine per completed turn
	onSummary func(SessionSummary) // optional; called once at session end
}

func (h *orangeWSHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	log := h.log
	start := time.Now()

	sessionID := newSessionID()
	traceID := r.Header.Get(headerTraceParent)
	requestID := r.Header.Get(headerRequestID)

	clientConn, err := websocket.Accept(w, r, &websocket.AcceptOptions{
		InsecureSkipVerify: true, // Envoy owns downstream TLS
	})
	if err != nil {
		log.Error("orange-ws: accept failed", "err", err)
		return
	}

	var (
		model        string
		providerName string
		providerKind string
		backendModel string
		closeReason  = closeReasonParseErr
		errClass     string
	)

	defer func() {
		if err := clientConn.CloseNow(); err != nil {
			log.Error("orange-ws: close client connection", "err", err)
		}
		log.Info("orange-ws: session ended",
			"session_id", sessionID,
			"provider", providerName,
			"kind", providerKind,
			"model", model,
			"backend_model", backendModel,
			"close_reason", closeReason,
			"duration_ms", time.Since(start).Milliseconds(),
			"err_class", errClass,
		)
	}()

	// Step 1: read first frame with timeout.
	firstCtx, firstCancel := context.WithTimeout(r.Context(), firstFrameTimeout)
	defer firstCancel()
	msgType, firstFrame, err := clientConn.Read(firstCtx)
	if err != nil || msgType != websocket.MessageText {
		errClass = "first_frame_read"
		clientConn.Close(websocket.StatusPolicyViolation, "expected text frame") //nolint:errcheck
		return
	}

	// Step 2: parse response.create and extract model.
	model, err = parseResponseCreate(firstFrame)
	if err != nil {
		errClass = "first_frame_parse"
		log.Warn("orange-ws: first frame rejected", "err", err)
		clientConn.Close(websocket.StatusPolicyViolation, err.Error()) //nolint:errcheck
		return
	}

	// Step 3: resolve provider from active orange.yaml snapshot.
	cfg := config.Get()
	var provider config.Provider
	var ok bool
	providerName, provider, ok = cfg.LookupModelProvider(model)
	if !ok {
		closeReason = closeReasonLookupErr
		errClass = "unknown_model"
		log.Warn("orange-ws: unknown model", "model", model)
		clientConn.Close(websocket.StatusPolicyViolation, "unknown model") //nolint:errcheck
		return
	}
	_, backendModel = cfg.LookupModel(model)
	providerKind = provider.Kind

	// Step 4: set a 60-minute session deadline.
	sessionCtx, sessionCancel := context.WithTimeout(r.Context(), maxSessionDuration)
	defer sessionCancel()

	// Step 5: build egress upgrade headers with internal orange-ws-* values.
	// Overwrite unconditionally — never preserve client-supplied values.
	egressHeader := http.Header{}
	egressHeader.Set(headerProvider, providerName)
	egressHeader.Set(headerKind, provider.Kind)
	egressHeader.Set(headerModel, model)
	egressHeader.Set(headerBackendModel, backendModel)
	// Forward trace context from the inbound request.
	for _, h := range []string{headerTraceParent, headerTraceState, headerRequestID} {
		if v := r.Header.Get(h); v != "" {
			egressHeader.Set(h, v)
		}
	}

	// Step 6: dial Envoy egress listener.
	baseURL, dialOpts := dialOptionsForEgress(h.egressURL)
	dialOpts.HTTPHeader = egressHeader
	egressURL := baseURL + r.URL.Path
	egressConn, _, err := websocket.Dial(sessionCtx, egressURL, dialOpts)
	if err != nil {
		closeReason = closeReasonParseErr
		errClass = "egress_dial"
		log.Error("orange-ws: egress dial failed", "url", egressURL, "err", err)
		clientConn.Close(websocket.StatusInternalError, "upstream unavailable") //nolint:errcheck
		return
	}
	defer func() {
		if err := egressConn.CloseNow(); err != nil {
			log.Error("orange-ws: close egress connection", "err", err)
		}
	}()

	// Single shared FrameTap — client goroutine calls FeedClient, upstream goroutine
	// calls FeedUpstream; mutex protects inFlight at turn boundaries.
	tap := &FrameTap{
		sessionID: sessionID,
		traceID:   traceID,
		requestID: requestID,
		onTurn:    h.onTurn,
	}

	// Step 7: forward the first frame, then run bidirectional pump.
	tap.FeedClient(firstFrame)
	if err := egressConn.Write(sessionCtx, websocket.MessageText, firstFrame); err != nil {
		closeReason = closeReasonUpstream
		errClass = "first_frame_forward"
		return
	}

	errc := make(chan error, 2)

	// Client → upstream
	go func() {
		for {
			mt, data, err := clientConn.Read(sessionCtx)
			if err != nil {
				errc <- fmt.Errorf("client read: %w", err)
				return
			}
			if mt == websocket.MessageText {
				tap.FeedClient(data)
			}
			if err := egressConn.Write(sessionCtx, mt, data); err != nil {
				errc <- fmt.Errorf("egress write: %w", err)
				return
			}
		}
	}()

	// Upstream → client
	go func() {
		for {
			mt, data, err := egressConn.Read(sessionCtx)
			if err != nil {
				errc <- fmt.Errorf("upstream read: %w", err)
				return
			}
			if mt == websocket.MessageText {
				tap.FeedUpstream(data)
			}
			if err := clientConn.Write(sessionCtx, mt, data); err != nil {
				errc <- fmt.Errorf("client write: %w", err)
				return
			}
		}
	}()

	// Step 8: wait for first pump error, classify close reason.
	firstErr := <-errc
	closeReason = classifyCloseReason(sessionCtx, firstErr)
	if closeReason != closeReasonNormal {
		errClass = firstErr.Error()
	}

	// Step 9: flush any in-flight turn and build the session summary.
	flushOutcome := closeReasonToTurnOutcome(closeReason)
	tap.FlushInFlight(flushOutcome)

	summary := tap.Summary(
		sessionID, traceID, requestID,
		model, providerName, providerKind, backendModel,
		closeReason, time.Since(start), errClass,
	)
	if h.onSummary != nil {
		h.onSummary(summary)
	}
}

// closeReasonToTurnOutcome maps a session close reason to a TurnOutcome for
// any turn that was in-flight when the session ended.
func closeReasonToTurnOutcome(reason string) TurnOutcome {
	switch reason {
	case closeReasonDeadline:
		return TurnOutcomeDeadline
	case closeReasonUpstream:
		return TurnOutcomeProviderError
	default:
		return TurnOutcomeClientDisconnect
	}
}

// parseResponseCreate parses the first client frame and returns the model field.
// Returns an error if the frame is not valid JSON, not a response.create event,
// or is missing the model field.
func parseResponseCreate(data []byte) (model string, err error) {
	if !bytes.Contains(data, []byte(`"response.create"`)) {
		return "", fmt.Errorf("expected response.create frame")
	}
	var f struct {
		Type  string `json:"type"`
		Model string `json:"model"`
	}
	if err := json.Unmarshal(data, &f); err != nil {
		return "", fmt.Errorf("invalid JSON: %w", err)
	}
	if f.Type != "response.create" {
		return "", fmt.Errorf("expected type response.create, got %q", f.Type)
	}
	if f.Model == "" {
		return "", fmt.Errorf("response.create missing model field")
	}
	return f.Model, nil
}

// classifyCloseReason maps a pump error to a human-readable close reason.
func classifyCloseReason(ctx context.Context, err error) string {
	if err == nil {
		return closeReasonNormal
	}
	if ctx != nil && ctx.Err() != nil {
		return closeReasonDeadline
	}
	msg := err.Error()
	if strings.HasPrefix(msg, "upstream read:") || strings.HasPrefix(msg, "upstream write:") {
		return closeReasonUpstream
	}
	return closeReasonNormal
}

func newSessionID() string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		// Fallback: use nanosecond timestamp as hex — not cryptographically random
		// but always available and still unique within a process.
		return fmt.Sprintf("%016x", time.Now().UnixNano())
	}
	return hex.EncodeToString(b[:])
}
