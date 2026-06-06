package config

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// bg is a convenience shorthand for context.Background() in tests.
var bg = context.Background()

// ── EnvResolver ───────────────────────────────────────────────────────────────

func TestEnvResolver_Resolve(t *testing.T) {
	t.Setenv("SECRET_TEST_VAR", "hello")
	r := &EnvResolver{}
	got, err := r.Resolve(bg, "env://SECRET_TEST_VAR")
	require.NoError(t, err)
	assert.Equal(t, "hello", got)
}

func TestEnvResolver_Unset(t *testing.T) {
	r := &EnvResolver{}
	_, err := r.Resolve(bg, "env://DEFINITELY_NOT_SET_XYZZY_12345")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not set")
}

func TestEnvResolver_BadRef(t *testing.T) {
	r := &EnvResolver{}
	_, err := r.Resolve(bg, "env://")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "expected env://")
}

func TestEnvResolver_WrongScheme(t *testing.T) {
	r := &EnvResolver{}
	// Passing a non-env ref; Cut will fail to find the "env://" separator.
	_, err := r.Resolve(bg, "literal://something")
	require.Error(t, err)
}

func TestEnvResolver_Invalidate_NoOp(t *testing.T) {
	// Invalidate must not panic and must not affect subsequent Resolve calls.
	t.Setenv("SECRET_TEST_NOOP", "value")
	r := &EnvResolver{}
	r.Invalidate("env://SECRET_TEST_NOOP")
	got, err := r.Resolve(bg, "env://SECRET_TEST_NOOP")
	require.NoError(t, err)
	assert.Equal(t, "value", got)
}

// ── FileResolver ──────────────────────────────────────────────────────────────

func writeSecretFile(t *testing.T, content string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "secret")
	require.NoError(t, os.WriteFile(path, []byte(content), 0o600))
	return path
}

func TestFileResolver_Resolve(t *testing.T) {
	path := writeSecretFile(t, "my-secret-token\n")
	r := &FileResolver{}
	got, err := r.Resolve(bg, "file://"+path)
	require.NoError(t, err)
	assert.Equal(t, "my-secret-token", got) // trailing newline trimmed
}

func TestFileResolver_TrimsWhitespace(t *testing.T) {
	path := writeSecretFile(t, "  padded  \n")
	r := &FileResolver{}
	got, err := r.Resolve(bg, "file://"+path)
	require.NoError(t, err)
	assert.Equal(t, "padded", got)
}

func TestFileResolver_Missing(t *testing.T) {
	r := &FileResolver{}
	_, err := r.Resolve(bg, "file:///this/path/does/not/exist")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "file resolver")
}

func TestFileResolver_BadRef(t *testing.T) {
	r := &FileResolver{}
	_, err := r.Resolve(bg, "file://")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "expected file://")
}

func TestFileResolver_Invalidate_NoOp(t *testing.T) {
	path := writeSecretFile(t, "val")
	r := &FileResolver{}
	r.Invalidate("file://" + path)
	got, err := r.Resolve(bg, "file://"+path)
	require.NoError(t, err)
	assert.Equal(t, "val", got)
}

// ── LiteralResolver ───────────────────────────────────────────────────────────

func TestLiteralResolver_Resolve(t *testing.T) {
	r := &LiteralResolver{}
	got, err := r.Resolve(bg, "literal://super-secret")
	require.NoError(t, err)
	assert.Equal(t, "super-secret", got)
}

func TestLiteralResolver_Empty(t *testing.T) {
	r := &LiteralResolver{}
	got, err := r.Resolve(bg, "literal://")
	require.NoError(t, err)
	assert.Equal(t, "", got) // empty literal is valid
}

func TestLiteralResolver_BadRef(t *testing.T) {
	r := &LiteralResolver{}
	_, err := r.Resolve(bg, "noscheme")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "expected literal://")
}

func TestLiteralResolver_Invalidate_NoOp(t *testing.T) {
	r := &LiteralResolver{}
	r.Invalidate("literal://anything")
	got, err := r.Resolve(bg, "literal://anything")
	require.NoError(t, err)
	assert.Equal(t, "anything", got)
}

// ── DispatchResolver ──────────────────────────────────────────────────────────

func newTestDispatch() *DispatchResolver {
	return NewDispatchResolver(map[string]SecretResolver{
		"literal": &LiteralResolver{},
		"env":     &EnvResolver{},
	})
}

func TestDispatchResolver_Routes(t *testing.T) {
	d := newTestDispatch()
	got, err := d.Resolve(bg, "literal://hello")
	require.NoError(t, err)
	assert.Equal(t, "hello", got)
}

