// Package classify is the downstream HTTP filter for orange.
//
// On a POST to /v1/chat/completions or /v1/messages it:
//   - stores a new [pending.Pending] in the per-stream object bag
//     (via Writer.SetStreamObject) at headers phase. hostpick reads it in
//     ChooseHost via ClusterLBContext.GetStreamObject and waits until classify
//     resolves it.
//   - in the body phase, parses the `model` field out of the JSON body,
//     looks up the upstream from config.models[], rewrites :authority to the
//     provider host (so auto_sni/auto_san_validation see the right name when
//     TLS handshakes for the suspended-then-resumed upstream connection),
//     writes the routing filter state and dynamic metadata, and resolves the
//     Pending.
//
// On a missing/unknown model it returns a JSON local response per
// config.classify.on_miss and resolves the Pending with an error code so the
// async ChooseHost waiter can complete cleanly.
package classify

import (
	"encoding/json"

	"github.com/dio/transit/examples/orange/config"
	"github.com/dio/transit/examples/orange/pending"
	"github.com/dio/transit/up"
	"github.com/tidwall/gjson"
)

// errorBody is the response shape for classify-generated errors.
type errorBody struct {
	Error string `json:"error"`
	Code  string `json:"code"`
}

const (
	FilterName = "orange-classify"

	// StreamObjectKey is the per-stream bag key under which classify stores the
	// *pending.Pending. hostpick imports this constant to look up the Pending
	// via ClusterLBContext.GetStreamObject — no string literal duplication.
	StreamObjectKey = "orange.pending"

	// Filter state — only the cluster LB can read this.
	StateUpstream = "orange.upstream"
	StateProvider = "orange.provider"
	StateModel    = "orange.model"

	// Dynamic metadata — readable by upstream HTTP filters (translate).
	MetadataNamespace   = "orange"
	MetadataKeyUpstream = "upstream"
	MetadataKeyProvider = "provider"
	MetadataKeyModel    = "model"

	pathV1ChatCompletions = "/v1/chat/completions"
	pathV1Messages        = "/v1/messages"

	// ErrModelRequired and ErrUnknownModel are the orange.* codes published on
	// pending.Result.Err. They mirror the error response codes.
	ErrModelRequired = "orange.model_required"

	// ErrStreamTerminated is the orange.* code published on pending.Result.Err
	// when the stream ends before the body handler resolves the Pending —
	// downstream disconnect, idle timeout, another filter's local reply, reset.
	// Resolve is CAS, so this is a no-op when bodyHandler already published a
	// result.
	ErrStreamTerminated = "orange.stream_terminated"
)

func init() {
	up.Register(FilterName, requestHandler,
		up.WithMutableBody(bodyHandler),
		up.WithOnStreamComplete(onStreamComplete))
}

// streamState is stashed in the per-stream context slot at headers phase so
// the body handler can find the same Pending without re-parsing anything.
type streamState struct {
	p *pending.Pending
}

func requestHandler(w *up.Writer, r *up.Request) {
	if r.Method != "POST" {
		return
	}
	switch r.Path {
	case pathV1ChatCompletions, pathV1Messages:
	default:
		return
	}

	p := pending.New()
	if r.Context != nil {
		*r.Context = &streamState{p: p}
	}
	// Store the *Pending in the per-stream object bag (Primitive A).
	// hostpick reads it in ChooseHost via ClusterLBContext.GetStreamObject.
	w.SetStreamObject(StreamObjectKey, p)
	w.Log(up.LogInfo, "orange-classify headers: authority_in=%s", r.Host)
}

func bodyHandler(w *up.Writer, chunk *up.BodyChunk) {
	w.Log(up.LogInfo, "orange-classify body: endStream=%v dataLen=%d", chunk.EndStream, len(chunk.Data))
	if !chunk.EndStream {
		// WithMutableBody fires once at end-of-stream, but be defensive.
		return
	}
	if chunk.Context == nil || *chunk.Context == nil {
		return // request we didn't tag in headers phase
	}
	st, ok := (*chunk.Context).(*streamState)
	if !ok || st == nil {
		return
	}
	// Cleanup (a terminal Resolve) is owned by onStreamComplete so it runs
	// even when this handler doesn't — downstream disconnect after headers,
	// idle timeout, foreign local reply. The SDK's Primitive A owns bag
	// lifetime; no manual Delete is needed here.

	cfg := config.Get()
	field := cfg.Classify.ModelField
	if field == "" {
		field = "model"
	}

	model := gjson.GetBytes(chunk.Data, field).String()
	if model == "" {
		st.p.Resolve(pending.Result{Err: ErrModelRequired})
		sendError(w, 400, ErrModelRequired,
			"request body is missing the `"+field+"` field")
		return
	}

	upstream := cfg.LookupModel(model)
	if upstream == "" {
		st.p.Resolve(pending.Result{Err: cfg.Classify.OnMiss.Code})
		sendError(w, cfg.Classify.OnMiss.Status, cfg.Classify.OnMiss.Code,
			"no upstream configured for model "+model)
		return
	}

	prov := cfg.Providers[upstream]
	// Rewrite :authority to the provider host so the upstream sees the right
	// Host header. SNI is no longer driven by :authority — it comes from each
	// host's `sni` metadata via transport_socket_matches (see envoy.tmpl.yaml).
	if host := prov.Host(); host != "" {
		w.SetRequestHeader(":authority", host)
	}

	w.SetFilterState(StateModel, model)
	w.SetFilterState(StateUpstream, upstream)
	w.SetMetadata(MetadataNamespace, MetadataKeyUpstream, upstream)
	w.SetMetadata(MetadataNamespace, MetadataKeyModel, model)
	if prov.Kind != "" {
		w.SetFilterState(StateProvider, prov.Kind)
		w.SetMetadata(MetadataNamespace, MetadataKeyProvider, prov.Kind)
	}

	w.Log(up.LogInfo, "orange-classify body: resolved model=%s provider=%s host=%s kind=%s",
		model, upstream, prov.Host(), prov.Kind)
	st.p.Resolve(pending.Result{
		Provider: upstream,
		Kind:     prov.Kind,
		Model:    model,
	})
}

// onStreamComplete is the single owner of per-stream cleanup. It runs once
// per stream regardless of how Envoy terminated it. The SDK's Primitive A
// drains the stream-object bag unconditionally; this callback only needs to
// publish the terminal ErrStreamTerminated so hostpick can complete cleanly.
//
// Resolve is a CAS — when bodyHandler already published a real Result this is
// a no-op.
func onStreamComplete(ctx *any) {
	if ctx == nil || *ctx == nil {
		return
	}
	st, ok := (*ctx).(*streamState)
	if !ok || st == nil {
		return
	}
	if st.p != nil {
		st.p.Resolve(pending.Result{Err: ErrStreamTerminated})
	}
	// No pending.Delete here: the stream-object bag is owned and drained
	// by the SDK (Primitive A / dropBag in filter.OnStreamComplete).
}

func sendError(w *up.Writer, status int, code, msg string) {
	body, _ := json.Marshal(errorBody{Error: msg, Code: code})
	w.SendLocalResponse(status, body,
		[2]string{"content-type", "application/json"})
}
