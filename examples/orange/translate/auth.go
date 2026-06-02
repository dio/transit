package translate

import "github.com/dio/transit/up"

// backendAuthHandler injects provider credentials into an outgoing request.
type backendAuthHandler interface {
	InjectAuth(w *up.Writer)
}

type noAuth struct{}

func (noAuth) InjectAuth(_ *up.Writer) {}

// BearerAuth sets Authorization: Bearer <Token>.
type BearerAuth struct{ Token string }

func (a BearerAuth) InjectAuth(w *up.Writer) {
	if a.Token != "" {
		w.SetRequestHeader("authorization", "Bearer "+a.Token)
	}
}

// APIKeyAuth sets a custom header to the given key value.
type APIKeyAuth struct {
	Header string
	Key    string
}

func (a APIKeyAuth) InjectAuth(w *up.Writer) {
	if a.Header != "" && a.Key != "" {
		w.SetRequestHeader(a.Header, a.Key)
	}
}

// AnthropicAuth sets x-api-key and anthropic-version, the two headers required
// by the Anthropic Messages API.
type AnthropicAuth struct {
	APIKey  string
	Version string
}

func (a AnthropicAuth) InjectAuth(w *up.Writer) {
	if a.APIKey != "" {
		w.SetRequestHeader("x-api-key", a.APIKey)
	}
	if a.Version != "" {
		w.SetRequestHeader("anthropic-version", a.Version)
	}
}
