package classify

import (
	"encoding/json"
	"os"
	"testing"

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

func TestHeaders_registersPendingAndToken(t *testing.T) {
	loadTestConfig(t)
	_, st := runFlow(t, nil)
	if st == nil {
		t.Fatal("expected stream state to be stashed in r.Context")
	}
	if st.token == "" {
		t.Error("token is empty")
	}
	if pending.Lookup(st.token) != st.p {
		t.Error("pending not registered under token")
	}
	pending.Delete(st.token)
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
	// Cleanup is owned by onStreamComplete now; the entry survives until then.
	if pending.Lookup(st.token) != st.p {
		t.Error("token unexpectedly missing from registry before stream complete")
	}
	var ctx any = st
	onStreamComplete(&ctx)
	if pending.Lookup(st.token) != nil {
		t.Error("token not deleted from registry after onStreamComplete")
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
	_, st := runFlow(t, nil)
	if st == nil {
		t.Fatal("expected stream state from headers phase")
	}

	var ctx any = st
	onStreamComplete(&ctx)

	if pending.Lookup(st.token) != nil {
		t.Error("token not deleted from registry after onStreamComplete")
	}
	res, ok := st.p.Result()
	if !ok {
		t.Fatal("pending should be resolved by onStreamComplete")
	}
	if res.Err != ErrStreamTerminated {
		t.Errorf("err = %q, want %q", res.Err, ErrStreamTerminated)
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
}
