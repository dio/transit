// Package match is the downstream HTTP filter for orange.
//
// On a POST to /v1/chat/completions, /v1/messages, or /v1/responses it:
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
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"github.com/tidwall/gjson"

	"github.com/dio/transit/examples/orange/internal/config"
	"github.com/dio/transit/examples/orange/internal/send"
	"github.com/dio/transit/up"
)

// Target is one hop in a fallback chain.
// ProviderBackend is the config-level backend name; BackendModel is the
// model name to send to that backend; ProviderKind is the API wire-format.
type Target struct {
	ProviderBackend string `json:"pb"`
	BackendModel    string `json:"bm"`
	ProviderKind    string `json:"pk"`
}

// Decision is the resolved value match publishes per request.
//
// The two identity fields have names that do not match what the access log
// calls them — this is intentional and frozen by the log schema:
//
//   - ProviderBackend  is the config-level backend name (e.g. "gemini", "openai_direct").
//     It is stored under MetadataKeyUpstream and appears in the access log as
//     the "upstream" field.
//
//   - ProviderKind  is the API wire-format used by that backend (e.g. "openai",
//     "anthropic"). It is stored under MetadataKeyProvider and appears in the
//     access log as the "provider" field. Upstream HTTP filters (adapt, meter)
//     use it to select the correct request/response codec — NOT to identify
//     which company's API is on the other end. A Gemini backend translating
//     to the OpenAI wire format will have ProviderKind == "openai".
//
// Concretely: for a request routed to a Gemini upstream via the OpenAI
// compatibility shim the log shows  upstream=gemini  provider=openai.
//
// Err is the orange.* error code (e.g. "orange.model_required",
// "orange.model_not_found"). When Err is set, Provider/Kind/Model are
// undefined; pick will Complete the host selection with a nil host and
// that string as errDetail, but match has already sent a local response so
// the stream is on its way to closing anyway.
type Decision struct {
	// ProviderBackend is the config-level backend name (maps to MetadataKeyUpstream /
	// log field "upstream"), e.g. "gemini", "openai_direct".
	ProviderBackend string
	// ProviderKind is the API wire-format used by that backend (maps to
	// MetadataKeyProvider / log field "provider"), e.g. "openai", "anthropic".
	// Used by the adapter and meter to pick the correct codec; unrelated to the
	// upstream's brand identity. A Gemini backend has ProviderKind == "openai"
	// when it speaks the OpenAI compatibility wire-format.
	ProviderKind string
	Model        string // client-facing model ID, kept for telemetry
	BackendModel string // resolved backend model name (from models[].name, or == Model if unset)
	Endpoint     string // endpoint discriminator, e.g. EndpointChatCompletions
	Binding      string // named binding within the provider; empty means "default"
	Fallbacks    []Target
	Err          string
}

// Apply writes the routing filter state and dynamic metadata for this Decision.
//
// Note the cross-mapping: ProviderBackend → MetadataKeyUpstream ("upstream"),
// ProviderKind → MetadataKeyProvider ("provider"). See Decision for the rationale.
func (d Decision) Apply(w *up.Writer) {
	w.SetFilterState(StateModel, d.Model)
	w.SetFilterState(StateUpstream, d.ProviderBackend) // config backend name → "upstream"
	w.SetFilterState(StateProvider, d.ProviderKind)    // API wire-format kind → "provider"
	w.SetFilterState(StateEndpoint, d.Endpoint)
	w.SetFilterState(StateBinding, d.Binding)
	w.SetMetadata(MetadataNamespace, MetadataKeyModel, d.Model)
	w.SetMetadata(MetadataNamespace, MetadataKeyUpstream, d.ProviderBackend) // config backend name → "upstream"
	w.SetMetadata(MetadataNamespace, MetadataKeyProvider, d.ProviderKind)    // API wire-format kind → "provider"
	w.SetMetadata(MetadataNamespace, MetadataKeyBackendModel, d.BackendModel)
	w.SetMetadata(MetadataNamespace, MetadataKeyEndpoint, d.Endpoint)
	w.SetMetadata(MetadataNamespace, MetadataKeyBinding, d.Binding)
	if len(d.Fallbacks) > 0 {
		if b, err := json.Marshal(d.Fallbacks); err == nil {
			w.SetFilterState(StateFallbacks, string(b))
		}
	}
}

