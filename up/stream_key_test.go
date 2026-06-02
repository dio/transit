package up

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/dio/transit/up/testutil"
)

// TestStreamKey_keyRoundTrip: Key() returns the string passed to NewStreamKey.
func TestStreamKey_keyRoundTrip(t *testing.T) {
	k := NewStreamKey[string]("orange.decision")
	require.Equal(t, "orange.decision", k.Key())
}

// TestStreamKey_setGetWriter: typed set/get round-trip through Writer.
func TestStreamKey_setGetWriter(t *testing.T) {
	w, _ := newTestWriter(t)
	defer func() { dropBag(w.f.streamObjectNonce) }()

	k := NewStreamKey[int]("test.int")
	k.Set(w, 99)

	got, ok := k.Get(w)
	require.True(t, ok, "Get returned false after Set")
	require.Equal(t, 99, got)
}

// TestStreamKey_getMissing: Get returns (zero, false) when the key was never set.
func TestStreamKey_getMissing(t *testing.T) {
	w, _ := newTestWriter(t)
	// No Set call; no bag is created.

	k := NewStreamKey[string]("missing.key")
	v, ok := k.Get(w)
	require.False(t, ok, "Get returned true for an unset key")
	require.Empty(t, v, "Get returned non-zero value for an unset key")
}

// TestStreamKey_structType: StreamKey works with a struct pointer type.
func TestStreamKey_structType(t *testing.T) {
	type Decision struct {
		Model string
		Score float64
	}

	w, _ := newTestWriter(t)
	defer func() { dropBag(w.f.streamObjectNonce) }()

	k := NewStreamKey[*Decision]("orange.decision")
	want := &Decision{Model: "gpt-4o", Score: 0.95}
	k.Set(w, want)

	got, ok := k.Get(w)
	require.True(t, ok, "Get returned false")
	require.Equal(t, want.Model, got.Model)
	require.InDelta(t, want.Score, got.Score, 1e-9)
}

// TestStreamKey_getFromCtx: a value Set via Writer is readable through
// FakeClusterLBContext.GetStreamObject via StreamKey.GetFromCtx.
func TestStreamKey_getFromCtx(t *testing.T) {
	w, h := newTestWriter(t)
	defer func() { dropBag(w.f.streamObjectNonce) }()

	k := NewStreamKey[string]("mcp.session")
	k.Set(w, "sess-abc")

	// NewWriter uses directWrite mode, so the nonce is already in h's filter state.
	ctx := testutil.NewFakeClusterLBContext(h)

	got, ok := k.GetFromCtx(ctx)
	require.True(t, ok, "GetFromCtx returned false")
	require.Equal(t, "sess-abc", got)
}

// TestStreamKey_getFromCtxNoValue: GetFromCtx returns (zero, false) when no
// bag exists (Writer never called SetStreamObject).
func TestStreamKey_getFromCtxNoValue(t *testing.T) {
	_, h := newTestWriter(t)
	// No Set call; no nonce written to filter state.
	ctx := testutil.NewFakeClusterLBContext(h)

	k := NewStreamKey[int]("noop.key")
	v, ok := k.GetFromCtx(ctx)
	require.False(t, ok, "GetFromCtx returned true for an empty context")
	require.Zero(t, v)
}

// TestStreamKey_multipleKeys: multiple StreamKey instances on the same stream
// are independent.
func TestStreamKey_multipleKeys(t *testing.T) {
	w, _ := newTestWriter(t)
	defer func() { dropBag(w.f.streamObjectNonce) }()

	ka := NewStreamKey[int]("key.a")
	kb := NewStreamKey[string]("key.b")

	ka.Set(w, 42)
	kb.Set(w, "hello")

	a, ok := ka.Get(w)
	require.True(t, ok)
	require.Equal(t, 42, a)

	b, ok := kb.Get(w)
	require.True(t, ok)
	require.Equal(t, "hello", b)
}

// TestStreamKey_compileTimeTypeSafety documents that the generics prevent
// mixing types at compile time. A StreamKey[int] cannot be used to retrieve
// a value as string — the type parameter enforces this. No runtime assertion
// is needed; the following would not compile:
//
//	kInt := NewStreamKey[int]("x")
//	kInt.Set(w, 1)
//	var s string = kInt.Get(w)  // compile error
//
// This test exists solely to document the constraint and keep the test suite
// complete per WS-C acceptance criteria.
func TestStreamKey_compileTimeTypeSafety(t *testing.T) {
	t.Log("Type safety is enforced at compile time by Go generics; see comment above.")
}
