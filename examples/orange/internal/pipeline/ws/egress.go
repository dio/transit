package ws

import (
	"net/http"

	"github.com/dio/transit/examples/orange/internal/config"
	"github.com/dio/transit/examples/orange/internal/pipeline/match"
	"github.com/dio/transit/examples/orange/internal/send"
	"github.com/dio/transit/up"
)

const EgressFilterName = "orange-ws-egress-match"

// internalHeaders is the complete list of x-orange-ws-* headers that must be
// stripped before routing to any provider.
var internalHeaders = [4]string{
	headerProvider,
	headerKind,
	headerModel,
	headerBackendModel,
}

func init() {
	up.Register(EgressFilterName, egressHandler)
}

// egressHandler is the orange-ws-egress-match downstream filter handler.
//
// It runs on the Envoy egress listener before the router. Its responsibilities:
//   - Validate the four x-orange-ws-* headers written by the orange-ws sidecar.
//   - Cross-check them against the active orange.yaml snapshot.
//   - Write the same Decision filter state and dynamic metadata as the HTTP match path.
//   - Strip all x-orange-ws-* headers unconditionally before the upstream route.
//
// Returns a 400 local reply on missing or config-inconsistent headers.
func egressHandler(w *up.Writer, r *up.Request) {
	provider := r.Header(headerProvider)
	kind := r.Header(headerKind)
	model := r.Header(headerModel)
	backendModel := r.Header(headerBackendModel)

	// Strip headers unconditionally — whether we send a local reply or proceed,
	// these must never reach a provider.
	for _, h := range internalHeaders {
		w.RemoveRequestHeader(h)
	}

	if provider == "" || kind == "" || model == "" || backendModel == "" {
		send.Error(w, http.StatusBadRequest, send.InvalidRequestError,
			"orange.ws_headers_missing",
			"orange-ws internal headers are missing; request did not originate from orange-ws sidecar")
		return
	}

	// Cross-check headers against the active config snapshot.
	// Headers are hints, not a trust boundary — they must still match active config.
	cfg := config.Get()
	configProvider, providerConfig, ok := cfg.LookupModelProvider(model)
	if !ok {
		send.Errorf(w, http.StatusBadRequest, send.InvalidRequestError,
			"orange.ws_model_not_found",
			"model %q not found in active orange config", model)
		return
	}
	if configProvider != provider {
		send.Errorf(w, http.StatusBadRequest, send.InvalidRequestError,
			"orange.ws_headers_inconsistent",
			"x-orange-ws-provider %q does not match active config provider %q for model %q",
			provider, configProvider, model)
		return
	}
	if providerConfig.Kind != kind {
		send.Errorf(w, http.StatusBadRequest, send.InvalidRequestError,
			"orange.ws_headers_inconsistent",
			"x-orange-ws-kind %q does not match active config kind %q for provider %q",
			kind, providerConfig.Kind, provider)
		return
	}

	// Write the same Decision filter state and dynamic metadata as the HTTP match path.
	d := match.Decision{
		Provider:     provider,
		Kind:         kind,
		Model:        model,
		BackendModel: backendModel,
	}
	d.Apply(w)

	// Rewrite :authority to the provider host so the upstream sees the right Host header.
	// This mirrors what the HTTP match path does in bodyHandler.
	w.SetRequestHeader(up.HeaderAuthority, providerConfig.Host())
}
