// Package ws implements the orange-ws sidecar and orange-ws-egress-match filter
// for OpenAI-compatible Responses WebSocket traffic.
//
// # Architecture
//
//	client
//	  -> Envoy inbound listener
//	  -> route /v1/responses WebSocket upgrades to orange-ws loopback
//	  -> orange-ws sidecar (this package, ws.go)
//	       reads first response.create, extracts model
//	       resolves provider/backend through orange.yaml
//	       overwrites x-orange-ws-* headers on the egress upgrade
//	       dials Envoy egress listener
//	  -> Envoy egress listener
//	  -> orange-ws-egress-match filter (this package, egress.go)
//	       validates and consumes x-orange-ws-* headers
//	       writes Orange decision metadata/filter-state (same as HTTP match path)
//	       strips internal headers
//	  -> existing orange_default route/cluster
//	  -> existing orange-pick and orange-adapt path
//	  -> provider
package ws

import (
	"bytes"
	"encoding/json"
	"sync"
	"time"

	"github.com/dio/transit/examples/orange/internal/pipeline/meter"
)

// TurnOutcome describes the outcome of a single response turn.
type TurnOutcome string

const (
	TurnOutcomeCompleted        TurnOutcome = "completed"
	TurnOutcomeProviderError    TurnOutcome = "provider_error"
	TurnOutcomeClientDisconnect TurnOutcome = "client_disconnect"
	TurnOutcomeDeadline         TurnOutcome = "deadline"
)

// UsageStatus describes whether usage was successfully extracted for a turn.
type UsageStatus string

const (
	UsageStatusReported      UsageStatus = "reported"
	UsageStatusMissing       UsageStatus = "missing"
	UsageStatusParseError    UsageStatus = "parse_error"
	UsageStatusNotApplicable UsageStatus = "not_applicable" // generate:false warmups
)

// TurnRecord is the per-turn metering record emitted when a response turn completes.
// It is the billable ledger entry; one is emitted per completed generated turn.
// Records must be bounded and secret-redacted — no raw prompts, tool outputs,
// provider credentials, or full frames.
type TurnRecord struct {
	SessionID          string
	TraceID            string
	RequestID          string
	ResponseID         string
	PreviousResponseID string
	Model              string
	Provider           string
	ProviderKind       string
	BackendModel       string
	IsWarmup           bool // true when response.create had generate:false
	Usage              meter.TokenUsage
	StartedAt          time.Time
	CompletedAt        time.Time
	DurationMs         int64
	Outcome            TurnOutcome
	UsageStatus        UsageStatus
}

// SessionSummary is the end-of-session aggregate record emitted once at close.
// It is for debugging and reconciliation; per-turn TurnRecords are the billing source.
type SessionSummary struct {
	SessionID      string
	TraceID        string
	RequestID      string
	Model          string
	Provider       string
	ProviderKind   string
	BackendModel   string
	TotalTurns     int
	GeneratedTurns int
	WarmupTurns    int
	FailedTurns    int
	AggUsage       meter.TokenUsage
	ResponseIDs    []string
	CloseReason    string
	CloseCode      int
	DeadlineHit    bool
	Duration       time.Duration
	ErrClass       string
}

// inFlightTurn holds the per-turn state captured from a response.create frame
// and consumed when the matching response.completed frame arrives.
type inFlightTurn struct {
	model              string
	previousResponseID string
	isWarmup           bool // generate:false
	startedAt          time.Time
}

// FrameTap is a shared, mutex-protected per-session frame inspector. Create one
// per session; pass it to both pump goroutines. Each goroutine calls only its
// own method (FeedClient or FeedUpstream), so the mutex only contends at turn
// boundaries — negligible under the OpenAI one-in-flight protocol guarantee.
type FrameTap struct {
	mu sync.Mutex

	sessionID string
	traceID   string
	requestID string

	// inFlight is the turn started by the most recent response.create, cleared
	// when the matching response.completed (or error/close) arrives.
	inFlight *inFlightTurn

	// onTurn is called from FeedUpstream when a turn completes. Must be fast
	// and non-blocking — it runs in the upstream→client pump goroutine.
	onTurn func(TurnRecord)

	// Summary accumulators — updated under mu.
	turns       []TurnRecord
	responseIDs []string
}

