package classify

import (
	"encoding/json"
	"os"
	"testing"

	"github.com/dio/transit/down"
	"github.com/dio/transit/examples/orange/config"
	"github.com/dio/transit/examples/orange/pending"
	"github.com/dio/transit/up"
	"github.com/dio/transit/up/testutil"
)

func loadTestConfig(t *testing.T) {
	t.Helper()
	t.Setenv("OPENAI_API_KEY", "sk-test-openai")
	t.Setenv("ANTHROPIC_API_KEY", "sk-test-anthropic")
	if err := os.Setenv(config.EnvVar, "../orange.yaml"); err != nil {
		t.Fatal(err)
	}
	config.MustReload()
}

func newPostStream(t *testing.T) (*up.Writer, *testutil.FakeFilterHandle, *up.Request, *any) {
	t.Helper()
	hdr := map[string]string{
		":method":      "POST",
		":path":        "/v1/chat/completions",
		"content-type": "application/json",
	}
	h := testutil.NewFilterHandle(testutil.WithHeaders(hdr))
	w := up.NewWriter(h)
	r := up.NewRequest(h.RequestHeaders(), FilterName)
	var ctx any
	r.Context = &ctx
	return w, h, r, &ctx
}

// runFlow runs requestHandler and (if body != nil) bodyHandler, sharing the
// per-stream context slot the SDK normally provides via *f.context.
func runFlow(t *testing.T, body []byte) (*testutil.FakeFilterHandle, *streamState) {
	t.Helper()
	w, h, r, ctx := newPostStream(t)
	requestHandler(w, r)
	if body == nil {
		st, _ := (*ctx).(*streamState)
		return h, st
	}
	chunk := &up.BodyChunk{Data: body, EndStream: true, Context: ctx}
	bodyHandler(w, chunk)
	st, _ := (*ctx).(*streamState)
	return h, st
}

// streamObjectNonce reads the "up.stream_object_id" filter state from the
// handle; returns "" if absent.
func streamObjectNonce(h *testutil.FakeFilterHandle) string {
	v, ok := h.FilterStateString(down.StreamObjectIDKey)
	if !ok {
		return ""
	}
	return v
}

func TestHeaders_storesPendingInStreamObjectBag(t *testing.T) {
	loadTestConfig(t)
	w, h, r, ctx := newPostStream(t)
	requestHandler(w, r)

	st, ok := (*ctx).(*streamState)
	if !ok || st == nil {
		t.Fatal("expected stream state to be stashed in r.Context")
	}
	if st.p == nil {
		t.Error("pending is nil")
	}

	// The Pending must be in the stream-object bag under StreamObjectKey.
	nonce := streamObjectNonce(h)
	if nonce == "" {
		t.Fatal("stream-object nonce not written to filter state")
	}
	bag, ok := down.LookupStreamObjectBag(nonce)
	if !ok {
		t.Fatal("bag not found for nonce")
	}
	v, ok := bag.Get(StreamObjectKey)
	if !ok {
		t.Fatal("StreamObjectKey not in bag")
	}
	if v != st.p {
		t.Error("bag entry is not the same *pending.Pending as streamState.p")
	}

	// Cleanup: drain the bag (normally done by OnStreamComplete).
	down.DropStreamObjectBag(nonce)
}

func TestBody_knownModel_resolvesUpstream(t *testing.T) {
	loadTestConfig(t)
	body := []byte(`{"model":"gpt-4o-mini","messages":[]}`)
	h, st := runFlow(t, body)

	if len(h.LocalResponses) != 0 {
		t.Fatalf("unexpected local response: %+v", h.LocalResponses)
	}
	res, ok := st.p.Result()
	if !ok {
		t.Fatal("pending not resolved")
	}
	if res.Err != "" {
		t.Errorf("err = %q, want empty", res.Err)
	}
	if res.Provider != "openai_direct" {
		t.Errorf("provider = %q", res.Provider)
	}
	if res.Kind != "openai" {
		t.Errorf("kind = %q", res.Kind)
	}
	if res.Model != "gpt-4o-mini" {
		t.Errorf("model = %q", res.Model)
	}
	if v, ok := h.Metadata(MetadataNamespace, MetadataKeyUpstream); !ok || v != "openai_direct" {
		t.Errorf("metadata upstream = (%q, %v)", v, ok)
	}

	// Bag entry must still be present before onStreamComplete (SDK owns the drain).
	nonce := streamObjectNonce(h)
	if nonce == "" {
		t.Fatal("nonce missing from filter state")
	}
	if _, ok := down.LookupStreamObjectBag(nonce); !ok {
		t.Error("bag entry unexpectedly missing before stream complete")
	}

	// Simulate OnStreamComplete: call onStreamComplete then drain the bag.
	var ctx any = st
	onStreamComplete(&ctx)
	down.DropStreamObjectBag(nonce)

	// After drain, bag must be gone.
	if _, ok := down.LookupStreamObjectBag(nonce); ok {
		t.Error("bag entry not gone after stream complete + drain")
	}
	// Resolve must remain the bodyHandler's (CAS loses races silently).
	if got, _ := st.p.Result(); got.Err != "" {
		t.Errorf("post-cleanup err = %q, want empty (CAS should preserve body result)", got.Err)
	}
}

