package responsesws

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
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/coder/websocket"

	"github.com/dio/transit/examples/orange/internal/config"
	"github.com/dio/transit/examples/orange/internal/observability"
	"github.com/dio/transit/examples/orange/internal/pipeline/match"
	"github.com/dio/transit/up"
)

const (
	MeterFilterName = "orange-responsesws-meter"

	// maxSessionDuration is the OpenAI-documented 60-minute connection limit.
	maxSessionDuration = 60 * time.Minute

	// defaultFirstFrameTimeout bounds the wait for the initial response.create
	// frame. Codex may prewarm the WebSocket before the user submits a prompt, so
	// this must tolerate interactive idle time.
	defaultFirstFrameTimeout = 10 * time.Minute

	// defaultReadLimit bounds a single WebSocket message. Codex response.create
	// frames include tools and skill context, so they are routinely larger than
	// coder/websocket's 32 KiB default.
	defaultReadLimit = 32 << 20

	// Internal header names written by the sidecar on the egress upgrade.
	// Consumed and stripped by orange-responsesws-egress-match.
	headerProvider     = "x-orange-responsesws-provider"
	headerKind         = "x-orange-responsesws-kind"
	headerModel        = "x-orange-responsesws-model"
	headerBackendModel = "x-orange-responsesws-backend-model"

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

	localWarmupResponseIDPrefix = "resp_orange_responsesws_warmup_"

	responseswsMeterWaitTimeout = 250 * time.Millisecond
)

func init() {
	up.Register(MeterFilterName, requestHandler, up.WithResponse(responseHandler))
}

func resolveListenAddr() string {
	if v := os.Getenv("ORANGE_RESPONSESWS_LISTEN_ADDR"); v != "" {
		return v
	}
	return "127.0.0.1:0"
}

func resolveEgressURL() string {
	if v := os.Getenv("ORANGE_RESPONSESWS_EGRESS_URL"); v != "" {
		return v
	}
	return "ws://127.0.0.1:10003"
}

func resolveFirstFrameTimeout() time.Duration {
	if v := os.Getenv("ORANGE_RESPONSESWS_FIRST_FRAME_TIMEOUT"); v != "" {
		d, err := time.ParseDuration(v)
		if err == nil && d > 0 {
			return d
		}
		observability.Logger("orange/responsesws").Warn("invalid ORANGE_RESPONSESWS_FIRST_FRAME_TIMEOUT; using default",
			"value", v,
			"default", defaultFirstFrameTimeout,
		)
	}
	return defaultFirstFrameTimeout
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

// sidecarOptions configures a Sidecar.
type sidecarOptions struct {
	listenAddr      string
	shutdownTimeout time.Duration
	egressURL       string // non-empty means egress-via-Envoy (required for orange-responsesws)
	log             *slog.Logger
}

// Sidecar manages the embedded HTTP server lifecycle for orange-responsesws.
// Create with NewSidecar or newSidecar, then call Listen to bind the socket,
// Serve to start accepting connections, and Stop to shut down gracefully.
type Sidecar struct {
	handler        http.Handler
	opts           sidecarOptions
	ready          chan struct{} // closed after net.Listen succeeds
	started        chan struct{} // closed after Listen (success or failure)
	mu             sync.Mutex
	srv            *http.Server
	ln             net.Listener
	resolved       string
	unixSocketPath string // non-empty when ln is a Unix socket; removed in Stop
	stopOnce       sync.Once
}

func newSidecar(h http.Handler, opts sidecarOptions) (*Sidecar, error) {
	if opts.shutdownTimeout == 0 {
		opts.shutdownTimeout = 5 * time.Second
	}
	return &Sidecar{
		handler: h,
		opts:    opts,
		ready:   make(chan struct{}),
		started: make(chan struct{}),
	}, nil
}

// NewSidecar constructs the responsesws handler and sidecar. listenAddr
// overrides ORANGE_RESPONSESWS_LISTEN_ADDR and the compiled-in default;
// pass "" to use the env var / default. Supports TCP ("127.0.0.1:0") and
// Unix sockets ("unix:///tmp/orange-responsesws.sock"). The sidecar is not
// yet bound; call Listen then Serve to start it.
func NewSidecar(listenAddr string) (*Sidecar, error) {
	if listenAddr == "" {
		listenAddr = resolveListenAddr()
	}
	log := observability.Logger("orange/responsesws")
	handler := &responseswsHandler{
		egressURL:         resolveEgressURL(),
		firstFrameTimeout: resolveFirstFrameTimeout(),
		log:               log,
		onSummary:         publishSummary,
	}
	return newSidecar(handler, sidecarOptions{
		listenAddr:      listenAddr,
		shutdownTimeout: 5 * time.Second,
		egressURL:       handler.egressURL,
		log:             log,
	})
}

// Ready returns a channel closed after net.Listen succeeds. ListenAddr() is
// valid after Ready() closes.
func (s *Sidecar) Ready() <-chan struct{} { return s.ready }

// ListenAddr returns the resolved bind address. Empty before Ready() closes.
func (s *Sidecar) ListenAddr() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.resolved
}

