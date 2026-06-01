// Package classify is the downstream HTTP filter for orange.
//
// On a POST to /v1/chat/completions or /v1/messages it:
//   - mints a per-request token at headers phase and publishes it via filter
//     state (StateToken). hostpick reads this token in ChooseHost and waits on
//     a Pending until classify resolves it,
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
	"strconv"
	"sync/atomic"

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

	// Filter state — only the cluster LB can read this.
	StateUpstream = "orange.upstream"
	StateProvider = "orange.provider"
	StateModel    = "orange.model"

	// StateToken is the per-request handoff key hostpick reads to find the
	// matching pending.Pending. Kept distinct from StateUpstream so a future
	// synchronous classify variant could populate StateUpstream without
	// colliding with the async path.
	StateToken = "orange.cls-token"

	// Dynamic metadata — readable by upstream HTTP filters (credinject).
	MetadataNamespace   = "orange"
	MetadataKeyUpstream = "upstream"
	MetadataKeyProvider = "provider"
	MetadataKeyModel    = "model"

	pathV1ChatCompletions = "/v1/chat/completions"
	pathV1Messages        = "/v1/messages"

	// ErrModelRequired and ErrUnknownModel are the orange.* codes published on
	// pending.Result.Err. They mirror the error response codes.
	ErrModelRequired = "orange.model_required"
)

func init() {
	up.RegisterWithMutableBody(FilterName, requestHandler, bodyHandler, nil)
}

// streamState is stashed in the per-stream context slot at headers phase so
// the body handler can find the same token + pending without re-parsing
// anything.
type streamState struct {
	token string
	p     *pending.Pending
}

var tokenSeq atomic.Uint64

// mintToken returns a process-unique token. We don't use x-request-id because
// it's not guaranteed to be unique under our control (it can be forwarded,
// rewritten by tracing, etc.) — minting our own keeps the routing key
// hermetic.
func mintToken() string {
	return "orange-" + strconv.FormatUint(tokenSeq.Add(1), 36)
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

	token := mintToken()
	p := pending.Register(token)
	if r.Context != nil {
		*r.Context = &streamState{token: token, p: p}
	}
	// hostpick reads this in ChooseHost and waits on p.
	w.SetFilterState(StateToken, token)
	w.Log(up.LogInfo, "orange-classify headers: token=%s authority_in=%s", token, r.Host)
}

func bodyHandler(w *up.Writer, chunk *up.BodyChunk) {
	w.Log(up.LogInfo, "orange-classify body: endStream=%v dataLen=%d", chunk.EndStream, len(chunk.Data))
	if !chunk.EndStream {
		// RegisterWithMutableBody fires once at end-of-stream, but be defensive.
		return
	}
	if chunk.Context == nil || *chunk.Context == nil {
		return // request we didn't tag in headers phase
	}
	st, ok := (*chunk.Context).(*streamState)
	if !ok || st == nil {
		return
	}
	defer pending.Delete(st.token)

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

	ups := cfg.Upstreams[upstream]
	// Rewrite :authority to the provider host so the upstream sees the right
	// Host header. SNI is no longer driven by :authority — it comes from each
	// host's `sni` metadata via transport_socket_matches (see envoy.tmpl.yaml).
	if host := ups.Host(); host != "" {
		w.SetRequestHeader(":authority", host)
	}

	w.SetFilterState(StateModel, model)
	w.SetFilterState(StateUpstream, upstream)
	w.SetMetadata(MetadataNamespace, MetadataKeyUpstream, upstream)
	w.SetMetadata(MetadataNamespace, MetadataKeyModel, model)
	if ups.Kind != "" {
		w.SetFilterState(StateProvider, ups.Kind)
		w.SetMetadata(MetadataNamespace, MetadataKeyProvider, ups.Kind)
	}

	w.Log(up.LogInfo, "orange-classify body: resolved model=%s upstream=%s host=%s kind=%s",
		model, upstream, ups.Host(), ups.Kind)
	st.p.Resolve(pending.Result{
		Upstream: upstream,
		Provider: ups.Kind,
		Model:    model,
	})
}

func sendError(w *up.Writer, status int, code, msg string) {
	body, _ := json.Marshal(errorBody{Error: msg, Code: code})
	w.SendLocalResponse(status, body,
		[2]string{"content-type", "application/json"})
}
