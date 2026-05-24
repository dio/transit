package wsproxy

import (
	"os"
	"strings"

	"github.com/dio/transit/up"
)

const AuthExtensionName = "ws-auth"

// AuthConfig holds credentials for the upstream cluster.
// Loaded once at filter config time from environment variables.
type AuthConfig struct {
	// StripHeaders are request headers removed before forwarding upstream.
	StripHeaders []string

	// InjectHeaders are headers added to the upstream request.
	// Values support ${ENV_VAR} expansion.
	InjectHeaders map[string]string
}

// DefaultAuthConfig returns the default config reading from well-known env vars.
func DefaultAuthConfig() AuthConfig {
	return AuthConfig{
		StripHeaders: []string{"authorization", "x-api-key"},
		InjectHeaders: map[string]string{
			"authorization": "Bearer ${OPENAI_API_KEY}",
		},
	}
}

// resolveValue expands a single ${ENV_VAR} reference.
func resolveValue(v string) string {
	if !strings.HasPrefix(v, "${") || !strings.HasSuffix(v, "}") {
		return v
	}
	name := v[2 : len(v)-1]
	if val := os.Getenv(name); val != "" {
		return val
	}
	return v // return unexpanded if not set
}

func init() {
	// ws-auth is an upstream HTTP filter: it runs on the connection to the
	// upstream cluster, not on the downstream request. This is the correct
	// placement to strip client credentials and inject provider credentials.
	//
	// In envoy.yaml it is wired under typed_extension_protocol_options on
	// the upstream cluster, not in the listener's http_filters chain.
	up.Register(AuthExtensionName, func(w *up.Writer, r *up.Request) {
		cfg := DefaultAuthConfig()

		for _, h := range cfg.StripHeaders {
			w.SetRequestHeader(h, "")
		}

		for k, v := range cfg.InjectHeaders {
			resolved := resolveValue(v)
			if resolved != v { // only inject if env var was found
				w.SetRequestHeader(k, resolved)
			}
		}
	})
}
