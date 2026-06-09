package config

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	"connectrpc.com/connect"
	"github.com/hashicorp/golang-lru/v2/expirable"
	"golang.org/x/sync/singleflight"

	secretv1 "github.com/dio/transit/examples/orange/api/orange/secret/v1"
	secretv1connect "github.com/dio/transit/examples/orange/api/orange/secret/v1/secretv1connect"
)

// ── Scheme-dispatching resolver ───────────────────────────────────────────────

// DispatchResolver routes each reference to the SecretResolver registered for
// its scheme (the segment before "://"). It is immutable after construction and
// safe for concurrent use.
type DispatchResolver struct {
	resolvers map[string]SecretResolver
}

// NewDispatchResolver builds a DispatchResolver from a scheme → resolver map.
// All registrations must be complete before the resolver is used concurrently.
func NewDispatchResolver(resolvers map[string]SecretResolver) *DispatchResolver {
	r := &DispatchResolver{resolvers: make(map[string]SecretResolver, len(resolvers))}
	for scheme, resolver := range resolvers {
		r.resolvers[scheme] = resolver
	}
	return r
}

// Resolve extracts the scheme from ref and forwards the call to the matching
// resolver. Returns an error when ref has no "://" separator or the scheme is
// not registered.
func (d *DispatchResolver) Resolve(ctx context.Context, ref string) (string, error) {
	scheme, _, ok := strings.Cut(ref, "://")
	if !ok {
		return "", fmt.Errorf("secret ref %q: missing scheme (expected scheme://...)", ref)
	}
	r, ok := d.resolvers[scheme]
	if !ok {
		return "", fmt.Errorf("secret ref %q: no resolver registered for scheme %q", ref, scheme)
	}
	return r.Resolve(ctx, ref)
}

// Invalidate forwards the call to the matching scheme's resolver. Unknown
// schemes are silently ignored.
func (d *DispatchResolver) Invalidate(ref string) {
	scheme, _, ok := strings.Cut(ref, "://")
	if !ok {
		return
	}
	if r, ok := d.resolvers[scheme]; ok {
		r.Invalidate(ref)
	}
}

// ── Built-in resolvers ────────────────────────────────────────────────────────

// EnvResolver resolves env://VAR_NAME references by reading the named
// environment variable. It reads the live process environment on every call —
// no caching, no state. Wrap with CachedResolver if repeated reads are costly.
type EnvResolver struct{}

// Resolve returns the value of the environment variable named by the part of
// ref after "env://". Returns an error when the variable is not set in the
// process environment.
func (e *EnvResolver) Resolve(_ context.Context, ref string) (string, error) {
	_, name, ok := strings.Cut(ref, "env://")
	if !ok || name == "" {
		return "", fmt.Errorf("env resolver: %q: expected env://<VAR_NAME>", ref)
	}
	val, set := os.LookupEnv(name)
	if !set {
		return "", fmt.Errorf("env resolver: variable %q is not set", name)
	}
	return val, nil
}

// Invalidate is a no-op; env vars have no client-side cache to evict.
func (e *EnvResolver) Invalidate(_ string) {}

// FileResolver resolves file:///path/to/secret references by reading the file
// at the given path. The content is returned with leading and trailing
// whitespace trimmed, which handles the common case of a trailing newline.
// No caching — reads the file on every call.
type FileResolver struct{}

// Resolve reads the file at the path encoded in ref (the segment after "file://")
// and returns its whitespace-trimmed content.
func (f *FileResolver) Resolve(_ context.Context, ref string) (string, error) {
	_, path, ok := strings.Cut(ref, "file://")
	if !ok || path == "" {
		return "", fmt.Errorf("file resolver: %q: expected file://<path>", ref)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("file resolver: %w", err)
	}
	return strings.TrimSpace(string(data)), nil
}

// Invalidate is a no-op; FileResolver reads the file directly on every call.
func (f *FileResolver) Invalidate(_ string) {}

// LiteralResolver resolves literal://value references by returning the value
// portion verbatim. Intended for development and integration tests only —
// embedding plaintext secrets in config files is not safe for production.
type LiteralResolver struct{}

// Resolve returns the segment of ref after "literal://". An empty value is
// valid and returns an empty string without error.
func (l *LiteralResolver) Resolve(_ context.Context, ref string) (string, error) {
	_, value, ok := strings.Cut(ref, "literal://")
	if !ok {
		return "", fmt.Errorf("literal resolver: %q: expected literal://<value>", ref)
	}
	return value, nil
}

// Invalidate is a no-op; literal values are stateless.
func (l *LiteralResolver) Invalidate(_ string) {}

// ── Caching wrapper ───────────────────────────────────────────────────────────

// cachedResolverMaxSize is the maximum number of distinct secret references
// held in a CachedResolver at any time. Entries are evicted LRU-style when
// the limit is reached, and individually via Invalidate.
const cachedResolverMaxSize = 1024

// CachedResolver wraps an inner SecretResolver and holds resolved values in a
// TTL-based LRU cache (backed by github.com/hashicorp/golang-lru/v2/expirable).
// On a cache miss it delegates to the inner resolver, which may be a
// DispatchResolver, a SecretService client, or any other implementation.
//
// Thundering herd: concurrent cache misses for the same ref are coalesced by
// a singleflight.Group so the inner resolver is called at most once per ref
// per in-flight window.
//
// Secret rotation: when the secret-service notifies of a rotation (e.g. via
// webhook), call Invalidate with the affected ref. The next Resolve call will
// fetch a fresh value; no config reload is required. Errors from the inner
// resolver are never cached — a failing call is retried on the next request.
//
// CachedResolver is safe for concurrent use.
type CachedResolver struct {
	inner SecretResolver
	cache *expirable.LRU[string, string]
	group singleflight.Group
}

