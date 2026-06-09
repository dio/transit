// Package adapt is the upstream HTTP filter that adapts each request to its
// target provider through a 4-phase pipeline.
//
// # Translator flow
//
// For each request a [translator.Translator] instance is created in the
// request-headers phase, stored in [streamContext], and driven through all four
// Envoy extproc phases:
//
//  1. Request-headers — [translator.Translator.RequestHeaders] records metadata
//     (e.g. streaming flag) and returns headers to add to the upstream request.
//  2. Request-body — [translator.Translator.RequestBody] translates the full
//     buffered body to the backend wire format and sets :path (which embeds the
//     model name for GCP/Bedrock paths) plus content-type/content-length.
//     AWS SigV4 signing happens here too, after the final body is known.
//  3. Response-headers — [translator.Translator.ResponseHeaders] rewrites
//     content-type when the streaming framing changes (e.g. EventStream → SSE).
//  4. Response-body — [translator.Translator.ResponseBody] called once per chunk;
//     the translator buffers partial frames internally and emits translated SSE.
//
// Auth is separate: [backendAuthHandler.InjectAuth] runs at the end of phase 1
// (static credentials). [BodyAwareAuthHandler.InjectAuthWithBody] runs at the
// end of phase 2 for handlers that need the final body hash (AWS SigV4).
//
// # Config contract
//
// adapt reads three pieces of per-request state from dynamic metadata written
// by the match filter:
//
//   - orange.upstream     — provider name; selects the ProviderConfig
//   - orange.backend_model — resolved backend model name (from models[].name)
//
// The strip-request-headers list is a compile-time constant; see [stripRequestHeaders].
package adapt

import (
	"context"
	"encoding/json"
	"strconv"
	"strings"

	"github.com/dio/transit/examples/orange/internal/config"
	"github.com/dio/transit/examples/orange/internal/pipeline/match"
	"github.com/dio/transit/examples/orange/internal/translator"
	"github.com/dio/transit/up"
	"github.com/dio/transit/up/compress"
)

const stateAttempt = match.StateAttempt

const (
	FilterName = "orange-adapt"

	// orangeAPIKeyHeader is the orange-internal gateway auth header sent by
	// clients to signal passthrough mode. Its presence tells orange to forward
	// the client's own Anthropic credentials (authorization / x-api-key) to the
	// upstream rather than injecting orange's own API key. The header is always
	// stripped before the request leaves orange — the upstream never sees it.
	orangeAPIKeyHeader = "x-orange-api-key"
)

// adaptAppState is the new-system config source. When non-nil, handler uses
// adaptAppState.Snapshot() instead of the legacy config.Get().
// Set via SetAppState before Envoy initialises the filter.
var (
	adaptAppState    *config.AppState
	adaptSecResolver config.SecretResolver
)

// SetAppState configures the new-system AppState and SecretResolver for the
// adapt filter. Call before Envoy initialises the filter.
func SetAppState(s *config.AppState, r config.SecretResolver) {
	adaptAppState = s
	adaptSecResolver = r
}

func init() {
	up.Register(FilterName, handler,
		up.WithMutableBody(bodyHandler),
		up.WithResponse(responseHandler),
	)
}

// streamContext holds per-request state shared across all four filter phases.
type streamContext struct {
	translator       translator.Translator
	auth             backendAuthHandler
	upstreamHost     string
	rawQuery         string // query string from the original client path (e.g. "beta=true")
	passthroughMode  bool   // true when x-orange-api-key was present; client credentials forwarded
	requestMutations *config.RequestMutations
}