func TestDispatchResolver_EnvScheme(t *testing.T) {
	t.Setenv("DISPATCH_TEST_VAR", "dispatched")
	d := newTestDispatch()
	got, err := d.Resolve(bg, "env://DISPATCH_TEST_VAR")
	require.NoError(t, err)
	assert.Equal(t, "dispatched", got)
}

func TestDispatchResolver_UnknownScheme(t *testing.T) {
	d := newTestDispatch()
	_, err := d.Resolve(bg, "vault://path/to/secret")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no resolver registered for scheme")
}

func TestDispatchResolver_NoScheme(t *testing.T) {
	d := newTestDispatch()
	_, err := d.Resolve(bg, "no-scheme-here")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "missing scheme")
}

func TestDispatchResolver_Invalidate_ForwardsToScheme(t *testing.T) {
	// Use a spy resolver to verify Invalidate is forwarded.
	spy := &spyResolver{inner: &LiteralResolver{}}
	d := NewDispatchResolver(map[string]SecretResolver{"spy": spy})
	d.Invalidate("spy://ref")
	assert.Equal(t, 1, spy.invalidateCalls)
}

func TestDispatchResolver_Invalidate_UnknownScheme_NoOp(t *testing.T) {
	d := newTestDispatch()
	// Must not panic for unknown scheme.
	assert.NotPanics(t, func() { d.Invalidate("vault://x") })
}

func TestDispatchResolver_Invalidate_MissingScheme_NoOp(t *testing.T) {
	d := newTestDispatch()
	assert.NotPanics(t, func() { d.Invalidate("no-scheme") })
}

// ── CachedResolver ───────────────────────────────────────────────────────────

func TestCachedResolver_CacheHit(t *testing.T) {
	spy := &spyResolver{inner: &LiteralResolver{}}
	c := NewCachedResolver(spy, time.Hour)

	v1, err := c.Resolve(bg, "spy://literal://token")
	require.NoError(t, err)
	v2, err := c.Resolve(bg, "spy://literal://token")
	require.NoError(t, err)

	assert.Equal(t, v1, v2)
	assert.Equal(t, 1, spy.resolveCalls, "inner resolver must only be called once on a cache hit")
}

func TestCachedResolver_CacheExpiry(t *testing.T) {
	spy := &spyResolver{inner: &LiteralResolver{}}
	// TTL must be at least 100*time.Nanosecond so that the expirable.LRU
	// internal sweep ticker (ttl/100 buckets) gets a positive interval.
	c := NewCachedResolver(spy, 10*time.Millisecond)

	_, err := c.Resolve(bg, "spy://literal://token")
	require.NoError(t, err)
	time.Sleep(50 * time.Millisecond) // ensure TTL is exhausted
	_, err = c.Resolve(bg, "spy://literal://token")
	require.NoError(t, err)

	assert.Equal(t, 2, spy.resolveCalls, "inner must be called again after TTL expiry")
}

func TestCachedResolver_Invalidate(t *testing.T) {
	spy := &spyResolver{inner: &LiteralResolver{}}
	c := NewCachedResolver(spy, time.Hour)
	const ref = "spy://literal://rotating-secret"

	_, err := c.Resolve(bg, ref)
	require.NoError(t, err)
	assert.Equal(t, 1, spy.resolveCalls)

	c.Invalidate(ref)

	_, err = c.Resolve(bg, ref)
	require.NoError(t, err)
	assert.Equal(t, 2, spy.resolveCalls, "inner must be called again after explicit invalidation")
}

func TestCachedResolver_Invalidate_Absent(t *testing.T) {
	c := NewCachedResolver(&LiteralResolver{}, time.Hour)
	// Must not panic when the ref is not in the cache.
	assert.NotPanics(t, func() { c.Invalidate("literal://absent") })
}

func TestCachedResolver_ErrorNotCached(t *testing.T) {
	r := &EnvResolver{}
	c := NewCachedResolver(r, time.Hour)
	const ref = "env://CACHING_NONEXISTENT_VAR_TEST"

	_, err := c.Resolve(bg, ref)
	require.Error(t, err)

	// Set the var now; should resolve successfully on retry (error not cached).
	t.Setenv("CACHING_NONEXISTENT_VAR_TEST", "now-set")
	got, err := c.Resolve(bg, ref)
	require.NoError(t, err)
	assert.Equal(t, "now-set", got)
}

func TestCachedResolver_NegativeTTL_Panics(t *testing.T) {
	assert.Panics(t, func() {
		NewCachedResolver(&LiteralResolver{}, -time.Second)
	})
}

