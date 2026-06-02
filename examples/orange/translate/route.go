package translate

import (
	"strings"

	"github.com/dio/transit/up"
)

// APISchemaName identifies the OpenAPI request/response shape a client or
// upstream speaks.
type APISchemaName string

const (
	SchemaOpenAI    APISchemaName = "openai"
	SchemaAnthropic APISchemaName = "anthropic"
)

// ProviderConfig carries the upstream-specific settings needed to build a Route.
type ProviderConfig struct {
	Schema     APISchemaName
	PathPrefix string // upstream path prefix; "" or "/v1" means no rewrite
	Secret     string // resolved credential (Bearer token or API key)
	// Extra is schema-specific. For SchemaAnthropic it is the anthropic-version header value.
	Extra string
}

// Route carries the translation decisions for one (client schema, upstream) pair.
type Route struct {
	auth       backendAuthHandler
	pathPrefix string
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

// RouteFor returns the Route for a given client schema talking to a given upstream ProviderConfig.
func RouteFor(clientSchema APISchemaName, p ProviderConfig) Route {
	switch clientSchema {
	case SchemaOpenAI:
		return routeOpenAIClient(p)
	default:
		return Route{auth: noAuth{}}
	}
}

func routeOpenAIClient(p ProviderConfig) Route {
	var auth backendAuthHandler
	switch p.Schema {
	case SchemaOpenAI:
		auth = BearerAuth{Token: p.Secret}
	case SchemaAnthropic:
		auth = AnthropicAuth{APIKey: p.Secret, Version: p.Extra}
	default:
		auth = noAuth{}
	}
	return Route{auth: auth, pathPrefix: p.PathPrefix}
}