// Listen binds the listener synchronously. Supports TCP (default) and Unix
// domain sockets (addr prefixed with "unix://"). On success Ready() is closed
// and ListenAddr() returns the actual bound address.
func (s *Sidecar) Listen() error {
	ln, err := listenForSidecar(s.opts.listenAddr)
	if err != nil {
		close(s.started)
		return err
	}

	var unixPath string
	if ln.Addr().Network() == "unix" {
		unixPath = ln.Addr().String()
	}

	s.mu.Lock()
	s.ln = ln
	s.resolved = ln.Addr().String()
	s.srv = &http.Server{Handler: s.handler}
	s.unixSocketPath = unixPath
	s.mu.Unlock()

	close(s.ready)
	close(s.started)
	return nil
}

// Serve accepts connections on the already-bound listener. Must be called after
// a successful Listen. Returns http.ErrServerClosed when Stop is called.
func (s *Sidecar) Serve() error {
	log := s.opts.log
	if log == nil {
		log = observability.Logger("orange/responsesws")
	}
	log.Info("orange-responsesws sidecar listening", "addr", s.resolved)
	if s.opts.egressURL == "" {
		log.Warn("orange-responsesws egress URL missing; sidecar will dial providers directly")
	}
	s.mu.Lock()
	ln := s.ln
	srv := s.srv
	s.mu.Unlock()
	return srv.Serve(ln)
}

// Stop shuts down the sidecar gracefully and removes the Unix socket file if
// applicable.
func (s *Sidecar) Stop() {
	s.stopOnce.Do(func() {
		<-s.started
		s.mu.Lock()
		srv := s.srv
		ln := s.ln
		unixPath := s.unixSocketPath
		s.mu.Unlock()
		if srv != nil {
			ctx, cancel := context.WithTimeout(context.Background(), s.opts.shutdownTimeout)
			defer cancel()
			_ = srv.Shutdown(ctx)
		}
		if ln != nil {
			_ = ln.Close()
		}
		if unixPath != "" {
			_ = os.Remove(unixPath)
		}
	})
}

// responseswsHandler is the http.Handler for per-WebSocket-session logic.
type responseswsHandler struct {
	egressURL         string
	firstFrameTimeout time.Duration
	log               *slog.Logger
	onTurn            func(TurnRecord)     // optional; called from upstream pump goroutine per completed turn
	onSummary         func(SessionSummary) // optional; called once at session end
}

type streamContext struct {
	requestID string
}

