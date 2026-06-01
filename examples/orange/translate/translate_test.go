package translate

import (
	"os"
	"testing"

	"github.com/dio/transit/examples/orange/classify"
	"github.com/dio/transit/examples/orange/config"
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

// newStream produces a FakeFilterHandle pre-populated with the dynamic
// metadata classify writes (orange.upstream), so handler() takes the routing
// path instead of the early "no upstream metadata" return.
func newStream(t *testing.T, upstream string) (*up.Writer, *testutil.FakeFilterHandle, *up.Request) {
	t.Helper()
	hdr := map[string]string{
		":method":       "POST",
		":path":         "/v1/messages",
		"content-type":  "application/json",
		"authorization": "Bearer client-supplied",
		"x-api-key":     "client-supplied",
	}
	h := testutil.NewFilterHandle(testutil.WithHeaders(hdr))
	h.SetMetadata(classify.MetadataNamespace, classify.MetadataKeyUpstream, upstream)
	w := up.NewWriter(h)
	r := up.NewRequest(h.RequestHeaders(), FilterName)
	return w, h, r
}

func headerValue(h *testutil.FakeFilterHandle, key string) string {
	vs := h.RequestHeaders().Get(key)
	if len(vs) == 0 {
		return ""
	}
	return vs[0].ToString()
}

func TestHandler_anthropic_injectsKeyAndVersion(t *testing.T) {
	loadTestConfig(t)
	w, h, r := newStream(t, "anthropic_direct")
	handler(w, r)

	if got := headerValue(h, "x-api-key"); got != "sk-test-anthropic" {
		t.Errorf("x-api-key = %q, want sk-test-anthropic", got)
	}
	if got := headerValue(h, "anthropic-version"); got != "2023-06-01" {
		t.Errorf("anthropic-version = %q, want 2023-06-01", got)
	}
	// authorization must be stripped (per orange.yaml strip_request_headers)
	// before injection so a client-supplied Bearer can't leak to Anthropic.
	if got := headerValue(h, "authorization"); got != "" {
		t.Errorf("authorization not stripped: got %q", got)
	}
}

func TestHandler_openai_injectsBearer(t *testing.T) {
	loadTestConfig(t)
	w, h, r := newStream(t, "openai_direct")
	handler(w, r)

	if got := headerValue(h, "authorization"); got != "Bearer sk-test-openai" {
		t.Errorf("authorization = %q, want Bearer sk-test-openai", got)
	}
	// OpenAI doesn't use x-api-key; the client-supplied one must be stripped
	// even when the chosen provider has no use for it.
	if got := headerValue(h, "x-api-key"); got != "" {
		t.Errorf("x-api-key not stripped: got %q", got)
	}
	if got := headerValue(h, "anthropic-version"); got != "" {
		t.Errorf("anthropic-version unexpectedly set: %q", got)
	}
}

func TestHandler_noUpstream_noChanges(t *testing.T) {
	loadTestConfig(t)
	h := testutil.NewFilterHandle(testutil.WithHeaders(map[string]string{
		":method":       "POST",
		":path":         "/v1/messages",
		"authorization": "Bearer client-supplied",
	}))
	w := up.NewWriter(h)
	r := up.NewRequest(h.RequestHeaders(), FilterName)
	handler(w, r)

	// Without metadata, translate must not touch headers — leave the request
	// untouched so a misconfigured pipeline surfaces as a clear upstream error
	// rather than a silently-stripped client Authorization.
	if got := headerValue(h, "authorization"); got != "Bearer client-supplied" {
		t.Errorf("authorization mutated without upstream metadata: %q", got)
	}
}
