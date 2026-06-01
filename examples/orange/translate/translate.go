// Package translate is the upstream HTTP filter that adapts each request to
// its target provider:
//
//   - strips client-supplied auth headers (config.translate.strip_request_headers),
//   - rewrites :path when the provider's path_prefix differs from /v1,
//   - injects the configured provider credential.
//
// It reads the chosen upstream name from dynamic metadata written by classify
// (`orange.upstream`).
package translate

import (
	"strings"

	"github.com/dio/transit/examples/orange/classify"
	"github.com/dio/transit/examples/orange/config"
	"github.com/dio/transit/up"
)

const FilterName = "orange-translate"

const (
	kindOpenAI    = "openai"
	kindAnthropic = "anthropic"
)

func init() {
	up.Register(FilterName, handler)
}

func handler(w *up.Writer, r *up.Request) {
	name, ok := w.GetMetadataString(up.MetadataSourceDynamic, classify.MetadataNamespace, classify.MetadataKeyUpstream)
	if !ok {
		w.Log(up.LogInfo, "orange-translate: no upstream metadata (authority=%s)", r.Host)
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
	w.Log(up.LogInfo, "orange-translate: provider=%s authority=%s kind=%s", upstream, r.Host, prov.Kind)

	for _, h := range cfg.Translate.StripRequestHeaders {
		w.RemoveRequestHeader(h)
	}

	if prefix := prov.ResolvedPathPrefix(); prefix != "/v1" {
		w.SetRequestHeader(":path", prefix+strings.TrimPrefix(r.Path, "/v1"))
	}

	secret := cfg.ProviderSecret(upstream)
	switch prov.Kind {
	case kindOpenAI:
		if secret != "" {
			w.SetRequestHeader("authorization", "Bearer "+secret)
		}
	case kindAnthropic:
		// Anthropic auth is x-api-key (not Authorization: Bearer) plus a required
		// anthropic-version header. Both are configured per-provider so different
		// keys / API revisions can coexist.
		if secret != "" {
			w.SetRequestHeader("x-api-key", secret)
		}
		if prov.AnthropicVersion != "" {
			w.SetRequestHeader("anthropic-version", prov.AnthropicVersion)
		}
	}
}
