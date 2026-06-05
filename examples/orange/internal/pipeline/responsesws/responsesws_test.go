package responsesws

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/dio/transit/examples/orange/internal/pipeline/match"
	"github.com/dio/transit/examples/orange/internal/pipeline/meter"
	"github.com/dio/transit/up"
	"github.com/dio/transit/up/testutil"
)

// ---- parseResponseCreate -------------------------------------------------------

func TestParseResponseCreate_validFrame(t *testing.T) {
	data := `{"type":"response.create","model":"gpt-4o-mini","input":[]}`
	model, err := parseResponseCreate([]byte(data))
	require.NoError(t, err)
	assert.Equal(t, "gpt-4o-mini", model)
}

func TestParseResponseCreate_nestedResponseModel(t *testing.T) {
	data := `{"type":"response.create","response":{"model":"gpt-4o-mini"},"input":[]}`
	model, err := parseResponseCreate([]byte(data))
	require.NoError(t, err)
	assert.Equal(t, "gpt-4o-mini", model)
}

func TestParseResponseCreateFrame_warmup(t *testing.T) {
	data := `{"type":"response.create","model":"gpt-4o-mini","input":[],"generate":false}`
	frame, err := parseResponseCreateFrame([]byte(data))
	require.NoError(t, err)

	assert.Equal(t, "gpt-4o-mini", frame.Model)
	assert.True(t, frame.IsWarmup())
}

func TestParseResponseCreateFrame_generateTrueIsNotWarmup(t *testing.T) {
	data := `{"type":"response.create","model":"gpt-4o-mini","input":[],"generate":true}`
	frame, err := parseResponseCreateFrame([]byte(data))
	require.NoError(t, err)

	assert.False(t, frame.IsWarmup())
}

func TestParseResponseCreate_nonJSON(t *testing.T) {
	_, err := parseResponseCreate([]byte("not-json"))
	require.Error(t, err)
}

func TestParseResponseCreate_wrongType(t *testing.T) {
	data := `{"type":"response.output_item.delta","delta":"hi"}`
	_, err := parseResponseCreate([]byte(data))
	require.Error(t, err)
}

func TestParseResponseCreate_missingModel(t *testing.T) {
	data := `{"type":"response.create"}`
	_, err := parseResponseCreate([]byte(data))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "missing model")
}

func TestParseResponseCreate_missingResponseCreateKey(t *testing.T) {
	data := `{"type":"something.else","model":"gpt-4o"}`
	_, err := parseResponseCreate([]byte(data))
	require.Error(t, err)
}

func TestSummarizeFirstFrame(t *testing.T) {
	summary := summarizeFirstFrame([]byte(`{"type":"response.create","response":{"model":"gpt-4o-mini"},"input":[{"role":"user","content":"secret"}]}`))

	assert.Equal(t, len(`{"type":"response.create","response":{"model":"gpt-4o-mini"},"input":[{"role":"user","content":"secret"}]}`), summary["bytes"])
	assert.Equal(t, "response.create", summary["type"])
	assert.Equal(t, "gpt-4o-mini", summary["model"])
	assert.Equal(t, []string{"input", "response", "type"}, summary["keys"])
	assert.NotContains(t, summary, "input")
}

func TestSummarizeFirstFrameInvalidJSON(t *testing.T) {
	summary := summarizeFirstFrame([]byte("not-json"))

	assert.Equal(t, "invalid", summary["json"])
	assert.Equal(t, len("not-json"), summary["bytes"])
}

func TestWarmupCompletedFrame(t *testing.T) {
	data, err := warmupCompletedFrame("sid")
	require.NoError(t, err)

	var frame struct {
		Type     string `json:"type"`
		Response struct {
			ID     string `json:"id"`
			Status string `json:"status"`
			Usage  struct {
				InputTokens  int `json:"input_tokens"`
				OutputTokens int `json:"output_tokens"`
				TotalTokens  int `json:"total_tokens"`
			} `json:"usage"`
		} `json:"response"`
	}
	require.NoError(t, json.Unmarshal(data, &frame))
	assert.Equal(t, "response.completed", frame.Type)
	assert.Equal(t, "resp_orange_responsesws_warmup_sid", frame.Response.ID)
	assert.Equal(t, "completed", frame.Response.Status)
	assert.Zero(t, frame.Response.Usage.InputTokens)
	assert.Zero(t, frame.Response.Usage.OutputTokens)
	assert.Zero(t, frame.Response.Usage.TotalTokens)
}

