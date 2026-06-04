package adapt

import (
	"context"
	"fmt"
	"sync"

	"github.com/dio/transit/examples/orange/internal/config"
	"github.com/dio/transit/up"
)

var (
	handlerCacheMu sync.Mutex
	handlerCache   = map[string]backendAuthHandler{}
)

// clearAuthHandlerCache discards all cached handlers. Called by tests after
// config.MustReload() to prevent stale handlers crossing test boundaries.
func clearAuthHandlerCache() {
	handlerCacheMu.Lock()
	handlerCache = map[string]backendAuthHandler{}
	handlerCacheMu.Unlock()
}

// getOrCreateAuthHandler returns the cached handler for upstreamName,
// constructing it once on first use.
func getOrCreateAuthHandler(upstreamName string, prov config.Provider, secret string) (backendAuthHandler, error) {
	handlerCacheMu.Lock()
	if h, ok := handlerCache[upstreamName]; ok {
		handlerCacheMu.Unlock()
		return h, nil
	}
	handlerCacheMu.Unlock()

	h, err := buildAuthHandler(prov, secret)
	if err != nil {
		return nil, err
	}

	handlerCacheMu.Lock()
	if existing, ok := handlerCache[upstreamName]; ok {
		handlerCacheMu.Unlock()
		return existing, nil
	}
	handlerCache[upstreamName] = h
	handlerCacheMu.Unlock()
	return h, nil
}

// InjectHeaderAuth injects provider credentials that can be computed during
// request headers. It is used by Responses WebSocket egress, where there is no terminal
// request body event for body-aware signers.
func InjectHeaderAuth(w *up.Writer, upstreamName string, prov config.Provider, secret string) error {
	handler, err := getOrCreateAuthHandler(upstreamName, prov, secret)
	if err != nil {
		return err
	}
	if _, ok := handler.(BodyAwareAuthHandler); ok {
		return fmt.Errorf("orange: provider %q uses body-aware auth, which is not supported for Responses WebSocket egress", upstreamName)
	}
	handler.InjectAuth(w)
	return nil
}

// buildAuthHandler constructs the right handler for prov using this priority:
//  1. type: gcp → GCPAuth (secret = SA JSON if secret_ref set, otherwise ADC)
//  2. Secret non-empty → static credential handler
//  3. gcpvertexai / gcpanthropic (no explicit type) → GCPAuth (ADC)
//  4. awsbedrock / awsanthropic  → AWSAuth (SigV4; requires extra.aws_region)
//  5. Otherwise → noAuth{}
func buildAuthHandler(prov config.Provider, secret string) (backendAuthHandler, error) {
	if prov.Auth.Type == "gcp" {
		return NewGCPAuth(context.Background(), secret)
	}
	if secret != "" {
		return staticAuthHandler(prov, secret), nil
	}
	switch prov.EffectiveBackendSchema() {
	case "gcpvertexai", "gcpanthropic":
		return NewGCPAuth(context.Background(), "")
	case "awsbedrock", "awsanthropic":
		region := prov.Extra["aws_region"]
		if region == "" {
			return nil, fmt.Errorf("orange: provider %q requires extra.aws_region", prov.EffectiveBackendSchema())
		}
		return NewAWSAuth(context.Background(), region)
	}
	return noAuth{}, nil
}

// staticAuthHandler maps Auth.Type to the appropriate static-credential handler.
func staticAuthHandler(prov config.Provider, secret string) backendAuthHandler {
	switch prov.Auth.Type {
	case "bearer":
		return BearerAuth{Token: secret}
	case "x-api-key":
		return APIKeyAuth{Header: "x-api-key", Key: secret}
	case "gemini":
		return APIKeyAuth{Header: "x-goog-api-key", Key: secret}
	case "anthropic":
		return AnthropicAuth{APIKey: secret, Version: prov.Extra["anthropic_version"]}
	default:
		return noAuth{}
	}
}
