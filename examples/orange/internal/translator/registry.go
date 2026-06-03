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