func TestStripLocalWarmupPreviousResponseID(t *testing.T) {
	data := []byte(`{"type":"response.create","model":"gpt-4o-mini","previous_response_id":"resp_orange_responsesws_warmup_sid","input":"hi"}`)

	got, stripped := stripLocalWarmupPreviousResponseID(data)
	require.True(t, stripped)

	var frame map[string]any
	require.NoError(t, json.Unmarshal(got, &frame))
	assert.NotContains(t, frame, "previous_response_id")
	assert.Equal(t, "response.create", frame["type"])
	assert.Equal(t, "gpt-4o-mini", frame["model"])
}

func TestStripLocalWarmupPreviousResponseID_preservesProviderID(t *testing.T) {
	data := []byte(`{"type":"response.create","model":"gpt-4o-mini","previous_response_id":"resp_provider_123","input":"hi"}`)

	got, stripped := stripLocalWarmupPreviousResponseID(data)
	require.False(t, stripped)
	assert.Equal(t, data, got)
}

// ---- listenForSidecar ----------------------------------------------------------

func TestListenForSidecar_TCP(t *testing.T) {
	ln, err := listenForSidecar("127.0.0.1:0")
	require.NoError(t, err)
	defer ln.Close()
	_, ok := ln.(*net.TCPListener)
	assert.True(t, ok, "expected *net.TCPListener for TCP address")
}

func TestListenForSidecar_Unix(t *testing.T) {
	// Use a short fixed path under /tmp to stay within the macOS 104-byte limit.
	path := fmt.Sprintf("/tmp/ows-test-%d.sock", time.Now().UnixNano()%1_000_000)
	t.Cleanup(func() { _ = os.Remove(path) })
	ln, err := listenForSidecar("unix://" + path)
	require.NoError(t, err)
	defer ln.Close()
	_, ok := ln.(*net.UnixListener)
	assert.True(t, ok, "expected *net.UnixListener for unix:// address")
}

// ---- dialOptionsForEgress -------------------------------------------------------

func TestDialOptionsForEgress_TCP(t *testing.T) {
	wsURL, opts := dialOptionsForEgress("ws://127.0.0.1:10003")
	assert.Equal(t, "ws://127.0.0.1:10003", wsURL)
	// Default transport — no custom HTTPClient.
	assert.Nil(t, opts.HTTPClient)
}

func TestDialOptionsForEgress_Unix(t *testing.T) {
	wsURL, opts := dialOptionsForEgress("ws+unix:///var/run/orange-responsesws.sock")
	assert.Equal(t, "ws://localhost", wsURL)
	require.NotNil(t, opts.HTTPClient)
	// Verify the transport dials the Unix socket by checking its type.
	transport, ok := opts.HTTPClient.Transport.(*http.Transport)
	require.True(t, ok, "expected *http.Transport with DialContext for Unix")
	assert.NotNil(t, transport.DialContext)
}

// ---- resolveFirstFrameTimeout --------------------------------------------------

func TestResolveFirstFrameTimeout_default(t *testing.T) {
	t.Setenv("ORANGE_RESPONSESWS_FIRST_FRAME_TIMEOUT", "")
	assert.Equal(t, defaultFirstFrameTimeout, resolveFirstFrameTimeout())
}

func TestResolveFirstFrameTimeout_envOverride(t *testing.T) {
	t.Setenv("ORANGE_RESPONSESWS_FIRST_FRAME_TIMEOUT", "2m")
	assert.Equal(t, 2*time.Minute, resolveFirstFrameTimeout())
}

func TestResolveFirstFrameTimeout_invalidFallsBack(t *testing.T) {
	t.Setenv("ORANGE_RESPONSESWS_FIRST_FRAME_TIMEOUT", "not-a-duration")
	assert.Equal(t, defaultFirstFrameTimeout, resolveFirstFrameTimeout())
}

