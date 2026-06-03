// Package match is the downstream HTTP filter for orange.
//
// On a POST to /v1/chat/completions or /v1/messages it:
//   - stores a new [*up.StreamPromise[Decision]] in the per-stream object bag
//     via [DecisionKey].Set at headers phase. pick reads it in ChooseHost
//     via [DecisionKey].GetFromCtx and waits until match resolves it.
//   - in the body phase, parses the `model` field out of the JSON body,
//     looks up the upstream from config.models[], rewrites :authority to the
//     provider host, writes the routing filter state and dynamic metadata, and
//     resolves the promise. SNI comes from the selected host's configured
//     hostname, not from request headers.
//
// On a missing/unknown model it returns a JSON local response per
// config.classify.on_miss and resolves the promise with an error code so the
// async ChooseHost waiter can complete cleanly.
package match

import (
	"net/http"

	"github.com/tidwall/gjson"

	"github.com/dio/transit/examples/orange/internal/config"
	"github.com/dio/transit/examples/orange/internal/send"
	"github.com/dio/transit/up"
)

// Decision is the resolved value match publishes per request.
//
// Err is the orange.* error code (e.g. "orange.model_required",
// "orange.model_not_found"). When Err is set, Provider/Kind/Model are
// undefined; pick will Complete the host selection with a nil host and
// that string as errDetail, but match has already sent a local response so
// the stream is on its way to closing anyway.
type Decision struct {
	Provider     string // selected provider name, e.g. "openai_direct"
	Kind         string // provider kind, e.g. "openai"
	Model        string // client-facing model ID, kept for telemetry
	BackendModel string // resolved backend model name (from models[].name, or == Model if unset)
	Err          string
}

// Apply writes the routing filter state and dynamic metadata for this Decision.
func (d Decision) Apply(w *up.Writer) {
	w.SetFilterState(StateModel, d.Model)
	w.SetFilterState(StateUpstream, d.Provider)
	w.SetFilterState(StateProvider, d.Kind)
	w.SetMetadata(MetadataNamespace, MetadataKeyModel, d.Model)
	w.SetMetadata(MetadataNamespace, MetadataKeyUpstream, d.Provider)
	w.SetMetadata(MetadataNamespace, MetadataKeyProvider, d.Kind)
	w.SetMetadata(MetadataNamespace, MetadataKeyBackendModel, d.BackendModel)
}

// DecisionKey is the typed stream-object key match uses to store the
// per-request promise. pick calls DecisionKey.GetFromCtx to retrieve it
// without a string literal or a type assertion.
var DecisionKey = up.NewStreamKey[*up.StreamPromise[Decision]]("orange.decision")

const (
	FilterName = "orange-match"

	// Filter state — only the cluster LB can read this.
	StateUpstream = "orange.upstream"
	StateProvider = "orange.provider"
	StateModel    = "orange.model"

	// Dynamic metadata — readable by upstream HTTP filters (adapt).
	MetadataNamespace       = "orange"
	MetadataKeyUpstream     = "upstream"
	MetadataKeyProvider     = "provider"
	MetadataKeyModel        = "model"
	MetadataKeyBackendModel = "backend_model"

	pathV1ChatCompletions = "/v1/chat/completions"
	pathV1Messages        = "/v1/messages"

	// ErrModelRequired and ErrUnknownModel are the orange.* codes published on
	// Decision.Err. They mirror the error response codes.
	ErrModelRequired = "orange.model_required"
	ErrUnknownModel  = "orange.model_not_found"
	ErrNotFound      = "orange.not_found"

	// ErrStreamTerminated is the orange.* code published on Decision.Err when
	// the stream ends before the body handler resolves the promise — downstream
	// disconnect, idle timeout, another filter's local reply, reset.
	// Resolve is first-wins, so this is a no-op when bodyHandler already
	// published a result.
	ErrStreamTerminated = "orange.stream_terminated"
)

