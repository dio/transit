package up

import (
	"testing"

	"github.com/dio/transit/up/testutil"
)

// TestStreamKey_keyRoundTrip: Key() returns the string passed to NewStreamKey.
func TestStreamKey_keyRoundTrip(t *testing.T) {
	k := NewStreamKey[string]("orange.decision")
	if got := k.Key(); got != "orange.decision" {
		t.Errorf("Key() = %q, want %q", got, "orange.decision")
	}
}

// TestStreamKey_setGetWriter: typed set/get round-trip through Writer.
func TestStreamKey_setGetWriter(t *testing.T) {
	w, _ := newTestWriter(t)
	defer func() { dropBag(w.f.streamObjectNonce) }()

	k := NewStreamKey[int]("test.int")
	k.Set(w, 99)

	got, ok := k.Get(w)
	if !ok {
		t.Fatal("Get returned false after Set")
	}
	if got != 99 {
		t.Errorf("Get = %d, want 99", got)
	}
}

// TestStreamKey_getMissing: Get returns (zero, false) when the key was never set.
func TestStreamKey_getMissing(t *testing.T) {
	w, _ := newTestWriter(t)
	// No Set call; no bag is created.

	k := NewStreamKey[string]("missing.key")
	v, ok := k.Get(w)
	if ok {
		t.Errorf("Get returned true for an unset key, value = %q", v)
	}
	if v != "" {
		t.Errorf("Get returned non-zero value for an unset key: %q", v)
	}
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
	if !ok {
		t.Fatal("Get returned false")
	}
	if got.Model != want.Model || got.Score != want.Score {
		t.Errorf("got %+v, want %+v", got, want)
	}
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
	if !ok {
		t.Fatal("GetFromCtx returned false")
	}
	if got != "sess-abc" {
		t.Errorf("GetFromCtx = %q, want %q", got, "sess-abc")
	}
}

// TestStreamKey_getFromCtxNoValue: GetFromCtx returns (zero, false) when no
// bag exists (Writer never called SetStreamObject).
func TestStreamKey_getFromCtxNoValue(t *testing.T) {
	_, h := newTestWriter(t)
	// No Set call; no nonce written to filter state.
	ctx := testutil.NewFakeClusterLBContext(h)

	k := NewStreamKey[int]("noop.key")
	v, ok := k.GetFromCtx(ctx)
	if ok {
		t.Errorf("GetFromCtx returned true with value %d for an empty context", v)
	}
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
	if !ok || a != 42 {
		t.Errorf("ka.Get = %d, %v; want 42, true", a, ok)
	}
	b, ok := kb.Get(w)
	if !ok || b != "hello" {
		t.Errorf("kb.Get = %q, %v; want hello, true", b, ok)
	}
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
