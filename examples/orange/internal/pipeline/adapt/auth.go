package adapt

import (
	"github.com/dio/transit/up"
)

// backendAuthHandler injects provider credentials into an outgoing request.
type backendAuthHandler interface {
	InjectAuth(w *up.Writer)
}

// BodyAwareAuthHandler is implemented by auth handlers that need the final
// translated body to compute their credentials (e.g., AWS SigV4).
// Called in the RequestBody phase, after the translator has produced the
// final body and :path.
type BodyAwareAuthHandler interface {
	InjectAuthWithBody(w *up.Writer, req SigningRequest) error
}

// SigningRequest carries the data a body-aware auth handler needs to sign.
type SigningRequest struct {
	Method string // always "POST" for LLM inference APIs
	Path   string // final upstream :path after translator rewrite
	Host   string // upstream hostname (no scheme)
	Body   []byte // final translated body; nil means original passes through
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