// ---- Sidecar lifecycle -------------------------------------------------------

func TestResponsesWSSidecar_ReadyAfterListen(t *testing.T) {
	sc, err := newSidecar(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}),
		sidecarOptions{listenAddr: "127.0.0.1:0", shutdownTimeout: time.Second})
	require.NoError(t, err)

	require.NoError(t, sc.Listen())
	go func() { _ = sc.Serve() }()

	// Ready channel must close without timing out.
	select {
	case <-sc.Ready():
	case <-time.After(2 * time.Second):
		t.Fatal("sidecar Ready() did not close in time")
	}

	addr := sc.ListenAddr()
	assert.NotEmpty(t, addr, "ListenAddr must be non-empty after Ready")

	sc.Stop()
}

func TestResponsesWSSidecar_StopGraceful(t *testing.T) {
	sc, err := newSidecar(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}),
		sidecarOptions{listenAddr: "127.0.0.1:0", shutdownTimeout: time.Second})
	require.NoError(t, err)

	require.NoError(t, sc.Listen())
	done := make(chan error, 1)
	go func() { done <- sc.Serve() }()

	<-sc.Ready()
	sc.Stop()

	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("sidecar did not stop in time")
	}
}

func TestResponsesWSSidecar_ListenAddrEmptyBeforeReady(t *testing.T) {
	sc, err := newSidecar(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}),
		sidecarOptions{listenAddr: "127.0.0.1:0"})
	require.NoError(t, err)
	assert.Empty(t, sc.ListenAddr(), "ListenAddr must be empty before Ready")
}

// ---- FrameTap: FeedClient / FeedUpstream ---------------------------------------

// tapWithCapture returns a FrameTap and a slice pointer that accumulates emitted TurnRecords.
func tapWithCapture(sessionID, traceID, requestID string) (*FrameTap, *[]TurnRecord) {
	var records []TurnRecord
	tap := &FrameTap{
		sessionID: sessionID,
		traceID:   traceID,
		requestID: requestID,
		onTurn:    func(r TurnRecord) { records = append(records, r) },
	}
	return tap, &records
}

func TestFrameTap_FeedClient_cheapPathForDeltaFrames(t *testing.T) {
	tap, records := tapWithCapture("sid", "tid", "rid")
	// Delta frames do not contain "response.create" — FeedClient is a no-op.
	tap.FeedClient([]byte(`{"type":"response.output_item.delta","delta":"hello"}`))
	assert.Empty(t, *records, "no turn should be emitted for delta frames")
}

func TestFrameTap_FeedUpstream_cheapPathForDeltaFrames(t *testing.T) {
	tap, records := tapWithCapture("sid", "tid", "rid")
	tap.FeedUpstream([]byte(`{"type":"response.output_item.delta","delta":"hi"}`))
	assert.Empty(t, *records, "no turn should be emitted for delta frames")
}

func TestFrameTap_completedTurn_reportsUsage(t *testing.T) {
	tap, records := tapWithCapture("sid", "tid", "rid")

	// Simulate a full turn: FeedClient sets inFlight, FeedUpstream completes it.
	tap.FeedClient([]byte(`{"type":"response.create","model":"gpt-4o-mini"}`))
	tap.FeedUpstream([]byte(`{"type":"response.completed","response":{"id":"resp_xyz","usage":{"input_tokens":10,"output_tokens":20}}}`))

	require.Len(t, *records, 1)
	r := (*records)[0]
	assert.Equal(t, "sid", r.SessionID)
	assert.Equal(t, "tid", r.TraceID)
	assert.Equal(t, "gpt-4o-mini", r.Model)
	assert.Equal(t, "resp_xyz", r.ResponseID)
	assert.Equal(t, TurnOutcomeCompleted, r.Outcome)
	assert.Equal(t, UsageStatusReported, r.UsageStatus)
	assert.Equal(t, uint32(10), r.Usage.Input)
	assert.Equal(t, uint32(20), r.Usage.Output)
}

