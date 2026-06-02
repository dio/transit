package translate

import (
	"strings"

	"github.com/dio/transit/up"
)

// APISchemaName identifies the OpenAPI request/response shape a client or
// upstream speaks. Used by RouteFor to select translator and auth.
type APISchemaName string

const (
	SchemaOpenAI    APISchemaName = "openai"
	SchemaAnthropic APISchemaName = "anthropic"
)

// ProviderConfig carries the upstream-specific settings that RouteFor needs to
// build a Route. Callers (e.g. orange/translate) populate it from their own
// config types; up/translate has no dependency on orange/config.
type ProviderConfig struct {
	Schema     APISchemaName
	PathPrefix string // upstream path prefix; "" or "/v1" means no rewrite
	Secret     string // resolved credential (Bearer token or API key)
	// Extra is schema-specific extra. For SchemaAnthropic it is the
	// anthropic-version header value.
	Extra string
}

// Route is a value that carries the translation decisions for one
// (client schema, upstream) pair. It is created once per request by RouteFor
// and applied by Apply.
type Route struct {
	auth       BackendAuthHandler
	pathPrefix string // non-empty and != "/v1" → rewrite
}

// Apply strips client-auth headers listed in strip, rewrites :path when needed,
// then injects upstream credentials.
func (ro Route) Apply(w *up.Writer, r *up.Request, strip []string) {
	for _, h := range strip {
		w.RemoveRequestHeader(h)
	}
	if ro.pathPrefix != "" && ro.pathPrefix != "/v1" {
		w.SetRequestHeader(":path", ro.pathPrefix+strings.TrimPrefix(r.Path, "/v1"))
	}
	ro.auth.InjectAuth(w)
}

// RouteFor returns the Route for a given client schema talking to a given
// upstream ProviderConfig. clientSchema is currently always SchemaOpenAI for
// orange; the switch is here to make future client-schema expansion explicit.
func RouteFor(clientSchema APISchemaName, p ProviderConfig) Route {
	switch clientSchema {
	case SchemaOpenAI:
		return routeOpenAIClient(p)
	default:
		return Route{auth: NoAuth{}}
	}
}

func routeOpenAIClient(p ProviderConfig) Route {
	var auth BackendAuthHandler
	switch p.Schema {
	case SchemaOpenAI:
		auth = BearerAuth{Token: p.Secret}
	case SchemaAnthropic:
		auth = AnthropicAuth{APIKey: p.Secret, Version: p.Extra}
	default:
		auth = NoAuth{}
	}
	return Route{auth: auth, pathPrefix: p.PathPrefix}
}