// DecisionKey is the typed stream-object key match uses to store the
// per-request promise. pick calls DecisionKey.GetFromCtx to retrieve it
// without a string literal or a type assertion.
var DecisionKey = up.NewStreamKey[*up.StreamPromise[Decision]]("orange.decision")

// EndpointKey stores the endpoint discriminator so bodyHandler can read it
// without the original request path being available.
var EndpointKey = up.NewStreamKey[string]("orange.endpoint")

// ResolvedKeyKey stores the resolved config.ResolvedKey for key-mode requests
// when the new AppState system is active.
var ResolvedKeyKey = up.NewStreamKey[config.ResolvedKey]("orange.resolved_key")

// KeyIDKey stores the resolved key id (bearer token) for key-mode requests.
var KeyIDKey = up.NewStreamKey[string]("orange.key_id")

const (
	FilterName = "orange-match"

	// Filter state — only the cluster LB can read this.
	StateUpstream  = "orange.provider_backend"
	StateProvider  = "orange.provider_kind"
	StateModel     = "orange.model"
	StateEndpoint  = "orange.endpoint"
	StateBinding   = "orange.provider_binding"
	StateFallbacks = "orange.fallbacks"
	// StateAttempt is written by the adapt upstream filter after each HTTP
	// attempt (value is the 1-based attempt number as a decimal string). Pick's
	// ChooseHost reads this to select the correct fallback chain target on retries.
	// GetHostSelectionRetryCount() cannot be used because it counts within-attempt
	// host-selection retries and resets to 0 at the start of each new HTTP attempt.
	StateAttempt = "orange.adapt.attempt"

	// Dynamic metadata — readable by upstream HTTP filters (adapt, meter).
	//
	// MetadataKeyUpstream stores Decision.ProviderBackend (config backend name, e.g. "gemini").
	// MetadataKeyProvider stores Decision.ProviderKind    (API wire-format, e.g. "openai").
	// These appear verbatim as the "provider_backend" and "provider_kind" fields in the access log.
	//
	// IMPORTANT: the string values of MetadataKeyUpstream and MetadataKeyProvider are
	// referenced directly by the DYNAMIC_METADATA(...) expressions in every Envoy access
	// log format (envoy.tmpl.yaml, envoy.yaml, e2e/testdata/envoy.tmpl.yaml). Both must
	// be updated together whenever these strings change.
	MetadataNamespace       = "orange"
	MetadataKeyUpstream     = "provider_backend" // config backend name — NOT the API wire-format
	MetadataKeyProvider     = "provider_kind"    // API wire-format kind — NOT the config backend name
	MetadataKeyModel        = "model"
	MetadataKeyBackendModel = "backend_model"
	MetadataKeyEndpoint     = "endpoint"
	MetadataKeyBinding      = "provider_binding"

	// Endpoint discriminator values carried on Decision.Endpoint.
	EndpointChatCompletions = "chat_completions"
	EndpointMessages        = "messages"
	EndpointCountTokens     = "count_tokens"
	EndpointResponses       = "responses"
	EndpointEmbeddings      = "embeddings"
	EndpointImages          = "images"

	pathV1ChatCompletions     = "/v1/chat/completions"
	pathV1Messages            = "/v1/messages"
	pathV1MessagesCountTokens = "/v1/messages/count_tokens"
	pathV1Models              = "/v1/models"
	pathV1Responses           = "/v1/responses"
	pathV1Embeddings          = "/v1/embeddings"
	pathV1ImageGenerations    = "/v1/images/generations"
	pathMCP                   = "/mcp"

	// Request-side image generation metadata forwarded to the meter.
	MetadataKeyImageSize    = "image_size"
	MetadataKeyImageQuality = "image_quality"

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

	// ErrUnknownKey is the orange.* code published when key-mode is active and
	// the request bears an unknown or missing API key.
	ErrUnknownKey = "orange.unknown_key"

	// MetadataKeyRejectReason is the dynamic metadata key written when a request
	// is rejected at the headers phase (e.g. unknown key).
	MetadataKeyRejectReason = "reject_reason"

	// Attribution metadata keys written when a request resolves through a KeyBlob.
	MetadataKeyAttributionWorkspace = "attribution.workspace"
	MetadataKeyAttributionUser      = "attribution.user"
	MetadataKeyAttributionKeyID     = "attribution.key_id"
)