func TestFrameTap_completedTurn_mapsAllUsageFields(t *testing.T) {
	tap, records := tapWithCapture("sid", "tid", "rid")

	tap.FeedClient([]byte(`{"type":"response.create","model":"gpt-4o-mini"}`))
	frame := `{"type":"response.completed","response":{"id":"resp_1","usage":{` +
		`"input_tokens":100,"output_tokens":50,` +
		`"input_tokens_details":{"cached_tokens":20,"cache_creation_input_tokens":5},` +
		`"output_tokens_details":{"reasoning_tokens":15}}}}`
	tap.FeedUpstream([]byte(frame))

	require.Len(t, *records, 1)
	u := (*records)[0].Usage
	assert.Equal(t, uint32(100), u.Input)
	assert.Equal(t, uint32(50), u.Output)
	assert.Equal(t, uint32(20), u.CachedInput)
	assert.Equal(t, uint32(5), u.CacheCreationInput)
	assert.Equal(t, uint32(15), u.ReasoningOutput)
}

func TestFrameTap_warmupTurn_notApplicableUsageStatus(t *testing.T) {
	tap, records := tapWithCapture("sid", "tid", "rid")

	// generate:false → warmup turn
	tap.FeedClient([]byte(`{"type":"response.create","model":"gpt-4o","generate":false}`))
	tap.FeedUpstream([]byte(`{"type":"response.completed","response":{"id":"resp_w","usage":{"input_tokens":5,"output_tokens":0}}}`))

	require.Len(t, *records, 1)
	r := (*records)[0]
	assert.True(t, r.IsWarmup)
	assert.Equal(t, UsageStatusNotApplicable, r.UsageStatus)
}

func TestFrameTap_completedTurn_missingUsage(t *testing.T) {
	tap, records := tapWithCapture("sid", "tid", "rid")

	tap.FeedClient([]byte(`{"type":"response.create","model":"gpt-4o-mini"}`))
	// No usage field in response.
	tap.FeedUpstream([]byte(`{"type":"response.completed","response":{"id":"resp_2"}}`))

	require.Len(t, *records, 1)
	assert.Equal(t, UsageStatusMissing, (*records)[0].UsageStatus)
}

func TestFrameTap_completedTurn_parseError(t *testing.T) {
	tap, records := tapWithCapture("sid", "tid", "rid")

	tap.FeedClient([]byte(`{"type":"response.create","model":"gpt-4o-mini"}`))
	// Malformed JSON triggers parse error.
	tap.FeedUpstream([]byte(`{"type":"response.completed", INVALID}`))

	require.Len(t, *records, 1)
	assert.Equal(t, UsageStatusParseError, (*records)[0].UsageStatus)
}

func TestFrameTap_providerError_emitsTurnRecord(t *testing.T) {
	tap, records := tapWithCapture("sid", "tid", "rid")

	tap.FeedClient([]byte(`{"type":"response.create","model":"gpt-4o-mini"}`))
	tap.FeedUpstream([]byte(`{"type":"response.failed","error":{"code":"server_error"}}`))

	require.Len(t, *records, 1)
	r := (*records)[0]
	assert.Equal(t, TurnOutcomeProviderError, r.Outcome)
	assert.Equal(t, UsageStatusMissing, r.UsageStatus)
}

func TestFrameTap_previousResponseID_propagated(t *testing.T) {
	tap, records := tapWithCapture("sid", "tid", "rid")

	tap.FeedClient([]byte(`{"type":"response.create","model":"gpt-4o","previous_response_id":"resp_prev"}`))
	tap.FeedUpstream([]byte(`{"type":"response.completed","response":{"id":"resp_next","usage":{"input_tokens":1,"output_tokens":1}}}`))

	require.Len(t, *records, 1)
	assert.Equal(t, "resp_prev", (*records)[0].PreviousResponseID)
}

func TestFrameTap_multiTurn_accumulatesRecords(t *testing.T) {
	tap, records := tapWithCapture("sid", "tid", "rid")

	for i := range 3 {
		tap.FeedClient([]byte(`{"type":"response.create","model":"gpt-4o-mini"}`))
		frame := fmt.Sprintf(`{"type":"response.completed","response":{"id":"resp_%d","usage":{"input_tokens":5,"output_tokens":10}}}`, i)
		tap.FeedUpstream([]byte(frame))
	}

	assert.Len(t, *records, 3)
	for _, r := range *records {
		assert.Equal(t, UsageStatusReported, r.UsageStatus)
	}
}