var router = up.NewRouter(func(w *up.Writer, r *up.Request) {
	send.Errorf(w, http.StatusNotFound, send.NotFoundError, ErrNotFound, "no handler for %s %s", r.Method, r.Path)
}).
	POST(pathV1ChatCompletions, tagRequest).
	POST(pathV1Messages, tagRequest)

func init() {
	up.Register(FilterName, router.Dispatch,
		up.WithAttributes("scope", "match"),
		up.WithMutableBody(bodyHandler),
		up.WithOnStreamComplete(onStreamComplete))
}

func tagRequest(w *up.Writer, r *up.Request) {
	if r.Context == nil {
		panic("BUG: r.Context is nil; SDK must provide a per-stream context slot")
	}
	p := up.NewStreamPromise[Decision]()
	*r.Context = p
	// Store the promise in the per-stream object bag.
	// pick reads it in ChooseHost via DecisionKey.GetFromCtx.
	DecisionKey.Set(w, p)
	w.Slog().Info("Received headers", "authority_in", r.Host)
}

func bodyHandler(w *up.Writer, chunk *up.BodyChunk) {
	w.Slog().Debug("Received body", "end_stream", chunk.EndStream, "data_len", len(chunk.Data))
	if !chunk.EndStream {
		// WithMutableBody fires once at end-of-stream, but be defensive.
		return
	}
	p, ok := DecisionKey.Get(w)
	if !ok {
		return // request we didn't tag in headers phase
	}
	// Cleanup (a terminal Resolve) is owned by onStreamComplete so it runs
	// even when this handler doesn't — downstream disconnect after headers,
	// idle timeout, foreign local reply. The SDK's Primitive A owns bag
	// lifetime; no manual Delete is needed here.

	model := gjson.GetBytes(chunk.Data, "model").String()
	if model == "" {
		w.Slog().Warn("Received body missing model", "endStream", chunk.EndStream)
		p.Resolve(Decision{Err: ErrModelRequired})
		send.Error(w, http.StatusBadRequest, send.InvalidRequestError, ErrModelRequired, "request body is missing the `model` field")
		return
	}

	cfg := config.Get()
	upstream, backendModel := cfg.LookupModel(model)
	if upstream == "" {
		w.Slog().Warn("Received body unknown model", "model", model)
		p.Resolve(Decision{Err: ErrUnknownModel})
		send.Errorf(w, http.StatusNotFound, send.InvalidRequestError, ErrUnknownModel, "no upstream configured for model %s", model)
		return
	}
	provider := cfg.Providers[upstream]
	// Rewrite :authority to the provider host so the upstream sees the right
	// Host header. SNI is driven by the selected host's configured hostname.
	w.SetRequestHeader(up.HeaderAuthority, provider.Host())

	d := Decision{Provider: upstream, Kind: provider.Kind, Model: model, BackendModel: backendModel}
	d.Apply(w)

	w.Slog().Info("Received body resolved", "model", model, "backend_model", backendModel, "provider", upstream, "host", provider.Host(), "kind", provider.Kind)
	p.Resolve(d)
}

// onStreamComplete is the single owner of per-stream cleanup. It runs once
// per stream regardless of how Envoy terminated it. The SDK's Primitive A
// drains the stream-object bag unconditionally; this callback only needs to
// publish the terminal ErrStreamTerminated so pick can complete cleanly.
//
// Resolve is first-wins — when bodyHandler already published a real Decision
// this is a no-op.
func onStreamComplete(ctx *any) {
	if ctx == nil || *ctx == nil {
		return
	}
	p, ok := (*ctx).(*up.StreamPromise[Decision])
	if !ok || p == nil {
		return
	}
	p.Resolve(Decision{Err: ErrStreamTerminated})
	// No bag delete here: the stream-object bag is owned and drained
	// by the SDK (Primitive A / dropBag in filter.OnStreamComplete).
}