func handler(w *up.Writer, r *up.Request) {
	name, ok := w.GetMetadataString(up.MetadataSourceDynamic, match.MetadataNamespace, match.MetadataKeyUpstream)
	if !ok {
		w.Slog().Info("No upstream metadata", "authority", r.Host)
		return
	}
	upstream := name.String()
	if upstream == "" {
		return
	}

	// Determine attempt number. Filter state persists across retries; we write
	// attempt+1 back each time handler is called so the next retry sees it.
	attempt := 0
	if v, ok := w.GetFilterState(stateAttempt); ok && v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			attempt = n
		}
	}
	w.SetFilterState(stateAttempt, strconv.Itoa(attempt+1))

	backendModel := ""
	if bm, ok := w.GetMetadataString(up.MetadataSourceDynamic, match.MetadataNamespace, match.MetadataKeyBackendModel); ok {
		backendModel = bm.String()
	}

	// On retry, advance to the next chain target: update upstream + backendModel
	// from the fallbacks list written by match.Apply, and update the access-log
	// metadata so provider_backend reflects the provider that actually handled the request.
	// Index is clamped to the last fallback so extra Envoy retries beyond the chain
	// length keep routing to the last provider instead of silently reverting to primary.
	if attempt > 0 {
		if fbJSON, ok := w.GetFilterState(match.StateFallbacks); ok && fbJSON != "" {
			var fallbacks []match.Target
			if err := json.Unmarshal([]byte(fbJSON), &fallbacks); err == nil && len(fallbacks) > 0 {
				idx := attempt - 1
				if idx >= len(fallbacks) {
					idx = len(fallbacks) - 1
				}
				t := fallbacks[idx]
				upstream = t.ProviderBackend
				backendModel = t.BackendModel
				w.SetMetadata(match.MetadataNamespace, match.MetadataKeyUpstream, upstream)
				if backendModel != "" {
					w.SetMetadata(match.MetadataNamespace, match.MetadataKeyBackendModel, backendModel)
				}
			}
		}
	}

	// Read the binding set by match for the primary. Fallback targets always use
	// the "default" binding (chain targets name only a provider + model).
	binding := ""
	if attempt == 0 {
		binding, _ = w.GetFilterState(match.StateBinding)
	}

	// Use new AppState when available; fall back to legacy singleton.
	var (
		host     string
		endpoint string
		schema   string
		secret   string
	)
	if adaptAppState != nil {
		cfgSnap := adaptAppState.Snapshot()
		if cfgSnap == nil || cfgSnap.Global == nil {
			return
		}
		provRec, ok := cfgSnap.Global.Providers[upstream]
		if !ok {
			return
		}
		host = provRec.BindingHost(binding)
		w.SetRequestHeader(up.HeaderAuthority, host)
		w.Slog().Info("Adapting request", "provider", upstream, "attempt", attempt, "authority", host, "kind", provRec.Kind, "accept-encoding", r.Header("accept-encoding"))
		if ev, ok := w.GetMetadataString(up.MetadataSourceDynamic, match.MetadataNamespace, match.MetadataKeyEndpoint); ok {
			endpoint = ev.String()
		}
		schema = provRec.EffectiveBackendSchema()
		t, err := translator.NewForRoute(schema, endpoint, translatorCfgRec(provRec, backendModel, adaptSecResolver))
		if err != nil {
			w.Slog().Info("No translator for schema/endpoint, skipping", "schema", schema, "endpoint", endpoint)
			return
		}
		if adaptSecResolver != nil {
			secret, _ = adaptSecResolver.Resolve(context.Background(), provRec.Auth.SecretRef)
		}
		authHandler, err := getOrCreateAuthHandlerRec(upstream, provRec, secret)
		if err != nil {
			w.Slog().Info("Auth handler error", "provider", upstream, "err", err)
			authHandler = noAuth{}
		}
		// Look up request mutations from the model record (client-facing model ID
		// is written into metadata by the match filter).
		var reqMut *config.RequestMutations
		if mv, ok := w.GetMetadataString(up.MetadataSourceDynamic, match.MetadataNamespace, match.MetadataKeyModel); ok {
			if modelRec, found := cfgSnap.Global.Models[mv.String()]; found {
				reqMut = modelRec.RequestMutations
			}
		}

		passthroughMode := r.Header(orangeAPIKeyHeader) != ""
		_, rawQuery, _ := strings.Cut(r.Path, "?")
		sc := &streamContext{
			translator:       t,
			auth:             authHandler,
			upstreamHost:     host,
			rawQuery:         rawQuery,
			passthroughMode:  passthroughMode,
			requestMutations: reqMut,
		}
		if r.Context != nil {
			*r.Context = sc
		}
		if passthroughMode {
			w.RemoveRequestHeader(orangeAPIKeyHeader)
			w.SetMetadata(match.MetadataNamespace, "passthrough", "true")
		} else {
			w.RemoveRequestHeader("authorization")
			w.RemoveRequestHeader("x-api-key")
			w.RemoveRequestHeader("anthropic-version")
		}
		if !compress.AcceptEncodingAllSupported(r.Header("accept-encoding")) {
			w.SetRequestHeader("accept-encoding", "identity")
		}
		hdrs, err := t.RequestHeaders(allRequestHeaders(r))
		if err != nil {
			w.Slog().Info("RequestHeaders error", "err", err)
		}
		applyRequestHeaders(w, hdrs)
		if !passthroughMode {
			authHandler.InjectAuth(w)
		}
		// Apply header mutations after auth so they can override or extend auth-set headers.
		if reqMut != nil && len(reqMut.Headers) > 0 {
			for k, v := range resolveExtra(adaptSecResolver, reqMut.Headers) {
				w.SetRequestHeader(k, v)
			}
		}
	}
}

