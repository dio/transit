package translator

import "path"

// anthropicPassthrough is a no-op translator for Anthropic→Anthropic routing.
// The client already speaks the Anthropic Messages API; only the :path needs
// to be set (incorporating the configured path prefix). Auth is injected
// separately by adapt.AnthropicAuth.
type anthropicPassthrough struct {
	messagesPath string
}

func (a *anthropicPassthrough) RequestHeaders(_ map[string]string) ([]Header, error) {
	return nil, nil
}

func (a *anthropicPassthrough) RequestBody(raw []byte) ([]Header, []byte, error) {
	return []Header{{pathHeaderName, a.messagesPath}}, nil, nil
}

func (a *anthropicPassthrough) ResponseHeaders(_ map[string]string) ([]Header, error) {
	return nil, nil
}

func (a *anthropicPassthrough) ResponseBody(_ []byte, _ bool) ([]Header, []byte, error) {
	return nil, nil, nil
}

func init() {
	Register("anthropic", func(cfg ProviderConfig) Translator {
		return &anthropicPassthrough{
			messagesPath: path.Join("/", cfg.PathPrefix, "messages"),
		}
	})
	// count_tokens shares the same no-op body translation; only the path differs.
	Register("anthropic:count_tokens", func(cfg ProviderConfig) Translator {
		return &anthropicPassthrough{
			messagesPath: path.Join("/", cfg.PathPrefix, "messages", "count_tokens"),
		}
	})
}