// matchAppState is the new-system config source. When non-nil, match uses
// matchAppState.Snapshot() instead of the legacy config.Get().
var matchAppState *config.AppState

// SetAppState configures the new-system AppState for the match filter.
// Call before Envoy initialises the filter.
func SetAppState(s *config.AppState) {
	matchAppState = s
}

var router = up.NewRouter(func(w *up.Writer, r *up.Request) {
	send.Errorf(w, http.StatusNotFound, send.NotFoundError, ErrNotFound, "no handler for %s %s", r.Method, r.Path)
}).
	GET(pathV1Models, listModels).
	POST(pathV1ChatCompletions, tagRequestForEndpoint(EndpointChatCompletions)).
	POST(pathV1MessagesCountTokens, tagRequestForEndpoint(EndpointCountTokens)).
	POST(pathV1Messages, tagRequestForEndpoint(EndpointMessages)).
	POST(pathV1Responses, tagRequestForEndpoint(EndpointResponses)).
	POST(pathV1Embeddings, tagRequestForEndpoint(EndpointEmbeddings)).
	POST(pathV1ImageGenerations, tagRequestForEndpoint(EndpointImages)).
	GET(pathV1Responses, func(*up.Writer, *up.Request) {}). // passthrough for WS upgrades → orange-responsesws sidecar
	GETPrefix(pathMCP, func(*up.Writer, *up.Request) {}).   // passthrough to orange-mcp sidecar
	POSTPrefix(pathMCP, func(*up.Writer, *up.Request) {}).  // passthrough to orange-mcp sidecar
	DELETEPrefix(pathMCP, func(*up.Writer, *up.Request) {}) // passthrough to orange-mcp sidecar

func init() {
	up.Register(FilterName, router.Dispatch,
		up.WithAttributes("scope", "match"),
		up.WithMutableBody(bodyHandler),
		up.WithOnStreamComplete(onStreamComplete))
}