// FeedClient inspects a client-sent frame. Full JSON parsing only occurs when
// the cheap bytes.Contains marker check passes (response.create frames). Called
// from the client→upstream pump goroutine.
func (t *FrameTap) FeedClient(data []byte) {
	if !bytes.Contains(data, []byte(`"response.create"`)) {
		return
	}
	var f struct {
		Type               string `json:"type"`
		Model              string `json:"model"`
		PreviousResponseID string `json:"previous_response_id"`
		Generate           *bool  `json:"generate"` // nil means generate:true (default)
	}
	if json.Unmarshal(data, &f) != nil || f.Type != "response.create" || f.Model == "" {
		return
	}
	isWarmup := f.Generate != nil && !*f.Generate

	t.mu.Lock()
	t.inFlight = &inFlightTurn{
		model:              f.Model,
		previousResponseID: f.PreviousResponseID,
		isWarmup:           isWarmup,
		startedAt:          time.Now(),
	}
	t.mu.Unlock()
}

// FeedUpstream inspects a provider-sent frame. Full JSON parsing only occurs
// when the cheap bytes.Contains marker check passes. Called from the
// upstream→client pump goroutine.
func (t *FrameTap) FeedUpstream(data []byte) {
	if bytes.Contains(data, []byte(`"response.completed"`)) {
		t.feedCompleted(data, TurnOutcomeCompleted)
		return
	}
	// Detect provider error frames ("response.failed", "error").
	if bytes.Contains(data, []byte(`"response.failed"`)) || bytes.Contains(data, []byte(`"error"`)) {
		t.feedError(data)
	}
	// All other frames (deltas, etc.) use the cheap forward path — no full parse.
}

// FlushInFlight emits a TurnRecord for any in-flight turn that did not receive
// a completion frame (e.g., session closed early, deadline). Should be called
// at session end before building the SessionSummary.
func (t *FrameTap) FlushInFlight(outcome TurnOutcome) {
	t.mu.Lock()
	inf := t.inFlight
	t.inFlight = nil
	t.mu.Unlock()

	if inf == nil {
		return
	}
	now := time.Now()
	rec := TurnRecord{
		SessionID:          t.sessionID,
		TraceID:            t.traceID,
		RequestID:          t.requestID,
		PreviousResponseID: inf.previousResponseID,
		Model:              inf.model,
		IsWarmup:           inf.isWarmup,
		StartedAt:          inf.startedAt,
		CompletedAt:        now,
		DurationMs:         now.Sub(inf.startedAt).Milliseconds(),
		Outcome:            outcome,
		UsageStatus:        UsageStatusMissing,
	}
	t.emit(rec)
}

// Summary returns a SessionSummary built from the emitted turn records.
// Call after FlushInFlight.
func (t *FrameTap) Summary(sessionID, traceID, requestID, model, provider, providerKind, backendModel string,
	closeReason string, duration time.Duration, errClass string) SessionSummary {
	t.mu.Lock()
	turns := t.turns
	ids := t.responseIDs
	t.mu.Unlock()

	var agg meter.TokenUsage
	var generated, warmups, failed int
	for _, r := range turns {
		if r.UsageStatus == UsageStatusReported {
			agg.Input += r.Usage.Input
			agg.Output += r.Usage.Output
			agg.CachedInput += r.Usage.CachedInput
			agg.CacheCreationInput += r.Usage.CacheCreationInput
			agg.ReasoningOutput += r.Usage.ReasoningOutput
			agg.AudioInput += r.Usage.AudioInput
			agg.AudioOutput += r.Usage.AudioOutput
		}
		if r.IsWarmup {
			warmups++
		} else if r.UsageStatus == UsageStatusMissing || r.Outcome == TurnOutcomeProviderError {
			failed++
		} else if r.Outcome == TurnOutcomeCompleted {
			generated++
		}
	}

	return SessionSummary{
		SessionID:      sessionID,
		TraceID:        traceID,
		RequestID:      requestID,
		Model:          model,
		Provider:       provider,
		ProviderKind:   providerKind,
		BackendModel:   backendModel,
		TotalTurns:     len(turns),
		GeneratedTurns: generated,
		WarmupTurns:    warmups,
		FailedTurns:    failed,
		AggUsage:       agg,
		ResponseIDs:    ids,
		CloseReason:    closeReason,
		Duration:       duration,
		ErrClass:       errClass,
	}
}