func TestCachedResolver_ZeroTTL_Panics(t *testing.T) {
	assert.Panics(t, func() {
		NewCachedResolver(&LiteralResolver{}, 0)
	})
}

func TestCachedResolver_Concurrent(t *testing.T) {
	// Many goroutines resolving the same ref simultaneously must all get the
	// correct value, and the inner resolver must be called at most once per
	// unique ref (coalesced by the write-lock slow path).
	spy := &spyResolver{inner: &LiteralResolver{}}
	c := NewCachedResolver(spy, time.Hour)

	const workers = 100
	const ref = "spy://literal://concurrent-value"
	results := make([]string, workers)
	var wg sync.WaitGroup
	for i := range workers {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			v, err := c.Resolve(bg, ref)
			require.NoError(t, err)
			results[i] = v
		}(i)
	}
	wg.Wait()

	for _, v := range results {
		assert.Equal(t, "concurrent-value", v)
	}
	// Under the write lock, exactly one goroutine fetches from the inner resolver.
	assert.Equal(t, 1, spy.resolveCalls)
}

// ── NewDefaultResolver ────────────────────────────────────────────────────────

func TestNewDefaultResolver_Env(t *testing.T) {
	t.Setenv("DEFAULT_RESOLVER_TEST", "env-val")
	r := NewDefaultResolver(time.Hour)
	got, err := r.Resolve(bg, "env://DEFAULT_RESOLVER_TEST")
	require.NoError(t, err)
	assert.Equal(t, "env-val", got)
}

func TestNewDefaultResolver_Literal(t *testing.T) {
	r := NewDefaultResolver(time.Hour)
	got, err := r.Resolve(bg, "literal://plain-text")
	require.NoError(t, err)
	assert.Equal(t, "plain-text", got)
}

func TestNewDefaultResolver_File(t *testing.T) {
	path := writeSecretFile(t, "file-secret")
	r := NewDefaultResolver(time.Hour)
	got, err := r.Resolve(bg, "file://"+path)
	require.NoError(t, err)
	assert.Equal(t, "file-secret", got)
}

func TestNewDefaultResolver_UnknownScheme(t *testing.T) {
	r := NewDefaultResolver(time.Hour)
	_, err := r.Resolve(bg, "vault://secret/path")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no resolver registered for scheme")
}

func TestNewDefaultResolver_Cached(t *testing.T) {
	t.Setenv("CACHED_RESOLVER_VAR", "cached")
	r := NewDefaultResolver(time.Hour)

	v1, err := r.Resolve(bg, "env://CACHED_RESOLVER_VAR")
	require.NoError(t, err)

	// Unset the var; result must still come from cache.
	os.Unsetenv("CACHED_RESOLVER_VAR")
	v2, err := r.Resolve(bg, "env://CACHED_RESOLVER_VAR")
	require.NoError(t, err)
	assert.Equal(t, v1, v2, "cached value must survive env var removal within TTL")
}

func TestNewDefaultResolver_Invalidate(t *testing.T) {
	t.Setenv("INVALIDATE_TEST_VAR", "original")
	r := NewDefaultResolver(time.Hour)

	_, err := r.Resolve(bg, "env://INVALIDATE_TEST_VAR")
	require.NoError(t, err)

	t.Setenv("INVALIDATE_TEST_VAR", "rotated")
	r.Invalidate("env://INVALIDATE_TEST_VAR")

	got, err := r.Resolve(bg, "env://INVALIDATE_TEST_VAR")
	require.NoError(t, err)
	assert.Equal(t, "rotated", got, "post-invalidate Resolve must pick up the new value")
}

// ── Test helpers ──────────────────────────────────────────────────────────────

// spyResolver wraps an inner SecretResolver and counts how many times Resolve
// and Invalidate are called. It uses the "spy://..." prefix convention:
// callers pass "spy://<real-ref>" and spyResolver strips the prefix before
// forwarding to the inner resolver.
type spyResolver struct {
	mu              sync.Mutex
	inner           SecretResolver
	resolveCalls    int
	invalidateCalls int
}

func (s *spyResolver) Resolve(ctx context.Context, ref string) (string, error) {
	s.mu.Lock()
	s.resolveCalls++
	s.mu.Unlock()
	// Strip the "spy://" prefix before forwarding.
	_, inner, _ := strings.Cut(ref, "spy://")
	return s.inner.Resolve(ctx, inner)
}

func (s *spyResolver) Invalidate(ref string) {
	s.mu.Lock()
	s.invalidateCalls++
	s.mu.Unlock()
	_, inner, _ := strings.Cut(ref, "spy://")
	s.inner.Invalidate(inner)
}
