package adapt

import (
	"context"
	"fmt"
	"sync"

	"github.com/dio/transit/examples/orange/internal/config"
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

// buildAuthHandler constructs the right handler for prov using this priority:
//  1. Secret non-empty → static credential handler
//  2. gcpvertexai / gcpanthropic → GCPAuth (ADC)
//  3. awsbedrock / awsanthropic  → AWSAuth (SigV4; requires extra.aws_region)
//  4. Otherwise → noAuth{}
func buildAuthHandler(prov config.Provider, secret string) (backendAuthHandler, error) {
	if secret != "" {
		return staticAuthHandler(prov, secret), nil
	}
	switch prov.EffectiveBackendSchema() {
	case "gcpvertexai", "gcpanthropic":
		return NewGCPAuth(context.Background())
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
	case "anthropic":
		return AnthropicAuth{APIKey: secret, Version: prov.Extra["anthropic_version"]}
	default:
		return noAuth{}
	}
}