// tagRequestForEndpoint returns a headers-phase handler that stores the
// per-stream promise and endpoint discriminator in the stream-object bag.
// When key-mode is active (keys[] present) it also resolves the per-key blob
// from the Authorization header and rejects unknown keys immediately.
func tagRequestForEndpoint(endpoint string) func(*up.Writer, *up.Request) {
	return func(w *up.Writer, r *up.Request) {
		if r.Context == nil {
			panic("BUG: r.Context is nil; SDK must provide a per-stream context slot")
		}
		p := up.NewStreamPromise[Decision]()
		*r.Context = p
		DecisionKey.Set(w, p)
		EndpointKey.Set(w, endpoint)

		// Key-mode: resolve the bearer key when keys[] are configured.
		if matchAppState != nil {
			cfgSnap := matchAppState.Snapshot()
			if cfgSnap != nil && cfgSnap.Keys != nil && len(cfgSnap.Keys) > 0 {
				keyID := parseBearerToken(r.Header("authorization"))
				if keyID == "" {
					// Also check x-api-key header (used by some Anthropic clients)
					keyID = r.Header("x-api-key")
				}
				if keyID == "" {
					w.Slog().Warn("Rejected unknown key", "endpoint", endpoint, "headers", r.AllHeaders())
					w.SetMetadata(MetadataNamespace, MetadataKeyRejectReason, ErrUnknownKey)
					p.Resolve(Decision{Endpoint: endpoint, Err: ErrUnknownKey})
					send.Error(w, http.StatusUnauthorized, send.AuthenticationError, ErrUnknownKey, "unknown or missing API key")
					return
				}
				keyRec, ok := cfgSnap.Keys[keyID]
				if !ok {
					w.Slog().Warn("Rejected unknown key", "endpoint", endpoint, "keyID", keyID)
					w.SetMetadata(MetadataNamespace, MetadataKeyRejectReason, ErrUnknownKey)
					p.Resolve(Decision{Endpoint: endpoint, Err: ErrUnknownKey})
					send.Error(w, http.StatusUnauthorized, send.AuthenticationError, ErrUnknownKey, "unknown or missing API key")
					return
				}
				resolvedKey, err := config.ResolveKey(keyRec, cfgSnap)
				if err != nil {
					w.Slog().Warn("Rejected unknown key", "endpoint", endpoint, "keyID", keyID, "err", err)
					w.SetMetadata(MetadataNamespace, MetadataKeyRejectReason, ErrUnknownKey)
					p.Resolve(Decision{Endpoint: endpoint, Err: ErrUnknownKey})
					send.Error(w, http.StatusUnauthorized, send.AuthenticationError, ErrUnknownKey, "unknown or missing API key")
					return
				}
				ResolvedKeyKey.Set(w, resolvedKey)
				KeyIDKey.Set(w, keyID)
				w.SetMetadata(MetadataNamespace, MetadataKeyAttributionWorkspace, resolvedKey.Workspace)
				w.SetMetadata(MetadataNamespace, MetadataKeyAttributionUser, resolvedKey.User)
				w.SetMetadata(MetadataNamespace, MetadataKeyAttributionKeyID, keyID)
			}
		}
		w.Slog().Info("Received headers", "authority_in", r.Host, "endpoint", endpoint)
	}
}

func listModels(w *up.Writer, _ *up.Request) {
	if matchAppState == nil {
		send.Error(w, http.StatusInternalServerError, send.InternalServerError, "orange.internal_error", "config not loaded")
		return
	}
	cfgSnap := matchAppState.Snapshot()
	if cfgSnap == nil || cfgSnap.Global == nil {
		send.Error(w, http.StatusInternalServerError, send.InternalServerError, "orange.internal_error", "config not loaded")
		return
	}
	list := config.V1ModelList{
		Object: "list",
		Data:   v1ModelsWithMeta(cfgSnap.Global),
	}
	if err := send.JSON(w, http.StatusOK, list); err != nil {
		w.Slog().Error("Failed to marshal model list", "err", err)
		send.Error(w, http.StatusInternalServerError, send.InternalServerError, "orange.internal_error", "failed to encode model list")
	}
}