func bodyHandler(w *up.Writer, chunk *up.BodyChunk) {
	if chunk.Context == nil {
		return
	}
	sc, ok := (*chunk.Context).(*streamContext)
	if !ok {
		return
	}

	// For SSE streaming requests, force identity encoding so the upstream does
	// not compress the response. Incremental gzip decompression across chunks
	// is not supported; compressed non-streaming bodies are decoded in full.
	var streamReq struct {
		Stream bool `json:"stream"`
	}
	if json.Unmarshal(chunk.Data, &streamReq) == nil && streamReq.Stream {
		w.SetRequestHeader("accept-encoding", "identity")
	}

	newHdrs, mutated, err := sc.translator.RequestBody(chunk.Data)
	if err != nil {
		return
	}
	// Re-append the original query string to any :path the translator sets.
	// Translators write a clean path (e.g. /v1/messages); clients like Claude
	// Code add query params (e.g. ?beta=true) that must reach the upstream.
	if sc.rawQuery != "" {
		for i := range newHdrs {
			if newHdrs[i].Name == ":path" && !strings.Contains(newHdrs[i].Value, "?") {
				newHdrs[i].Value += "?" + sc.rawQuery
			}
		}
	}
	applyRequestHeaders(w, newHdrs)

	effectiveBody := chunk.Data
	bodyModified := false
	if mutated != nil {
		effectiveBody = mutated
		bodyModified = true
	}
	// Apply body mutations after translation so the translator cannot overwrite them.
	if sc.requestMutations != nil && len(sc.requestMutations.Body) > 0 {
		resolved := resolveExtra(adaptSecResolver, sc.requestMutations.Body)
		if mb := applyBodyMutations(effectiveBody, resolved); mb != nil {
			effectiveBody = mb
			bodyModified = true
			w.SetRequestHeader("content-length", strconv.Itoa(len(effectiveBody)))
		}
	}
	if !sc.passthroughMode {
		if baw, ok := sc.auth.(BodyAwareAuthHandler); ok {
			req := SigningRequest{
				Method: "POST",
				Path:   pathFromHeaders(newHdrs),
				Host:   sc.upstreamHost,
				Body:   effectiveBody,
			}
			if err := baw.InjectAuthWithBody(w, req); err != nil {
				w.Slog().Info("InjectAuthWithBody error", "err", err)
			}
		}
	}

	if bodyModified {
		w.SetRequestBody(effectiveBody)
	}
}

func responseHandler(w *up.Writer, chunk *up.ResponseChunk) {
	if chunk.Context == nil {
		return
	}
	sc, ok := (*chunk.Context).(*streamContext)
	if !ok {
		return
	}
	t := sc.translator
	if chunk.StatusCode != 0 {
		newHdrs, err := t.ResponseHeaders(allResponseHeaders(chunk))
		if err != nil {
			return
		}
		applyResponseHeaders(w, newHdrs)
	} else {
		// Streaming requests force accept-encoding: identity (see bodyHandler),
		// so this branch is only reached for non-streaming JSON responses.
		// Decode before translation so the translator and token counter see raw
		// bytes, then re-encode with the same scheme so the client receives what
		// it negotiated (content-encoding is preserved) and content-length
		// reflects the re-compressed size. q-values in the client's
		// Accept-Encoding are not re-checked here: the upstream is trusted to
		// have honored them during negotiation.
		enc := chunk.ContentEncoding
		data := chunk.Data
		if enc != "" && enc != "identity" {
			decoded, err := compress.Decode(enc, data)
			if err != nil {
				w.Slog().Info("decompress response body error", "encoding", enc, "err", err)
				return
			}
			data = decoded
		}
		newHdrs, out, err := t.ResponseBody(data, chunk.EndStream)
		if err != nil {
			return
		}
		applyResponseHeaders(w, newHdrs)
		if out != nil {
			if enc != "" && enc != "identity" {
				reenc, err := compress.Encode(enc, out)
				if err != nil {
					w.Slog().Info("recompress response body error", "encoding", enc, "err", err)
					w.RemoveResponseHeader("content-encoding")
					w.SetResponseBody(out)
					return
				}
				w.SetResponseHeader("content-length", strconv.Itoa(len(reenc)))
				w.SetResponseBody(reenc)
			} else {
				w.SetResponseBody(out)
			}
		}
	}
}