func (h *responseswsHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	log := h.log
	start := time.Now()

	sessionID := newSessionID()
	traceID := r.Header.Get(headerTraceParent)
	requestID := r.Header.Get(headerRequestID)

	clientConn, err := websocket.Accept(w, r, &websocket.AcceptOptions{
		InsecureSkipVerify: true, // Envoy owns downstream TLS
	})
	if err != nil {
		log.Error("orange-responsesws: accept failed", "err", err)
		return
	}
	clientConn.SetReadLimit(defaultReadLimit)
	log.Info("orange-responsesws: client accepted",
		"session_id", sessionID,
		"path", r.URL.Path,
		"request_id", requestID,
		"read_limit_bytes", defaultReadLimit,
	)

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
			log.Error("orange-responsesws: close client connection", "err", err)
		}
		log.Info("orange-responsesws: session ended",
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
	firstFrameTimeout := h.firstFrameTimeout
	if firstFrameTimeout <= 0 {
		firstFrameTimeout = defaultFirstFrameTimeout
	}
	log.Info("orange-responsesws: waiting for first client frame",
		"session_id", sessionID,
		"timeout", firstFrameTimeout.String(),
	)
	firstCtx, firstCancel := context.WithTimeout(r.Context(), firstFrameTimeout)
	defer firstCancel()
	var firstFrame []byte
	firstReadAttempt := 0
	for {
		firstReadAttempt++
		log.Info("orange-responsesws: reading first client frame",
			"session_id", sessionID,
			"attempt", firstReadAttempt,
			"read_limit_bytes", defaultReadLimit,
		)
		msgType, frame, err := clientConn.Read(firstCtx)
		if err != nil {
			errClass = "first_frame_read"
			log.Warn("orange-responsesws: first frame read failed",
				"session_id", sessionID,
				"attempt", firstReadAttempt,
				"err", err,
			)
			clientConn.Close(websocket.StatusPolicyViolation, "expected text frame") //nolint:errcheck
			return
		}
		if msgType != websocket.MessageText {
			errClass = "first_frame_read"
			log.Warn("orange-responsesws: first frame rejected",
				"session_id", sessionID,
				"message_type", msgType,
				"bytes", len(frame),
			)
			clientConn.Close(websocket.StatusPolicyViolation, "expected text frame") //nolint:errcheck
			return
		}
		log.Info("orange-responsesws: first client frame received",
			"session_id", sessionID,
			"bytes", len(frame),
			"summary", summarizeFirstFrame(frame),
		)

		parsedFrame, err := parseResponseCreateFrame(frame)
		if err != nil {
			errClass = "first_frame_parse"
			log.Warn("orange-responsesws: first frame rejected",
				"session_id", sessionID,
				"err", err,
				"summary", summarizeFirstFrame(frame),
			)
			clientConn.Close(websocket.StatusPolicyViolation, err.Error()) //nolint:errcheck
			return
		}
		model = parsedFrame.Model
		if parsedFrame.IsWarmup() {
			log.Info("orange-responsesws: completing local warmup",
				"session_id", sessionID,
				"model", model,
			)
			if err := writeWarmupCompleted(firstCtx, clientConn, sessionID); err != nil {
				closeReason = closeReasonUpstream
				errClass = "warmup_write"
				log.Warn("orange-responsesws: warmup completion write failed",
					"session_id", sessionID,
					"err", err,
				)
				return
			}
			log.Info("orange-responsesws: local warmup completed",
				"session_id", sessionID,
				"model", model,
			)
			continue
		}
		if sanitized, stripped := stripLocalWarmupPreviousResponseID(frame); stripped {
			log.Info("orange-responsesws: stripped local warmup previous_response_id",
				"session_id", sessionID,
				"bytes_before", len(frame),
				"bytes_after", len(sanitized),
			)
			frame = sanitized
		}
		firstFrame = frame
		break
	}

	// Step 3: resolve provider from active orange.yaml snapshot.
	cfg := config.Get()
	var provider config.Provider
	var ok bool
	log.Info("orange-responsesws: resolving model provider",
		"session_id", sessionID,
		"model", model,
	)
	providerName, provider, ok = cfg.LookupModelProvider(model, match.EndpointResponses)
	if !ok {
		closeReason = closeReasonLookupErr
		errClass = "unknown_model"
		log.Warn("orange-responsesws: unknown model", "model", model)
		clientConn.Close(websocket.StatusPolicyViolation, "unknown model") //nolint:errcheck
		return
	}
	_, backendModel = cfg.LookupModel(model, match.EndpointResponses)
	providerKind = provider.Kind
	log.Info("orange-responsesws: model provider resolved",
		"session_id", sessionID,
		"provider", providerName,
		"kind", providerKind,
		"model", model,
		"backend_model", backendModel,
	)

	// Step 4: set a 60-minute session deadline.
	sessionCtx, sessionCancel := context.WithTimeout(r.Context(), maxSessionDuration)
	defer sessionCancel()

	// Step 5: build egress upgrade headers with internal orange-responsesws-* values.
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
	log.Info("orange-responsesws: dialing egress websocket",
		"session_id", sessionID,
		"url", egressURL,
		"provider", providerName,
		"kind", providerKind,
		"model", model,
		"backend_model", backendModel,
	)
	egressConn, _, err := websocket.Dial(sessionCtx, egressURL, dialOpts)
	if err != nil {
		closeReason = closeReasonParseErr
		errClass = "egress_dial"
		log.Error("orange-responsesws: egress dial failed", "url", egressURL, "err", err)
		clientConn.Close(websocket.StatusInternalError, "upstream unavailable") //nolint:errcheck
		return
	}
	egressConn.SetReadLimit(defaultReadLimit)
	log.Info("orange-responsesws: egress websocket connected",
		"session_id", sessionID,
		"url", egressURL,
		"read_limit_bytes", defaultReadLimit,
	)
	defer func() {
		if err := egressConn.CloseNow(); err != nil {
			log.Error("orange-responsesws: close egress connection", "err", err)
		}
	}()

	// Single shared FrameTap — client goroutine calls FeedClient, upstream goroutine
	// calls FeedUpstream; mutex protects inFlight at turn boundaries.
	tap := &FrameTap{
		sessionID: sessionID,
		traceID:   traceID,
		requestID: requestID,
		onTurn: func(rec TurnRecord) {
			rec.Provider = providerName
			rec.ProviderKind = providerKind
			rec.BackendModel = backendModel
			publishTurn(rec)
			if h.onTurn != nil {
				h.onTurn(rec)
			}
		},
	}

	// Step 7: forward the first frame, then run bidirectional pump.
	tap.FeedClient(firstFrame)
	log.Info("orange-responsesws: forwarding first client frame to egress",
		"session_id", sessionID,
		"bytes", len(firstFrame),
		"summary", summarizeFirstFrame(firstFrame),
	)
	if err := egressConn.Write(sessionCtx, websocket.MessageText, firstFrame); err != nil {
		closeReason = closeReasonUpstream
		errClass = "first_frame_forward"
		log.Warn("orange-responsesws: first frame forward failed",
			"session_id", sessionID,
			"err", err,
		)
		return
	}
	log.Info("orange-responsesws: first client frame forwarded",
		"session_id", sessionID,
		"bytes", len(firstFrame),
	)

	errc := make(chan error, 2)

	// Client → upstream
	go func() {
		for {
			log.Log(sessionCtx, observability.LevelTrace, "orange-responsesws: pump client->egress reading",
				"session_id", sessionID,
			)
			mt, data, err := clientConn.Read(sessionCtx)
			if err != nil {
				log.Debug("orange-responsesws: pump client->egress read ended",
					"session_id", sessionID,
					"err", err,
				)
				errc <- fmt.Errorf("client read: %w", err)
				return
			}
			log.Debug("orange-responsesws: pump client->egress read frame",
				"session_id", sessionID,
				"message_type", mt,
				"bytes", len(data),
				"summary", summarizeMaybeJSON(mt, data),
			)
			if mt == websocket.MessageText {
				if sanitized, stripped := stripLocalWarmupPreviousResponseID(data); stripped {
					log.Info("orange-responsesws: pump client->egress stripped local warmup previous_response_id",
						"session_id", sessionID,
						"bytes_before", len(data),
						"bytes_after", len(sanitized),
					)
					data = sanitized
				}
				tap.FeedClient(data)
			}
			if err := egressConn.Write(sessionCtx, mt, data); err != nil {
				log.Debug("orange-responsesws: pump client->egress write ended",
					"session_id", sessionID,
					"message_type", mt,
					"bytes", len(data),
					"err", err,
				)
				errc <- fmt.Errorf("egress write: %w", err)
				return
			}
			log.Log(sessionCtx, observability.LevelTrace, "orange-responsesws: pump client->egress wrote frame",
				"session_id", sessionID,
				"message_type", mt,
				"bytes", len(data),
			)
		}
	}()

	// Upstream → client
	go func() {
		for {
			log.Log(sessionCtx, observability.LevelTrace, "orange-responsesws: pump egress->client reading",
				"session_id", sessionID,
			)
			mt, data, err := egressConn.Read(sessionCtx)
			if err != nil {
				log.Debug("orange-responsesws: pump egress->client read ended",
					"session_id", sessionID,
					"err", err,
				)
				errc <- fmt.Errorf("upstream read: %w", err)
				return
			}
			log.Debug("orange-responsesws: pump egress->client read frame",
				"session_id", sessionID,
				"message_type", mt,
				"bytes", len(data),
				"summary", summarizeMaybeJSON(mt, data),
			)
			if mt == websocket.MessageText {
				tap.FeedUpstream(data)
			}
			if err := clientConn.Write(sessionCtx, mt, data); err != nil {
				log.Debug("orange-responsesws: pump egress->client write ended",
					"session_id", sessionID,
					"message_type", mt,
					"bytes", len(data),
					"err", err,
				)
				errc <- fmt.Errorf("client write: %w", err)
				return
			}
			log.Log(sessionCtx, observability.LevelTrace, "orange-responsesws: pump egress->client wrote frame",
				"session_id", sessionID,
				"message_type", mt,
				"bytes", len(data),
			)
		}
	}()

	// Step 8: wait for first pump error, classify close reason.
	firstErr := <-errc
	closeReason = classifyCloseReason(sessionCtx, firstErr)
	if closeReason != closeReasonNormal {
		errClass = firstErr.Error()
	}
	log.Info("orange-responsesws: pump ended",
		"session_id", sessionID,
		"close_reason", closeReason,
		"err", firstErr,
	)

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

type responseCreateFrame struct {
	Type     string `json:"type"`
	Model    string `json:"model"`
	Generate *bool  `json:"generate"`
	Response struct {
		Model string `json:"model"`
	} `json:"response"`
}

func (f responseCreateFrame) IsWarmup() bool {
	return f.Generate != nil && !*f.Generate
}

// parseResponseCreate parses the first client frame and returns the model field.
// Returns an error if the frame is not valid JSON, not a response.create event,
// or is missing the model field.
func parseResponseCreate(data []byte) (model string, err error) {
	f, err := parseResponseCreateFrame(data)
	if err != nil {
		return "", err
	}
	return f.Model, nil
}

func parseResponseCreateFrame(data []byte) (responseCreateFrame, error) {
	if !bytes.Contains(data, []byte(`"response.create"`)) {
		return responseCreateFrame{}, fmt.Errorf("expected response.create frame")
	}
	var f responseCreateFrame
	if err := json.Unmarshal(data, &f); err != nil {
		return responseCreateFrame{}, fmt.Errorf("invalid JSON: %w", err)
	}
	if f.Type != "response.create" {
		return responseCreateFrame{}, fmt.Errorf("expected type response.create, got %q", f.Type)
	}
	if f.Model == "" {
		f.Model = f.Response.Model
	}
	if f.Model == "" {
		return responseCreateFrame{}, fmt.Errorf("response.create missing model field")
	}
	return f, nil
}

func writeWarmupCompleted(ctx context.Context, conn *websocket.Conn, sessionID string) error {
	data, err := warmupCompletedFrame(sessionID)
	if err != nil {
		return err
	}
	return conn.Write(ctx, websocket.MessageText, data)
}

func warmupCompletedFrame(sessionID string) ([]byte, error) {
	frame := map[string]any{
		"type": "response.completed",
		"response": map[string]any{
			"id":     localWarmupResponseIDPrefix + sessionID,
			"status": "completed",
			"usage": map[string]int{
				"input_tokens":  0,
				"output_tokens": 0,
				"total_tokens":  0,
			},
		},
	}
	return json.Marshal(frame)
}

func stripLocalWarmupPreviousResponseID(data []byte) ([]byte, bool) {
	if !bytes.Contains(data, []byte(`"previous_response_id"`)) {
		return data, false
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return data, false
	}
	var previousResponseID string
	if err := json.Unmarshal(raw["previous_response_id"], &previousResponseID); err != nil {
		return data, false
	}
	if !strings.HasPrefix(previousResponseID, localWarmupResponseIDPrefix) {
		return data, false
	}
	delete(raw, "previous_response_id")
	sanitized, err := json.Marshal(raw)
	if err != nil {
		return data, false
	}
	return sanitized, true
}

func summarizeFirstFrame(data []byte) map[string]any {
	summary := map[string]any{
		"bytes": len(data),
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		summary["json"] = "invalid"
		return summary
	}
	summary["keys"] = sortedJSONKeys(raw)
	var eventType string
	if rawType, ok := raw["type"]; ok {
		_ = json.Unmarshal(rawType, &eventType)
	}
	if eventType != "" {
		summary["type"] = eventType
	}
	var model string
	if rawModel, ok := raw["model"]; ok {
		_ = json.Unmarshal(rawModel, &model)
	}
	if model == "" {
		var response struct {
			Model string `json:"model"`
		}
		if rawResponse, ok := raw["response"]; ok {
			_ = json.Unmarshal(rawResponse, &response)
			model = response.Model
		}
	}
	if model != "" {
		summary["model"] = model
	}
	return summary
}

func summarizeMaybeJSON(mt websocket.MessageType, data []byte) map[string]any {
	if mt != websocket.MessageText {
		return map[string]any{
			"bytes": len(data),
		}
	}
	return summarizeFirstFrame(data)
}

func sortedJSONKeys(raw map[string]json.RawMessage) []string {
	keys := make([]string, 0, len(raw))
	for k := range raw {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
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
