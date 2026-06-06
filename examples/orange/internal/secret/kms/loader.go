package kms

import (
	"context"
	"fmt"
	"net/url"
	"strings"
	"sync"
)

// Factory is a function that constructs a MasterKEKProvider from a parsed URI.
type Factory func(ctx context.Context, u *url.URL) (MasterKEKProvider, error)

var (
	registryMu sync.RWMutex
	registry   = map[string]Factory{}
)

// Register adds a Factory for the given URI scheme. Panics on duplicate registration.
func Register(scheme string, f Factory) {
	registryMu.Lock()
	defer registryMu.Unlock()
	if _, exists := registry[scheme]; exists {
		panic(fmt.Sprintf("kms: scheme %q already registered", scheme))
	}
	registry[scheme] = f
}

func init() {
	Register("env", func(_ context.Context, u *url.URL) (MasterKEKProvider, error) {
		varName := strings.TrimPrefix(u.String(), "env://")
		if varName == "" {
			return nil, fmt.Errorf("kms: env:// requires a variable name (e.g. env://MASTER_KEK_B64)")
		}
		return FromEnv(varName)
	})
	Register("file", func(ctx context.Context, u *url.URL) (MasterKEKProvider, error) {
		path := u.Host + u.Path
		if path == "" {
			return nil, fmt.Errorf("kms: file:// requires a path (e.g. file:///path/to/key)")
		}
		return FromFile(ctx, path)
	})
}

// Scheme returns the URI scheme (everything before the first colon).
func Scheme(uri string) string {
	if i := strings.Index(uri, ":"); i > 0 {
		return uri[:i]
	}
	return uri
}

// Load creates a MasterKEKProvider from a URI string.
//
// Built-in schemes:
//   - env://VAR_NAME   — read from environment variable at startup
//   - file:///path     — read from a file; watches for live rotation via fsnotify
func Load(ctx context.Context, uri string) (MasterKEKProvider, error) {
	if uri == "" {
		return nil, fmt.Errorf("kms: MASTER_KEK URI is required (e.g. env://MASTER_KEK_B64 or file:///path/to/key)")
	}

	u, err := url.Parse(uri)
	if err != nil {
		return nil, fmt.Errorf("kms: invalid MASTER_KEK URI %q: %w", uri, err)
	}

	registryMu.RLock()
	f, ok := registry[u.Scheme]
	registryMu.RUnlock()

	if !ok {
		registryMu.RLock()
		schemes := make([]string, 0, len(registry))
		for s := range registry {
			schemes = append(schemes, s+"://")
		}
		registryMu.RUnlock()
		return nil, fmt.Errorf("kms: unknown MASTER_KEK scheme %q (registered: %s)", u.Scheme, strings.Join(schemes, ", "))
	}

	return f(ctx, u)
}