func TestOnStreamComplete_cleansUpWhenBodyNeverRan(t *testing.T) {
	loadTestConfig(t)
	// Headers-only flow: simulates downstream disconnect / timeout after
	// headers and before the body handler runs.
	h, st := runFlow(t, nil)
	if st == nil {
		t.Fatal("expected stream state from headers phase")
	}

	nonce := streamObjectNonce(h)
	if nonce == "" {
		t.Fatal("nonce missing from filter state")
	}

	// Bag must be present before onStreamComplete.
	if _, ok := down.LookupStreamObjectBag(nonce); !ok {
		t.Error("bag entry missing before onStreamComplete")
	}

	var ctx any = st
	onStreamComplete(&ctx)

	// onStreamComplete must have resolved the Pending with ErrStreamTerminated.
	res, ok := st.p.Result()
	if !ok {
		t.Fatal("pending should be resolved by onStreamComplete")
	}
	if res.Err != ErrStreamTerminated {
		t.Errorf("err = %q, want %q", res.Err, ErrStreamTerminated)
	}

	// SDK drains the bag; simulate that here.
	down.DropStreamObjectBag(nonce)
	if _, ok := down.LookupStreamObjectBag(nonce); ok {
		t.Error("bag still present after drain")
	}
}

func TestOnStreamComplete_nilContextIsNoop(t *testing.T) {
	loadTestConfig(t)
	// SDK calls onStreamComplete even for requests that never matched the
	// route (no streamState was ever stashed). Must not panic.
	var ctx any
	onStreamComplete(&ctx)
	onStreamComplete(nil)
}

func TestBody_missingModel_400(t *testing.T) {
	loadTestConfig(t)
	body := []byte(`{"messages":[]}`)
	h, st := runFlow(t, body)

	if len(h.LocalResponses) != 1 || h.LocalResponses[0].Status != 400 {
		t.Fatalf("expected one 400 response, got %+v", h.LocalResponses)
	}
	var got struct{ Error, Code string }
	_ = json.Unmarshal(h.LocalResponses[0].Body, &got)
	if got.Code != ErrModelRequired {
		t.Errorf("code = %q", got.Code)
	}
	res, _ := st.p.Result()
	if res.Err != ErrModelRequired {
		t.Errorf("pending err = %q", res.Err)
	}

	// Cleanup bag.
	nonce := streamObjectNonce(h)
	down.DropStreamObjectBag(nonce)
}

func TestBody_unknownModel_404(t *testing.T) {
	loadTestConfig(t)
	body := []byte(`{"model":"gemini-1.5"}`)
	h, st := runFlow(t, body)

	if len(h.LocalResponses) != 1 || h.LocalResponses[0].Status != 404 {
		t.Fatalf("expected one 404 response, got %+v", h.LocalResponses)
	}
	var got struct{ Error, Code string }
	_ = json.Unmarshal(h.LocalResponses[0].Body, &got)
	if got.Code != "orange.model_not_found" {
		t.Errorf("code = %q", got.Code)
	}
	res, _ := st.p.Result()
	if res.Err != "orange.model_not_found" {
		t.Errorf("pending err = %q", res.Err)
	}

	// Cleanup bag.
	nonce := streamObjectNonce(h)
	down.DropStreamObjectBag(nonce)
}

func TestHeaders_skipsNonMatchingPath(t *testing.T) {
	loadTestConfig(t)
	hdr := map[string]string{":method": "GET", ":path": "/healthz"}
	h := testutil.NewFilterHandle(testutil.WithHeaders(hdr))
	w := up.NewWriter(h)
	r := up.NewRequest(h.RequestHeaders(), FilterName)
	var ctx any
	r.Context = &ctx

	requestHandler(w, r)
	if ctx != nil {
		t.Errorf("expected no stream state for non-matching path, got %v", ctx)
	}
	if len(h.LocalResponses) != 0 {
		t.Fatalf("expected no local response, got %+v", h.LocalResponses)
	}
	// No stream object should have been stored.
	nonce := streamObjectNonce(h)
	if nonce != "" {
		t.Errorf("unexpected nonce %q for non-matching request", nonce)
		down.DropStreamObjectBag(nonce)
	}
}

// TestHostpick_getStreamObject: verifies that a FakeClusterLBContext backed
// by the same handle can retrieve the *pending.Pending via GetStreamObject.
func TestHostpick_getStreamObject(t *testing.T) {
	loadTestConfig(t)
	w, h, r, _ := newPostStream(t)
	requestHandler(w, r)

	// Build a FakeClusterLBContext that reads the nonce from the same handle.
	ctx := testutil.NewFakeClusterLBContext(h)
	v, ok := ctx.GetStreamObject(StreamObjectKey)
	if !ok {
		t.Fatal("GetStreamObject returned false — nonce not propagated to filter state")
	}
	p, ok := v.(*pending.Pending)
	if !ok {
		t.Fatalf("type assertion failed: %T", v)
	}
	if p == nil {
		t.Fatal("Pending is nil")
	}

	// Cleanup.
	nonce := streamObjectNonce(h)
	down.DropStreamObjectBag(nonce)
}
