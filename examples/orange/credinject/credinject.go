// Package credinject is the upstream HTTP filter that aligns the request with
// the chosen provider:
//
//   - rewrites :authority to the upstream's host (so auto-SNI + the upstream
//     Host header match the provider),
//   - strips client-supplied auth headers (config.credinject.strip_request_headers),
//   - injects the configured provider credential.
//
// It reads the chosen upstream name from dynamic metadata written by classify
// (`orange.upstream`).
package credinject

import (
	"github.com/dio/transit/examples/orange/classify"
	"github.com/dio/transit/examples/orange/config"
	"github.com/dio/transit/up"
)

const FilterName = "orange-credinject"

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
		w.Log(up.LogInfo, "orange-credinject: no upstream metadata (authority=%s)", r.Host)
		return
	}
	upstream := name.String()
	if upstream == "" {
		return
	}
	cfg := config.Get()
	ups, ok := cfg.Upstreams[upstream]
	if !ok {
		return
	}
	w.Log(up.LogInfo, "orange-credinject: upstream=%s authority=%s kind=%s", upstream, r.Host, ups.Kind)

	for _, h := range cfg.Credinject.StripRequestHeaders {
		w.RemoveRequestHeader(h)
	}

	secret := cfg.UpstreamSecret(upstream)
	switch ups.Kind {
	case kindOpenAI:
		if secret != "" {
			w.SetRequestHeader("authorization", "Bearer "+secret)
		}
	case kindAnthropic:
		// Anthropic auth is x-api-key (not Authorization: Bearer) plus a required
		// anthropic-version header. Both are configured per-upstream so different
		// keys / API revisions can coexist.
		if secret != "" {
			w.SetRequestHeader("x-api-key", secret)
		}
		if ups.AnthropicVersion != "" {
			w.SetRequestHeader("anthropic-version", ups.AnthropicVersion)
		}
	}
}
