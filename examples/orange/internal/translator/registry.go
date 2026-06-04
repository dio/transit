package translator

import "fmt"

// Factory constructs a new Translator for a single request.
// cfg is read-only; the returned Translator may keep mutable streaming state.
type Factory func(cfg ProviderConfig) Translator

var registry = map[string]Factory{}

// Register associates name with factory f. Called from provider init() functions.
func Register(name string, f Factory) {
	if _, dup := registry[name]; dup {
		panic("translate: duplicate provider registration: " + name)
	}
	registry[name] = f
}

// New returns a fresh Translator for the named provider.
func New(name string, cfg ProviderConfig) (Translator, error) {
	f, ok := registry[name]
	if !ok {
		return nil, fmt.Errorf("translate: unknown provider %q", name)
	}
	return f(cfg), nil
}

// NewForRoute returns a fresh Translator for the given backend schema and
// endpoint discriminator. It tries the combined "schema:endpoint" key first
// (e.g. "openai:responses"), then falls back to the schema-only key. This lets
// endpoint-specific translators coexist with the existing schema-keyed ones
// without touching existing registrations.
func NewForRoute(schema, endpoint string, cfg ProviderConfig) (Translator, error) {
	if endpoint != "" {
		key := schema + ":" + endpoint
		if f, ok := registry[key]; ok {
			return f(cfg), nil
		}
	}
	return New(schema, cfg)
}