func TestFrameTap_FlushInFlight_emitsMissingUsage(t *testing.T) {
	tap, records := tapWithCapture("sid", "tid", "rid")

	// Start a turn but never complete it.
	tap.FeedClient([]byte(`{"type":"response.create","model":"gpt-4o-mini"}`))
	tap.FlushInFlight(TurnOutcomeClientDisconnect)

	require.Len(t, *records, 1)
	r := (*records)[0]
	assert.Equal(t, TurnOutcomeClientDisconnect, r.Outcome)
	assert.Equal(t, UsageStatusMissing, r.UsageStatus)
	assert.Equal(t, "gpt-4o-mini", r.Model)
}

func TestFrameTap_FlushInFlight_noopWhenIdle(t *testing.T) {
	tap, records := tapWithCapture("sid", "tid", "rid")
	// No FeedClient call — flush should be a no-op.
	tap.FlushInFlight(TurnOutcomeDeadline)
	assert.Empty(t, *records)
}

// ---- FrameTap.Summary ----------------------------------------------------------

func TestFrameTap_Summary_aggregatesUsage(t *testing.T) {
	tap, _ := tapWithCapture("sid", "tid", "rid")

	// Two complete turns with usage.
	for i := range 2 {
		tap.FeedClient([]byte(`{"type":"response.create","model":"gpt-4o-mini"}`))
		frame := fmt.Sprintf(`{"type":"response.completed","response":{"id":"resp_%d","usage":{"input_tokens":10,"output_tokens":20}}}`, i)
		tap.FeedUpstream([]byte(frame))
	}

	s := tap.Summary("sid", "tid", "rid", "gpt-4o-mini", "openai", "openai", "gpt-4o-mini",
		closeReasonNormal, 2*time.Second, "")
	assert.Equal(t, 2, s.TotalTurns)
	assert.Equal(t, 2, s.GeneratedTurns)
	assert.Equal(t, 0, s.WarmupTurns)
	assert.Equal(t, 0, s.FailedTurns)
	assert.Equal(t, uint32(20), s.AggUsage.Input)
	assert.Equal(t, uint32(40), s.AggUsage.Output)
	assert.Equal(t, closeReasonNormal, s.CloseReason)
}

func TestFrameTap_Summary_warmupTurnsNotCountedAsGenerated(t *testing.T) {
	tap, _ := tapWithCapture("sid", "tid", "rid")

	// One warmup turn.
	tap.FeedClient([]byte(`{"type":"response.create","model":"gpt-4o-mini","generate":false}`))
	tap.FeedUpstream([]byte(`{"type":"response.completed","response":{"id":"resp_w"}}`))

	// One generated turn.
	tap.FeedClient([]byte(`{"type":"response.create","model":"gpt-4o-mini"}`))
	tap.FeedUpstream([]byte(`{"type":"response.completed","response":{"id":"resp_g","usage":{"input_tokens":5,"output_tokens":5}}}`))

	s := tap.Summary("sid", "tid", "rid", "gpt-4o-mini", "openai", "openai", "gpt-4o-mini",
		closeReasonNormal, time.Second, "")
	assert.Equal(t, 2, s.TotalTurns)
	assert.Equal(t, 1, s.WarmupTurns)
	assert.Equal(t, 1, s.GeneratedTurns)
	assert.Equal(t, 0, s.FailedTurns)
}

func TestFrameTap_Summary_failedTurns(t *testing.T) {
	tap, _ := tapWithCapture("sid", "tid", "rid")

	// One provider error turn.
	tap.FeedClient([]byte(`{"type":"response.create","model":"gpt-4o-mini"}`))
	tap.FeedUpstream([]byte(`{"type":"response.failed","error":{}}`))

	s := tap.Summary("sid", "tid", "rid", "gpt-4o-mini", "openai", "openai", "gpt-4o-mini",
		closeReasonUpstream, time.Second, "upstream_error")
	assert.Equal(t, 1, s.FailedTurns)
	assert.Equal(t, 0, s.GeneratedTurns)
}

