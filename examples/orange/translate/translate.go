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
	"github.com/dio/transit/examples/orange/classify"
	"github.com/dio/transit/examples/orange/config"
	"github.com/dio/transit/up"
)

const FilterName = "orange-translate"

func init() {
	up.Register(FilterName, handler)
}

// kindSchema maps orange provider kind strings to their APISchemaName.
var kindSchema = map[string]APISchemaName{
	"openai":    SchemaOpenAI,
	"anthropic": SchemaAnthropic,
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

	route := RouteFor(SchemaOpenAI, ProviderConfig{
		Schema:     kindSchema[prov.Kind],
		PathPrefix: prov.ResolvedPathPrefix(),
		Secret:     cfg.ProviderSecret(upstream),
		Extra:      prov.AnthropicVersion,
	})
	route.Apply(w, r, cfg.Translate.StripRequestHeaders)
}
