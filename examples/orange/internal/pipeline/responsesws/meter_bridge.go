package responsesws

import (
	"sync"
	"time"

	"github.com/dio/transit/examples/orange/internal/pipeline/match"
	"github.com/dio/transit/examples/orange/internal/pipeline/meter"
	"github.com/dio/transit/up"
)

type meterState struct {
	mu      sync.Mutex
	updated chan struct{}
	record  responseswsMeterRecord
	has     bool
}

type responseswsMeterRecord struct {
	SessionID      string
	RequestID      string
	Model          string
	Provider       string
	ProviderKind   string
	BackendModel   string
	GeneratedTurns int
	CloseReason    string
	Usage          meter.TokenUsage
}

var meterStates sync.Map // map[x-request-id]*meterState

func requestHandler(_ *up.Writer, r *up.Request) {
	if r == nil || r.Context == nil || r.Method != "GET" || r.Path != "/v1/responses" {
		return
	}
	requestID := r.Header(headerRequestID)
	if requestID == "" {
		return
	}
	ensureMeterState(requestID)
	*r.Context = &streamContext{requestID: requestID}
}

func responseHandler(w *up.Writer, chunk *up.ResponseChunk) {
	if chunk == nil || !chunk.EndStream || chunk.Context == nil || *chunk.Context == nil {
		return
	}
	ctx, ok := (*chunk.Context).(*streamContext)
	if !ok || ctx == nil || ctx.requestID == "" {
		return
	}
	record, ok := waitMeterRecord(ctx.requestID, responseswsMeterWaitTimeout)
	if !ok {
		w.Slog().Warn("orange-responsesws meter unavailable for access log", "request_id", ctx.requestID)
		return
	}

	w.SetMetadata(match.MetadataNamespace, match.MetadataKeyModel, record.Model)
	w.SetMetadata(match.MetadataNamespace, match.MetadataKeyUpstream, record.Provider)
	w.SetMetadata(match.MetadataNamespace, match.MetadataKeyProvider, record.ProviderKind)
	w.SetMetadata(match.MetadataNamespace, match.MetadataKeyBackendModel, record.BackendModel)
	w.SetMetadata(match.MetadataNamespace, match.MetadataKeyEndpoint, match.EndpointResponses)
	meter.EmitUsage(w, record.Usage)
	w.Slog().Info("orange-responsesws metered",
		"request_id", ctx.requestID,
		"session_id", record.SessionID,
		"model", record.Model,
		"provider", record.Provider,
		"input_tokens", record.Usage.Input,
		"output_tokens", record.Usage.Output,
		"generated_turns", record.GeneratedTurns,
	)
}

func ensureMeterState(requestID string) *meterState {
	if requestID == "" {
		return nil
	}
	actual, _ := meterStates.LoadOrStore(requestID, &meterState{updated: make(chan struct{}, 1)})
	return actual.(*meterState)
}

func publishTurn(rec TurnRecord) {
	if rec.RequestID == "" {
		return
	}
	w := ensureMeterState(rec.RequestID)
	if w == nil {
		return
	}
	w.mu.Lock()
	w.record.SessionID = rec.SessionID
	w.record.RequestID = rec.RequestID
	w.record.Model = rec.Model
	w.record.Provider = rec.Provider
	w.record.ProviderKind = rec.ProviderKind
	w.record.BackendModel = rec.BackendModel
	if rec.Outcome == TurnOutcomeCompleted && !rec.IsWarmup {
		w.record.GeneratedTurns++
	}
	if rec.UsageStatus == UsageStatusReported {
		w.record.Usage.Input += rec.Usage.Input
		w.record.Usage.Output += rec.Usage.Output
		w.record.Usage.CachedInput += rec.Usage.CachedInput
		w.record.Usage.CacheCreationInput += rec.Usage.CacheCreationInput
		w.record.Usage.CacheReadInput += rec.Usage.CacheReadInput
		w.record.Usage.ReasoningOutput += rec.Usage.ReasoningOutput
	}
	w.has = true
	w.mu.Unlock()
	signalMeterUpdate(w)
}

func publishSummary(summary SessionSummary) {
	if summary.RequestID == "" {
		return
	}
	actual, ok := meterStates.Load(summary.RequestID)
	if !ok {
		return
	}
	w := actual.(*meterState)
	w.mu.Lock()
	w.record.SessionID = summary.SessionID
	w.record.RequestID = summary.RequestID
	w.record.Model = summary.Model
	w.record.Provider = summary.Provider
	w.record.ProviderKind = summary.ProviderKind
	w.record.BackendModel = summary.BackendModel
	w.record.GeneratedTurns = summary.GeneratedTurns
	w.record.CloseReason = summary.CloseReason
	w.record.Usage = summary.AggUsage
	w.has = true
	w.mu.Unlock()
	signalMeterUpdate(w)
}

func waitMeterRecord(requestID string, timeout time.Duration) (responseswsMeterRecord, bool) {
	w := ensureMeterState(requestID)
	if w == nil {
		return responseswsMeterRecord{}, false
	}
	if record, ok := currentMeterRecord(w); ok {
		meterStates.Delete(requestID)
		return record, true
	}
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	defer meterStates.Delete(requestID)

	select {
	case <-w.updated:
		return currentMeterRecord(w)
	case <-timer.C:
		return responseswsMeterRecord{}, false
	}
}

func currentMeterRecord(w *meterState) (responseswsMeterRecord, bool) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.record, w.has
}

func signalMeterUpdate(w *meterState) {
	select {
	case w.updated <- struct{}{}:
	default:
	}
}
