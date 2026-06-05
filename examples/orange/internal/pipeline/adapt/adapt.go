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
	"encoding/json"
	"strconv"
	"strings"

	"github.com/dio/transit/examples/orange/internal/config"
	"github.com/dio/transit/examples/orange/internal/pipeline/match"
	"github.com/dio/transit/examples/orange/internal/translator"
	"github.com/dio/transit/up"
	"github.com/dio/transit/up/compress"
)

const FilterName = "orange-adapt"

// stripRequestHeaders is the fixed set of client auth headers removed before
// the translator and credential handlers inject their own values. Deployment-
// invariant: every orange instance strips the same headers. See the config
// ergonomics doc for the rationale for keeping this out of config.
var stripRequestHeaders = []string{"authorization", "x-api-key", "anthropic-version"}

func init() {
	up.Register(FilterName, handler,
		up.WithMutableBody(bodyHandler),
		up.WithResponse(responseHandler),
	)
}

// streamContext holds per-request state shared across all four filter phases.
type streamContext struct {
	translator   translator.Translator
	auth         backendAuthHandler
	upstreamHost string
	rawQuery     string // query string from the original client path (e.g. "beta=true")
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
	cfg := config.Get()
	prov, ok := cfg.Providers[upstream]
	if !ok {
		return
	}
	w.Slog().Info("Adapting request", "provider", upstream, "authority", r.Host, "kind", prov.Kind)

	backendModel := ""
	if bm, ok := w.GetMetadataString(up.MetadataSourceDynamic, match.MetadataNamespace, match.MetadataKeyBackendModel); ok {
		backendModel = bm.String()
	}

	endpoint := ""
	if ep, ok := w.GetMetadataString(up.MetadataSourceDynamic, match.MetadataNamespace, match.MetadataKeyEndpoint); ok {
		endpoint = ep.String()
	}

	schema := prov.EffectiveBackendSchema()
	t, err := translator.NewForRoute(schema, endpoint, translatorCfg(prov, backendModel))
	if err != nil {
		w.Slog().Info("No translator for schema/endpoint, skipping", "schema", schema, "endpoint", endpoint)
		return
	}

	secret := cfg.ProviderSecret(upstream)
	authHandler, err := getOrCreateAuthHandler(upstream, prov, secret)
	if err != nil {
		w.Slog().Info("Auth handler error", "provider", upstream, "err", err)
		authHandler = noAuth{}
	}

	_, rawQuery, _ := strings.Cut(r.Path, "?")
	sc := &streamContext{
		translator:   t,
		auth:         authHandler,
		upstreamHost: prov.Host(),
		rawQuery:     rawQuery,
	}
	if r.Context != nil {
		*r.Context = sc
	}

	// Strip client-auth headers before applying translator headers and credentials.
	for _, h := range stripRequestHeaders {
		w.RemoveRequestHeader(h)
	}
	// Force identity if the client's Accept-Encoding includes an encoding we
	// cannot decode, so the upstream response is always decodable. Streaming
	// requests also force identity (handled in bodyHandler after the stream
	// flag is parsed from the request body).
	if !compress.AcceptEncodingAllSupported(r.Header("accept-encoding")) {
		w.SetRequestHeader("accept-encoding", "identity")
	}

	hdrs, err := t.RequestHeaders(allRequestHeaders(r))
	if err != nil {
		w.Slog().Info("RequestHeaders error", "err", err)
	}
	applyRequestHeaders(w, hdrs)
	// Static handlers (Bearer, APIKey, Anthropic) inject here.
	// AWSAuth.InjectAuth is a no-op; it signs in bodyHandler.
	authHandler.InjectAuth(w)
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
	if mutated != nil {
		effectiveBody = mutated
	}
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

	if mutated != nil {
		w.SetRequestBody(mutated)
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

// translatorCfg builds a [translator.ProviderConfig] from the provider definition
// and the backend model name resolved by match (from models[].name). backendModel
// is empty when the model entry has no name override, in which case translators
// forward the client-supplied model name unchanged.
func translatorCfg(p config.Provider, backendModel string) translator.ProviderConfig {
	return translator.ProviderConfig{
		BackendSchema: p.EffectiveBackendSchema(),
		PathPrefix:    p.ResolvedPathPrefix(),
		BackendModel:  backendModel,
		Extra:         p.Extra,
	}
}