func (t *FrameTap) feedCompleted(data []byte, outcome TurnOutcome) {
	var f struct {
		Type     string `json:"type"`
		Response struct {
			ID    string             `json:"id"`
			Usage *responsesAPIUsage `json:"usage"`
		} `json:"response"`
	}

	t.mu.Lock()
	inf := t.inFlight
	t.inFlight = nil
	t.mu.Unlock()

	now := time.Now()
	rec := TurnRecord{
		SessionID: t.sessionID,
		TraceID:   t.traceID,
		RequestID: t.requestID,
		Outcome:   outcome,
		StartedAt: now, // overridden below if inFlight
	}
	if inf != nil {
		rec.Model = inf.model
		rec.PreviousResponseID = inf.previousResponseID
		rec.IsWarmup = inf.isWarmup
		rec.StartedAt = inf.startedAt
	}

	if json.Unmarshal(data, &f) != nil {
		rec.UsageStatus = UsageStatusParseError
		rec.CompletedAt = now
		rec.DurationMs = now.Sub(rec.StartedAt).Milliseconds()
		t.emit(rec)
		return
	}

	rec.ResponseID = f.Response.ID
	rec.CompletedAt = now
	rec.DurationMs = now.Sub(rec.StartedAt).Milliseconds()

	if rec.IsWarmup {
		rec.UsageStatus = UsageStatusNotApplicable
	} else if f.Response.Usage != nil {
		rec.Usage = f.Response.Usage.toTokenUsage()
		rec.UsageStatus = UsageStatusReported
	} else {
		rec.UsageStatus = UsageStatusMissing
	}

	if f.Response.ID != "" {
		t.mu.Lock()
		t.responseIDs = appendUnique(t.responseIDs, f.Response.ID)
		t.mu.Unlock()
	}
	t.emit(rec)
}

func (t *FrameTap) feedError(data []byte) {
	var f struct {
		Type string `json:"type"`
	}
	if json.Unmarshal(data, &f) != nil {
		return
	}
	if f.Type != "response.failed" && f.Type != "error" {
		return
	}

	t.mu.Lock()
	inf := t.inFlight
	t.inFlight = nil
	t.mu.Unlock()

	if inf == nil {
		return
	}
	now := time.Now()
	rec := TurnRecord{
		SessionID:          t.sessionID,
		TraceID:            t.traceID,
		RequestID:          t.requestID,
		PreviousResponseID: inf.previousResponseID,
		Model:              inf.model,
		IsWarmup:           inf.isWarmup,
		StartedAt:          inf.startedAt,
		CompletedAt:        now,
		DurationMs:         now.Sub(inf.startedAt).Milliseconds(),
		Outcome:            TurnOutcomeProviderError,
		UsageStatus:        UsageStatusMissing,
	}
	t.emit(rec)
}

func (t *FrameTap) emit(rec TurnRecord) {
	t.mu.Lock()
	t.turns = append(t.turns, rec)
	t.mu.Unlock()
	if t.onTurn != nil {
		t.onTurn(rec)
	}
}

// responsesAPIUsage mirrors the OpenAI Responses API usage object in
// response.completed frames. Field names differ from Chat Completions
// (input_tokens_details vs prompt_tokens_details).
type responsesAPIUsage struct {
	InputTokens  uint32 `json:"input_tokens"`
	OutputTokens uint32 `json:"output_tokens"`
	InputTokensDetails struct {
		CachedTokens             uint32 `json:"cached_tokens"`
		CacheCreationInputTokens uint32 `json:"cache_creation_input_tokens"`
	} `json:"input_tokens_details"`
	OutputTokensDetails struct {
		ReasoningTokens uint32 `json:"reasoning_tokens"`
	} `json:"output_tokens_details"`
}

func (u *responsesAPIUsage) toTokenUsage() meter.TokenUsage {
	return meter.TokenUsage{
		Input:              u.InputTokens,
		Output:             u.OutputTokens,
		CachedInput:        u.InputTokensDetails.CachedTokens,
		CacheCreationInput: u.InputTokensDetails.CacheCreationInputTokens,
		ReasoningOutput:    u.OutputTokensDetails.ReasoningTokens,
	}
}

func appendUnique(s []string, v string) []string {
	for _, existing := range s {
		if existing == v {
			return s
		}
	}
	return append(s, v)
}