// v1ModelsWithMeta builds a sorted V1Model slice from GlobalConfig, including
// Metadata and using the provider NAME (map key) as OwnedBy to match the
// OpenAI-compatible response format.
func v1ModelsWithMeta(global *config.GlobalConfig) []config.V1Model {
	out := make([]config.V1Model, 0, len(global.Models))
	// Build a reverse map: ProviderRecord pointer → provider name.
	provNameByPtr := make(map[*config.ProviderRecord]string, len(global.Providers))
	for name, p := range global.Providers {
		provNameByPtr[p] = name
	}
	for id, m := range global.Models {
		ownedBy := ""
		if m.Provider != nil {
			ownedBy = provNameByPtr[m.Provider]
		}
		entry := config.V1Model{
			ID:      id,
			Object:  "model",
			OwnedBy: ownedBy,
		}
		if m.Metadata != nil {
			meta := map[string]any{}
			if m.Metadata.Description != "" {
				meta["description"] = m.Metadata.Description
			}
			if m.Metadata.ContextLength != 0 {
				meta["context_length"] = m.Metadata.ContextLength
			}
			if m.Metadata.MaxTokens != 0 {
				meta["max_tokens"] = m.Metadata.MaxTokens
			}
			if len(m.Metadata.Tags) > 0 {
				meta["tags"] = m.Metadata.Tags
			}
			if len(meta) > 0 {
				entry.Metadata = meta
			}
		}
		out = append(out, entry)
	}
	// Sort for stable output.
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j].ID < out[j-1].ID; j-- {
			out[j], out[j-1] = out[j-1], out[j]
		}
	}
	return out
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

	endpoint, _ := EndpointKey.Get(w)

	model := gjson.GetBytes(chunk.Data, "model").String()
	if model == "" {
		w.Slog().Warn("Received body missing model", "endStream", chunk.EndStream)
		p.Resolve(Decision{Endpoint: endpoint, Err: ErrModelRequired})
		send.Error(w, http.StatusBadRequest, send.InvalidRequestError, ErrModelRequired, "request body is missing the `model` field")
		return
	}

	// Use new AppState when available.
	if matchAppState != nil {
		cfgSnap := matchAppState.Snapshot()
		if cfgSnap == nil || cfgSnap.Global == nil {
			p.Resolve(Decision{Endpoint: endpoint, Err: ErrUnknownModel})
			send.Error(w, http.StatusInternalServerError, send.InternalServerError, "orange.internal_error", "config not loaded")
			return
		}

		// Resolve model record from global catalog.
		modelRec, ok := cfgSnap.Global.LookupModel(model)
		if !ok {
			w.Slog().Warn("Received body unknown model", "model", model)
			p.Resolve(Decision{Endpoint: endpoint, Err: ErrUnknownModel})
			send.Errorf(w, http.StatusNotFound, send.InvalidRequestError, ErrUnknownModel, "no upstream configured for model %s", model)
			return
		}

		// Check per-key routing overrides first.
		var routingCfg *config.RoutingConfig
		if rk, hasKey := ResolvedKeyKey.Get(w); hasKey && rk.RoutingOverrides != nil {
			if rc, hasOverride := rk.RoutingOverrides[model]; hasOverride {
				routingCfg = rc
			} else if rc, hasWildcard := rk.RoutingOverrides["*"]; hasWildcard {
				routingCfg = rc
			}
		}

		var upstream, backendModel, binding string
		var fallbacks []Target
		if routingCfg != nil {
			var rerr error
			upstream, backendModel, binding, fallbacks, rerr = resolveRoutingNew(cfgSnap.Global, routingCfg, model)
			if rerr != nil {
				w.Slog().Warn("Routing resolution failed", "model", model, "err", rerr)
				p.Resolve(Decision{Endpoint: endpoint, Err: ErrUnknownModel})
				send.Errorf(w, http.StatusNotFound, send.InvalidRequestError, ErrUnknownModel, "no upstream configured for model %s", model)
				return
			}
		} else {
			// Sugar path: use model record's provider directly.
			if modelRec.Provider == nil {
				w.Slog().Warn("Received body unknown model", "model", model)
				p.Resolve(Decision{Endpoint: endpoint, Err: ErrUnknownModel})
				send.Errorf(w, http.StatusNotFound, send.InvalidRequestError, ErrUnknownModel, "no upstream configured for model %s", model)
				return
			}
			// Find the provider name from global providers map.
			upstream = providerName(cfgSnap.Global, modelRec.Provider)
			backendModel = modelRec.APIName
			if backendModel == "" {
				backendModel = model
			}
			binding = modelRec.Binding
			// Apply endpoint discriminator override.
			if endpoint != "" {
				if ovProv, has := modelRec.EndpointOverrides[endpoint]; has && ovProv != nil {
					upstream = providerName(cfgSnap.Global, ovProv)
				}
			}
		}

		if upstream == "" {
			w.Slog().Warn("Received body unknown model", "model", model)
			p.Resolve(Decision{Endpoint: endpoint, Err: ErrUnknownModel})
			send.Errorf(w, http.StatusNotFound, send.InvalidRequestError, ErrUnknownModel, "no upstream configured for model %s", model)
			return
		}
		provRec := cfgSnap.Global.Providers[upstream]
		if provRec == nil {
			w.Slog().Warn("Provider not found", "upstream", upstream)
			p.Resolve(Decision{Endpoint: endpoint, Err: ErrUnknownModel})
			send.Errorf(w, http.StatusNotFound, send.InvalidRequestError, ErrUnknownModel, "no upstream configured for model %s", model)
			return
		}
		w.SetRequestHeader(up.HeaderAuthority, provRec.BindingHost(binding))

		if routingCfg != nil && routingCfg.Kind == config.RoutingKindChain {
			if ch := routingCfg.Chain; ch != nil && len(ch.Children) > 1 {
				w.SetRequestHeader("x-envoy-max-retries", strconv.Itoa(len(ch.Children)-1))
				if ch.Retry != nil {
					r := ch.Retry
					if r.RetryOn != "" {
						w.SetRequestHeader("x-envoy-retry-on", r.RetryOn)
					}
					if r.RetryGrpcOn != "" {
						w.SetRequestHeader("x-envoy-retry-grpc-on", r.RetryGrpcOn)
					}
					if r.PerTryTimeoutMs > 0 {
						w.SetRequestHeader("x-envoy-upstream-rq-per-try-timeout-ms", strconv.Itoa(r.PerTryTimeoutMs))
					}
					if len(r.RetriableStatusCodes) > 0 {
						parts := make([]string, len(r.RetriableStatusCodes))
						for i, c := range r.RetriableStatusCodes {
							parts[i] = strconv.Itoa(c)
						}
						w.SetRequestHeader("x-envoy-retriable-status-codes", strings.Join(parts, ","))
					}
					if len(r.RetriableHeaderNames) > 0 {
						w.SetRequestHeader("x-envoy-retriable-header-names", strings.Join(r.RetriableHeaderNames, ","))
					}
				}
			}
		}

		d := Decision{ProviderBackend: upstream, ProviderKind: string(provRec.Kind), Model: model, BackendModel: backendModel, Endpoint: endpoint, Binding: binding, Fallbacks: fallbacks}
		d.Apply(w)

		if endpoint == EndpointImages {
			if size := gjson.GetBytes(chunk.Data, "size").String(); size != "" {
				w.SetMetadata(MetadataNamespace, MetadataKeyImageSize, size)
			}
			if quality := gjson.GetBytes(chunk.Data, "quality").String(); quality != "" {
				w.SetMetadata(MetadataNamespace, MetadataKeyImageQuality, quality)
			}
		}

		w.Slog().Info("Received body resolved", "model", model, "backend_model", backendModel, "provider", upstream, "host", provRec.BindingHost(binding), "kind", string(provRec.Kind), "endpoint", endpoint, "binding", binding)
		p.Resolve(d)
		return
	}

	// AppState not configured — reject with model not found.
	w.Slog().Warn("Received body: AppState not configured", "model", model)
	p.Resolve(Decision{Endpoint: endpoint, Err: ErrUnknownModel})
	send.Errorf(w, http.StatusNotFound, send.InvalidRequestError, ErrUnknownModel, "no upstream configured for model %s", model)
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

