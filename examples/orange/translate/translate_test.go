package translate

import (
	"os"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/dio/transit/examples/orange/classify"
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

	require.Equal(t, "sk-test-anthropic", headerValue(h, "x-api-key"))
	require.Equal(t, "2023-06-01", headerValue(h, "anthropic-version"))
	// authorization must be stripped (per orange.yaml strip_request_headers)
	// before injection so a client-supplied Bearer can't leak to Anthropic.
	require.Empty(t, headerValue(h, "authorization"), "authorization not stripped")
}

func TestHandler_openai_injectsBearer(t *testing.T) {
	loadTestConfig(t)
	w, h, r := newStream(t, "openai_direct")
	handler(w, r)

	require.Equal(t, "Bearer sk-test-openai", headerValue(h, "authorization"))
	// OpenAI doesn't use x-api-key; the client-supplied one must be stripped.
	require.Empty(t, headerValue(h, "x-api-key"), "x-api-key not stripped")
	require.Empty(t, headerValue(h, "anthropic-version"), "anthropic-version unexpectedly set")
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
	require.Equal(t, "Bearer client-supplied", headerValue(h, "authorization"),
		"authorization mutated without upstream metadata")
}
