package classify

import (
	"encoding/json"
	"os"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/dio/transit/down"
	"github.com/dio/transit/examples/orange/config"
	"github.com/dio/transit/up"
	"github.com/dio/transit/up/testutil"
)

func loadTestConfig(t *testing.T) {
	t.Helper()
	t.Setenv("OPENAI_API_KEY", "sk-test-openai")
	t.Setenv("ANTHROPIC_API_KEY", "sk-test-anthropic")
	require.NoError(t, os.Setenv(config.EnvVar, "../orange.yaml"))
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
	require.True(t, ok && st != nil, "expected stream state to be stashed in r.Context")
	require.NotNil(t, st.p, "promise is nil")

	// The promise must be in the stream-object bag under DecisionKey.
	nonce := streamObjectNonce(h)
	require.NotEmpty(t, nonce, "stream-object nonce not written to filter state")

	bag, ok := down.LookupStreamObjectBag(nonce)
	require.True(t, ok, "bag not found for nonce")

	v, ok := bag.Get(DecisionKey.Key())
	require.True(t, ok, "DecisionKey not in bag")

	promise, ok := v.(*up.StreamPromise[Decision])
	require.True(t, ok, "bag entry has wrong type: %T", v)
	require.Same(t, st.p, promise, "bag entry is not the same promise as streamState.p")

	// Cleanup: drain the bag (normally done by OnStreamComplete).
	down.DropStreamObjectBag(nonce)
}

func TestBody_knownModel_resolvesUpstream(t *testing.T) {
	loadTestConfig(t)
	body := []byte(`{"model":"gpt-4o-mini","messages":[]}`)
	h, st := runFlow(t, body)

	require.Empty(t, h.LocalResponses, "unexpected local response")

	res, ok := st.p.Result()
	require.True(t, ok, "promise not resolved")
	require.Empty(t, res.Err)
	require.Equal(t, "openai_direct", res.Provider)
	require.Equal(t, "openai", res.Kind)
	require.Equal(t, "gpt-4o-mini", res.Model)

	v, ok := h.Metadata(MetadataNamespace, MetadataKeyUpstream)
	require.True(t, ok)
	require.Equal(t, "openai_direct", v)

	// Bag entry must still be present before onStreamComplete (SDK owns the drain).
	nonce := streamObjectNonce(h)
	require.NotEmpty(t, nonce, "nonce missing from filter state")
	_, ok = down.LookupStreamObjectBag(nonce)
	require.True(t, ok, "bag entry unexpectedly missing before stream complete")

	// Simulate OnStreamComplete: call onStreamComplete then drain the bag.
	var ctx any = st
	onStreamComplete(&ctx)
	down.DropStreamObjectBag(nonce)

	// After drain, bag must be gone.
	_, ok = down.LookupStreamObjectBag(nonce)
	require.False(t, ok, "bag entry not gone after stream complete + drain")

	// Resolve must remain the bodyHandler's (CAS loses races silently).
	got, _ := st.p.Result()
	require.Empty(t, got.Err, "post-cleanup err should be empty (CAS should preserve body result)")
}

func TestOnStreamComplete_cleansUpWhenBodyNeverRan(t *testing.T) {
	loadTestConfig(t)
	// Headers-only flow: simulates downstream disconnect / timeout after
	// headers and before the body handler runs.
	h, st := runFlow(t, nil)
	require.NotNil(t, st, "expected stream state from headers phase")

	nonce := streamObjectNonce(h)
	require.NotEmpty(t, nonce, "nonce missing from filter state")

	_, ok := down.LookupStreamObjectBag(nonce)
	require.True(t, ok, "bag entry missing before onStreamComplete")

	var ctx any = st
	onStreamComplete(&ctx)

	// onStreamComplete must have resolved the promise with ErrStreamTerminated.
	res, ok := st.p.Result()
	require.True(t, ok, "promise should be resolved by onStreamComplete")
	require.Equal(t, ErrStreamTerminated, res.Err)

	// SDK drains the bag; simulate that here.
	down.DropStreamObjectBag(nonce)
	_, ok = down.LookupStreamObjectBag(nonce)
	require.False(t, ok, "bag still present after drain")
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

	require.Len(t, h.LocalResponses, 1)
	require.EqualValues(t, 400, h.LocalResponses[0].Status)

	var got struct{ Error, Code string }
	_ = json.Unmarshal(h.LocalResponses[0].Body, &got)
	require.Equal(t, ErrModelRequired, got.Code)

	res, _ := st.p.Result()
	require.Equal(t, ErrModelRequired, res.Err)

	down.DropStreamObjectBag(streamObjectNonce(h))
}

func TestBody_unknownModel_404(t *testing.T) {
	loadTestConfig(t)
	body := []byte(`{"model":"gemini-1.5"}`)
	h, st := runFlow(t, body)

	require.Len(t, h.LocalResponses, 1)
	require.EqualValues(t, 404, h.LocalResponses[0].Status)

	var got struct{ Error, Code string }
	_ = json.Unmarshal(h.LocalResponses[0].Body, &got)
	require.Equal(t, "orange.model_not_found", got.Code)

	res, _ := st.p.Result()
	require.Equal(t, "orange.model_not_found", res.Err)

	down.DropStreamObjectBag(streamObjectNonce(h))
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
	require.Nil(t, ctx, "expected no stream state for non-matching path")
	require.Empty(t, h.LocalResponses)

	nonce := streamObjectNonce(h)
	if nonce != "" {
		down.DropStreamObjectBag(nonce)
		t.Errorf("unexpected nonce %q for non-matching request", nonce)
	}
}

// TestHostpick_getStreamObject: verifies that a FakeClusterLBContext backed
// by the same handle can retrieve the promise via DecisionKey.GetFromCtx.
func TestHostpick_getStreamObject(t *testing.T) {
	loadTestConfig(t)
	w, h, r, _ := newPostStream(t)
	requestHandler(w, r)

	ctx := testutil.NewFakeClusterLBContext(h)
	promise, ok := DecisionKey.GetFromCtx(ctx)
	require.True(t, ok, "DecisionKey.GetFromCtx returned false — nonce not propagated to filter state")
	require.NotNil(t, promise)

	down.DropStreamObjectBag(streamObjectNonce(h))
}