func allRequestHeaders(r *up.Request) map[string]string {
	pairs := r.AllHeaders()
	m := make(map[string]string, len(pairs))
	for _, p := range pairs {
		m[p[0]] = p[1]
	}
	return m
}

func allResponseHeaders(chunk *up.ResponseChunk) map[string]string {
	pairs := chunk.AllHeaders()
	m := make(map[string]string, len(pairs))
	for _, p := range pairs {
		m[p[0]] = p[1]
	}
	return m
}

func applyRequestHeaders(w *up.Writer, hdrs []translator.Header) {
	for _, h := range hdrs {
		w.SetRequestHeader(h.Name, h.Value)
	}
}

func applyResponseHeaders(w *up.Writer, hdrs []translator.Header) {
	for _, h := range hdrs {
		w.SetResponseHeader(h.Name, h.Value)
	}
}

func pathFromHeaders(hdrs []translator.Header) string {
	for _, h := range hdrs {
		if h.Name == ":path" {
			return h.Value
		}
	}
	return ""
}

// translatorCfgRec builds a [translator.ProviderConfig] from a ProviderRecord.
// backendModel is empty when the model entry has no name override, in which
// case translators forward the client-supplied model name unchanged.
// resolver is used to expand env://, file://, literal:// refs in Extra values.
func translatorCfgRec(p *config.ProviderRecord, backendModel string, resolver config.SecretResolver) translator.ProviderConfig {
	return translator.ProviderConfig{
		BackendSchema: p.EffectiveBackendSchema(),
		PathPrefix:    p.ResolvedPathPrefix(),
		BackendModel:  backendModel,
		Extra:         resolveExtra(resolver, p.Extra),
	}
}

// resolveExtra returns a copy of extra with every value resolved through resolver.
// Unresolvable values are left as-is so a missing env var surfaces as a bad path
// rather than a panic.
func resolveExtra(resolver config.SecretResolver, extra map[string]string) map[string]string {
	if resolver == nil || len(extra) == 0 {
		return extra
	}
	out := make(map[string]string, len(extra))
	for k, v := range extra {
		if resolved, err := resolver.Resolve(context.Background(), v); err == nil {
			out[k] = resolved
		} else {
			out[k] = v
		}
	}
	return out
}

// applyBodyMutations merges mutations into the JSON body. Keys support
// dot-path notation (e.g. "modelInfo.id") to address nested fields;
// intermediate objects are created as needed. Returns nil on parse failure so
// callers can fall back to the original body.
func applyBodyMutations(body []byte, mutations map[string]string) []byte {
	var obj map[string]any
	if err := json.Unmarshal(body, &obj); err != nil {
		return nil
	}
	for path, value := range mutations {
		setDotPath(obj, path, value)
	}
	out, err := json.Marshal(obj)
	if err != nil {
		return nil
	}
	return out
}

// setDotPath sets the value at a dot-separated path inside m, creating
// intermediate map[string]any nodes as needed.
func setDotPath(m map[string]any, path, value string) {
	key, rest, hasDot := strings.Cut(path, ".")
	if !hasDot {
		m[key] = value
		return
	}
	sub, _ := m[key].(map[string]any)
	if sub == nil {
		sub = make(map[string]any)
	}
	setDotPath(sub, rest, value)
	m[key] = sub
}
