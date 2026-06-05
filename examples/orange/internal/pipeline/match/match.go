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

// KeyBlobKey stores the resolved *config.KeyBlob for key-mode requests.
// Absent in legacy mode (no keys[] configured).
var KeyBlobKey = up.NewStreamKey[*config.KeyBlob]("orange.key_blob")

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

		cfg := config.Get()
		// Inject per-request Envoy retry headers derived from the chain config.
		// This must happen in the headers phase so that RetryStateImpl (created
		// in the router's decodeHeaders, which runs after this filter returns
		// Continue) picks up the values. The body phase is too late.
		//
		// x-envoy-max-retries: set to the deepest chain depth minus one so
		// Envoy allows exactly as many attempts as the chain has providers.
		// x-envoy-retry-on: union of all chains' conditions (additive OR onto
		// the route's retry_on; the route must have retry_on for retries to fire).
		// x-envoy-upstream-rq-per-try-timeout-ms: maximum per_try_timeout_ms
		// declared across all chains (conservative ceiling).
		if maxRetries := cfg.MaxChainRetries(); maxRetries > 0 {
			w.SetRequestHeader("x-envoy-max-retries", strconv.Itoa(maxRetries))
		}
		if retryOn, perTryMs := cfg.ChainRetryPolicy(); retryOn != "" || perTryMs > 0 {
			if retryOn != "" {
				w.SetRequestHeader("x-envoy-retry-on", retryOn)
			}
			if perTryMs > 0 {
				w.SetRequestHeader("x-envoy-upstream-rq-per-try-timeout-ms", strconv.Itoa(perTryMs))
			}
		}
		if cfg.HasKeys() {
			kb, keyID, ok := resolveKey(r.Header("authorization"), cfg)
			if !ok {
				w.Slog().Warn("Rejected unknown key", "endpoint", endpoint)
				w.SetMetadata(MetadataNamespace, MetadataKeyRejectReason, ErrUnknownKey)
				p.Resolve(Decision{Endpoint: endpoint, Err: ErrUnknownKey})
				send.Error(w, http.StatusUnauthorized, send.AuthenticationError, ErrUnknownKey, "unknown or missing API key")
				return
			}
			KeyBlobKey.Set(w, kb)
			KeyIDKey.Set(w, keyID)
			w.SetMetadata(MetadataNamespace, MetadataKeyAttributionWorkspace, kb.Workspace)
			w.SetMetadata(MetadataNamespace, MetadataKeyAttributionUser, kb.User)
			w.SetMetadata(MetadataNamespace, MetadataKeyAttributionKeyID, keyID)
		}

		w.Slog().Info("Received headers", "authority_in", r.Host, "endpoint", endpoint)
	}
}

func listModels(w *up.Writer, _ *up.Request) {
	if err := send.JSON(w, http.StatusOK, config.Get().OpenAIV1Models()); err != nil {
		w.Slog().Error("Failed to marshal model list", "err", err)
		send.Error(w, http.StatusInternalServerError, send.InternalServerError, "orange.internal_error", "failed to encode model list")
	}
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

	cfg := config.Get()

	// Resolve the model entry: prefer per-key blob, fall back to global models.
	var entry *config.ModelEntry
	if kb, ok := KeyBlobKey.Get(w); ok {
		if e, found := kb.LLM.Models[model]; found {
			entry = &e
		}
	}
	if entry == nil {
		if e, ok := cfg.Models[model]; ok {
			entry = &e
		}
	}
	if entry == nil {
		w.Slog().Warn("Received body unknown model", "model", model)
		p.Resolve(Decision{Endpoint: endpoint, Err: ErrUnknownModel})
		send.Errorf(w, http.StatusNotFound, send.InvalidRequestError, ErrUnknownModel, "no upstream configured for model %s", model)
		return
	}

	var upstream, backendModel, binding string
	var fallbacks []Target

	if entry.Routing != nil && entry.Routing.Chain != nil {
		// Chain routing: first child is the primary; the rest become fallbacks.
		children := entry.Routing.Chain.Children
		if len(children) > 0 && children[0].Target != nil {
			t := children[0].Target
			upstream = t.Provider
			backendModel = t.Name
			if backendModel == "" {
				backendModel = model
			}
		}
		for _, child := range children[1:] {
			if child.Target == nil {
				continue
			}
			t := child.Target
			bm := t.Name
			if bm == "" {
				bm = model
			}
			pk := ""
			if prov, ok := cfg.Providers[t.Provider]; ok {
				pk = prov.Kind
			}
			fallbacks = append(fallbacks, Target{
				ProviderBackend: t.Provider,
				BackendModel:    bm,
				ProviderKind:    pk,
			})
		}
	} else {
		// Sugar path: direct provider reference.
		upstream = entry.Provider
		backendModel = entry.Name
		if backendModel == "" {
			backendModel = model
		}
		binding = entry.Binding
		// Apply endpoint discriminator override.
		if endpoint != "" {
			if override, has := entry.Endpoints[endpoint]; has {
				upstream = override
			}
		}
	}

	if upstream == "" {
		w.Slog().Warn("Received body unknown model", "model", model)
		p.Resolve(Decision{Endpoint: endpoint, Err: ErrUnknownModel})
		send.Errorf(w, http.StatusNotFound, send.InvalidRequestError, ErrUnknownModel, "no upstream configured for model %s", model)
		return
	}
	provider := cfg.Providers[upstream]
	// Rewrite :authority to the binding's endpoint host so the upstream sees
	// the right Host header. SNI is driven by the selected host's configured
	// hostname. BindingHost falls back to Provider.Host() when binding is empty.
	w.SetRequestHeader(up.HeaderAuthority, provider.BindingHost(binding))

	d := Decision{ProviderBackend: upstream, ProviderKind: provider.Kind, Model: model, BackendModel: backendModel, Endpoint: endpoint, Binding: binding, Fallbacks: fallbacks}
	d.Apply(w)

	if endpoint == EndpointImages {
		if size := gjson.GetBytes(chunk.Data, "size").String(); size != "" {
			w.SetMetadata(MetadataNamespace, MetadataKeyImageSize, size)
		}
		if quality := gjson.GetBytes(chunk.Data, "quality").String(); quality != "" {
			w.SetMetadata(MetadataNamespace, MetadataKeyImageQuality, quality)
		}
	}

	w.Slog().Info("Received body resolved", "model", model, "backend_model", backendModel, "provider", upstream, "host", provider.BindingHost(binding), "kind", provider.Kind, "endpoint", endpoint, "binding", binding)
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

// ResolveKey parses the Authorization bearer token from authHeader and looks up
// the corresponding KeyBlob in the current config snapshot. Returns the blob,
// key id, and true when found. Returns nil, "", false when key-mode is not
// active (legacy config with no keys[]) or when the key is absent.
func ResolveKey(authHeader string) (*config.KeyBlob, string, bool) {
	return resolveKey(authHeader, config.Get())
}

// resolveKey is the internal implementation shared by ResolveKey and
// tagRequestForEndpoint (which already holds a cfg snapshot).
func resolveKey(authHeader string, cfg *config.Config) (*config.KeyBlob, string, bool) {
	if !cfg.HasKeys() {
		return nil, "", false
	}
	keyID := parseBearerToken(authHeader)
	if keyID == "" {
		return nil, "", false
	}
	kb, ok := cfg.LookupKey(keyID)
	return kb, keyID, ok
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