// minCachedResolverTTL is the smallest TTL accepted by NewCachedResolver.
// expirable.LRU divides the TTL by its internal bucket count (100) to derive
// the sweep ticker interval; a TTL below 100ns would produce a zero-duration
// ticker, which time.NewTicker rejects with a panic.
const minCachedResolverTTL = 100 * time.Nanosecond

// NewCachedResolver returns a CachedResolver that wraps inner with the given
// TTL. ttl must be >= minCachedResolverTTL (100ns); NewCachedResolver panics
// otherwise. The cache holds at most cachedResolverMaxSize entries; older
// entries are evicted LRU-style when the limit is reached.
func NewCachedResolver(inner SecretResolver, ttl time.Duration) *CachedResolver {
	if ttl < minCachedResolverTTL {
		panic("config: CachedResolver TTL must be >= 100ns")
	}
	return &CachedResolver{
		inner: inner,
		cache: expirable.NewLRU[string, string](cachedResolverMaxSize, nil, ttl),
	}
}

// Resolve returns the cached value for ref if present and unexpired. On a miss
// it uses singleflight to coalesce concurrent requests for the same ref, calls
// the inner resolver exactly once, caches the result, and returns it.
// Errors from the inner resolver are not cached — the next call retries.
func (c *CachedResolver) Resolve(ctx context.Context, ref string) (string, error) {
	if val, ok := c.cache.Get(ref); ok {
		return val, nil
	}
	v, err, _ := c.group.Do(ref, func() (any, error) {
		// Re-check inside singleflight: another goroutine may have populated
		// the entry between our cache miss above and acquiring the flight slot.
		if val, ok := c.cache.Get(ref); ok {
			return val, nil
		}
		val, err := c.inner.Resolve(ctx, ref)
		if err != nil {
			return "", err
		}
		c.cache.Add(ref, val)
		return val, nil
	})
	if err != nil {
		return "", err
	}
	str, ok := v.(string)
	if !ok {
		return "", fmt.Errorf("expected string, got %T", v)
	}
	return str, nil
}

// Invalidate removes ref from the cache. The next Resolve call for this ref
// will fetch a fresh value from the inner resolver. Safe to call when ref is
// absent from the cache.
func (c *CachedResolver) Invalidate(ref string) {
	c.cache.Remove(ref)
}

// OrangeResolver resolves orange://workspace_id/realm/secret_id references by
// calling the SecretResolverService to fetch the current enabled version's plaintext.
// It requires an HTTP client and server URL to communicate with the service.
type OrangeResolver struct {
	httpClient connect.HTTPClient
	serverURL  string
}

// NewOrangeResolver returns an OrangeResolver backed by the given HTTP client
// and server URL. The server URL should be the base URL of the orange service
// (e.g., "http://localhost:3000").
func NewOrangeResolver(httpClient connect.HTTPClient, serverURL string) *OrangeResolver {
	return &OrangeResolver{
		httpClient: httpClient,
		serverURL:  serverURL,
	}
}

// Resolve parses the orange:// reference and fetches the secret from
// SecretResolverService. The reference format is:
//
//	orange://workspace_id/realm/secret_id
func (o *OrangeResolver) Resolve(ctx context.Context, ref string) (string, error) {
	const scheme = "orange://"
	if !strings.HasPrefix(ref, scheme) {
		return "", fmt.Errorf("orange resolver: %q: expected %s<workspace_id>/<realm>/<secret_id>", ref, scheme)
	}

	path := ref[len(scheme):]
	parts := strings.SplitN(path, "/", 3)
	if len(parts) != 3 || parts[0] == "" || parts[1] == "" || parts[2] == "" {
		return "", fmt.Errorf("orange resolver: %q: expected %s<workspace_id>/<realm>/<secret_id>", ref, scheme)
	}

	workspaceID, realm, secretID := parts[0], parts[1], parts[2]

	if o.httpClient == nil || o.serverURL == "" {
		return "", fmt.Errorf("orange:// resolver not configured (no egress bundle / server URL): %s/%s/%s", workspaceID, realm, secretID)
	}

	client := secretv1connect.NewSecretResolverServiceClient(o.httpClient, o.serverURL)
	req := &secretv1.ResolveRequest{
		Realm:    realm,
		SecretId: secretID,
	}
	resp, err := client.Resolve(ctx, connect.NewRequest(req))
	if err != nil {
		return "", fmt.Errorf("orange resolver: resolve %s/%s/%s: %w", workspaceID, realm, secretID, err)
	}

	payload := resp.Msg.GetPayload()
	if payload == nil {
		return "", fmt.Errorf("orange resolver: resolve %s/%s/%s: unexpected response type", workspaceID, realm, secretID)
	}
	return string(payload.GetMaterial()), nil
}

// Invalidate is a no-op for OrangeResolver; secret rotation is handled via
// server-side invalidation and the Watch/Fetch streams.
func (o *OrangeResolver) Invalidate(_ string) {}

// ── Default resolver ──────────────────────────────────────────────────────────

// NewDefaultResolver returns a CachedResolver that handles all supported
// secret_ref schemes: env://, file://, literal://, and orange://.
//
// httpClient and serverURL back the orange:// scheme. When either is nil/"",
// orange:// references resolve to an error at use time (not at construction).
// Pass nil/"" in standalone mode where no egress bundle is available.
func NewDefaultResolver(httpClient connect.HTTPClient, serverURL string, ttl time.Duration) *CachedResolver {
	return NewCachedResolver(
		NewDispatchResolver(map[string]SecretResolver{
			"env":     &EnvResolver{},
			"file":    &FileResolver{},
			"literal": &LiteralResolver{},
			"orange":  NewOrangeResolver(httpClient, serverURL),
		}),
		ttl,
	)
}