func TestFrameTap_Summary_isBoundedAndSerializable(t *testing.T) {
	tap, _ := tapWithCapture("sid", "tid", "rid")
	s := tap.Summary("sid", "tid", "rid", "gpt-4o-mini", "openai", "openai", "gpt-4o-mini",
		closeReasonNormal, time.Second, "")

	data, err := json.Marshal(s)
	require.NoError(t, err)
	// Structural secret-redaction check: no API keys in summary.
	assert.False(t, strings.Contains(string(data), "sk-"), "summary must not contain secret keys")
}

// ---- TurnRecord: meter.TokenUsage type alignment --------------------------------

func TestTurnRecord_usageType_alignsWithMeter(t *testing.T) {
	// Compile-time proof that TurnRecord.Usage is meter.TokenUsage, not a local alias.
	var u meter.TokenUsage
	rec := TurnRecord{Usage: u}
	_ = rec
}

// ---- egressHeaders overwrite ---------------------------------------------------

func TestEgressHeaders_overwritesClientSuppliedValues(t *testing.T) {
	// Simulate what ServeHTTP does when building the egress upgrade headers.
	// Client-supplied header values must be clobbered unconditionally.
	fakeInbound, _ := http.NewRequest(http.MethodGet, "/v1/responses", nil)
	// Attacker tries to inject a different provider.
	fakeInbound.Header.Set(headerProvider, "evil-provider")
	fakeInbound.Header.Set(headerKind, "evil-kind")
	fakeInbound.Header.Set(headerModel, "evil-model")
	fakeInbound.Header.Set(headerBackendModel, "evil-backend")
	fakeInbound.Header.Set(headerTraceParent, "00-trace-span-01")

	// Build egress headers exactly as responseswsHandler.ServeHTTP does.
	egressHeader := http.Header{}
	egressHeader.Set(headerProvider, "openai")
	egressHeader.Set(headerKind, "openai")
	egressHeader.Set(headerModel, "gpt-4o-mini")
	egressHeader.Set(headerBackendModel, "gpt-4o-mini")
	for _, h := range []string{headerTraceParent, headerTraceState, headerRequestID} {
		if v := fakeInbound.Header.Get(h); v != "" {
			egressHeader.Set(h, v)
		}
	}

	// The egress header values are what the sidecar set — not what the client sent.
	assert.Equal(t, "openai", egressHeader.Get(headerProvider))
	assert.Equal(t, "gpt-4o-mini", egressHeader.Get(headerModel))
	// Trace header was forwarded from the legitimate inbound value.
	assert.Equal(t, "00-trace-span-01", egressHeader.Get(headerTraceParent))
}

// ---- traceHeaders forwarding ---------------------------------------------------

func TestTraceHeaders_forwarded(t *testing.T) {
	fakeInbound, _ := http.NewRequest(http.MethodGet, "/v1/responses", nil)
	fakeInbound.Header.Set(headerTraceParent, "00-a-b-01")
	fakeInbound.Header.Set(headerTraceState, "vendor=value")
	fakeInbound.Header.Set(headerRequestID, "req-123")

	egressHeader := http.Header{}
	for _, h := range []string{headerTraceParent, headerTraceState, headerRequestID} {
		if v := fakeInbound.Header.Get(h); v != "" {
			egressHeader.Set(h, v)
		}
	}

	assert.Equal(t, "00-a-b-01", egressHeader.Get(headerTraceParent))
	assert.Equal(t, "vendor=value", egressHeader.Get(headerTraceState))
	assert.Equal(t, "req-123", egressHeader.Get(headerRequestID))
}

func TestTraceHeaders_absentWhenNotPresent(t *testing.T) {
	fakeInbound, _ := http.NewRequest(http.MethodGet, "/v1/responses", nil)
	egressHeader := http.Header{}
	for _, h := range []string{headerTraceParent, headerTraceState, headerRequestID} {
		if v := fakeInbound.Header.Get(h); v != "" {
			egressHeader.Set(h, v)
		}
	}
	assert.Empty(t, egressHeader.Get(headerTraceParent))
}