// ProviderNameFromGlobal looks up the name (key) of prov in global.Providers by
// pointer equality. Returns "" when not found.
// It is exported for use by sibling packages (e.g. responsesws) that perform the
// same model→upstream name resolution.
func ProviderNameFromGlobal(global *config.GlobalConfig, prov *config.ProviderRecord) string {
	return providerName(global, prov)
}

// providerName looks up the name (key) of prov in global.Providers by pointer
// equality. Returns "" when not found.
func providerName(global *config.GlobalConfig, prov *config.ProviderRecord) string {
	for name, p := range global.Providers {
		if p == prov {
			return name
		}
	}
	return ""
}

// resolveRoutingNew walks the new-system *RoutingConfig tree and returns the
// primary upstream name, backend model, binding, fallback slice, and any error.
// It is the new-system counterpart of resolveRouting.
func resolveRoutingNew(global *config.GlobalConfig, node *config.RoutingConfig, entryModel string) (
	upstream, backendModel, binding string, fallbacks []Target, err error,
) {
	switch node.Kind {
	case config.RoutingKindTarget:
		t := node.Target
		if t == nil || t.Provider == nil {
			return "", "", "", nil, fmt.Errorf("routing target has no provider")
		}
		bm := t.ModelName
		if bm == "" {
			bm = entryModel
		}
		return providerName(global, t.Provider), bm, "", nil, nil

	case config.RoutingKindChain:
		ch := node.Chain
		if ch == nil || len(ch.Children) == 0 {
			return "", "", "", nil, fmt.Errorf("chain node has no children")
		}
		for i, child := range ch.Children {
			if child.Kind == config.RoutingKindChain {
				return "", "", "", nil, fmt.Errorf("chain.children[%d] is itself a chain (chain-of-chain not supported)", i)
			}
		}
		u, bm, bi, _, e := resolveRoutingNew(global, &ch.Children[0], entryModel)
		if e != nil {
			return "", "", "", nil, e
		}
		upstream, backendModel, binding = u, bm, bi
		for _, child := range ch.Children[1:] {
			childCopy := child
			cu, cbm, _, _, ce := resolveRoutingNew(global, &childCopy, entryModel)
			if ce != nil {
				return "", "", "", nil, ce
			}
			pk := ""
			if prov, ok := global.Providers[cu]; ok {
				pk = string(prov.Kind)
			}
			fallbacks = append(fallbacks, Target{
				ProviderBackend: cu,
				BackendModel:    cbm,
				ProviderKind:    pk,
			})
		}
		return upstream, backendModel, binding, fallbacks, nil

	case config.RoutingKindSplit:
		sp := node.Split
		if sp == nil || len(sp.Children) == 0 {
			return "", "", "", nil, fmt.Errorf("split node has no children")
		}
		idx := sampleSplitNew(sp)
		child := sp.Children[idx].Child
		return resolveRoutingNew(global, &child, entryModel)

	default:
		return "", "", "", nil, fmt.Errorf("routing node has no target, chain, or split")
	}
}