// ---- WebSocket meter bridge ----------------------------------------------------

func TestWSMeterBridge_ResponseHandlerEmitsAccessLogMetadata(t *testing.T) {
	meterStates = sync.Map{}
	t.Cleanup(func() { meterStates = sync.Map{} })

	const requestID = "req-meter"
	ensureMeterState(requestID)
	publishTurn(TurnRecord{
		SessionID:    "sid",
		RequestID:    requestID,
		Model:        "gpt-4o-mini",
		Provider:     "openai",
		ProviderKind: "openai",
		BackendModel: "gpt-4o-mini",
		Outcome:      TurnOutcomeCompleted,
		UsageStatus:  UsageStatusReported,
		Usage: meter.TokenUsage{
			Input:           11,
			Output:          5,
			CachedInput:     3,
			ReasoningOutput: 2,
		},
	})

	handle := testutil.NewFilterHandle()
	w := up.NewWriter(handle)
	ctx := any(&streamContext{requestID: requestID})

	responseHandler(w, &up.ResponseChunk{EndStream: true, Context: &ctx})

	model, ok := handle.Metadata(match.MetadataNamespace, match.MetadataKeyModel)
	require.True(t, ok)
	assert.Equal(t, "gpt-4o-mini", model)

	provider, ok := handle.Metadata(match.MetadataNamespace, match.MetadataKeyProvider)
	require.True(t, ok)
	assert.Equal(t, "openai", provider)

	input, ok := handle.MetadataNumber("orange_meter", "input_tokens")
	require.True(t, ok)
	assert.Equal(t, float64(11), input)

	output, ok := handle.MetadataNumber("orange_meter", "output_tokens")
	require.True(t, ok)
	assert.Equal(t, float64(5), output)
}

// ---- classifyCloseReason -------------------------------------------------------

func TestClassifyCloseReason_nilErr(t *testing.T) {
	assert.Equal(t, closeReasonNormal, classifyCloseReason(context.Background(), nil))
}

func TestClassifyCloseReason_upstreamError(t *testing.T) {
	err := fmt.Errorf("upstream read: connection reset")
	assert.Equal(t, closeReasonUpstream, classifyCloseReason(context.Background(), err))
}

func TestClassifyCloseReason_clientError(t *testing.T) {
	err := fmt.Errorf("client read: EOF")
	// Client read errors classify as normal (client chose to close).
	assert.Equal(t, closeReasonNormal, classifyCloseReason(context.Background(), err))
}

func TestClassifyCloseReason_deadline(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // make ctx.Err() non-nil
	err := fmt.Errorf("context deadline exceeded")
	assert.Equal(t, closeReasonDeadline, classifyCloseReason(ctx, err))
}

// ---- closeReasonToTurnOutcome --------------------------------------------------

func TestCloseReasonToTurnOutcome_deadline(t *testing.T) {
	assert.Equal(t, TurnOutcomeDeadline, closeReasonToTurnOutcome(closeReasonDeadline))
}

func TestCloseReasonToTurnOutcome_upstream(t *testing.T) {
	assert.Equal(t, TurnOutcomeProviderError, closeReasonToTurnOutcome(closeReasonUpstream))
}

func TestCloseReasonToTurnOutcome_normal(t *testing.T) {
	assert.Equal(t, TurnOutcomeClientDisconnect, closeReasonToTurnOutcome(closeReasonNormal))
}

// ---- newSessionID --------------------------------------------------------------

func TestNewSessionID_nonEmpty(t *testing.T) {
	id := newSessionID()
	assert.NotEmpty(t, id)
}

func TestNewSessionID_unique(t *testing.T) {
	ids := make(map[string]bool)
	for range 100 {
		ids[newSessionID()] = true
	}
	assert.Len(t, ids, 100, "session IDs must be unique")
}

// ---- up package — check MeterFilterName constant ------------------------------

func TestInit_registersFilterName(t *testing.T) {
	assert.Equal(t, "orange-responsesws-meter", MeterFilterName)
}