// ResolveKey parses the Authorization bearer token from authHeader and looks up
// the key in the current AppState snapshot. Returns the resolved workspace,
// user, keyID, and true when found. Returns "", "", "", false when AppState is
// not active, no keys are configured, or the key is absent.
func ResolveKey(authHeader string) (workspace, user, keyID string, found bool) {
	keyID = parseBearerToken(authHeader)
	if keyID == "" {
		return "", "", "", false
	}
	if matchAppState == nil {
		return "", "", "", false
	}
	snap := matchAppState.Snapshot()
	if snap == nil || snap.Keys == nil || len(snap.Keys) == 0 {
		return "", "", keyID, false
	}
	keyRec, ok := snap.Keys[keyID]
	if !ok {
		return "", "", keyID, false
	}
	resolvedKey, err := config.ResolveKey(keyRec, snap)
	if err != nil {
		return "", "", keyID, false
	}
	return resolvedKey.Workspace, resolvedKey.User, keyID, true
}

// parseBearerToken extracts the token value from an Authorization header.
// The comparison is case-insensitive on the "Bearer " prefix.
func parseBearerToken(authHeader string) string {
	const prefix = "bearer "
	if len(authHeader) <= len(prefix) {
		return ""
	}
	if !strings.EqualFold(authHeader[:len(prefix)], prefix) {
		return ""
	}
	return strings.TrimSpace(authHeader[len(prefix):])
}
