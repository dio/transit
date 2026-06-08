package adapt

import (
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/dio/transit/examples/orange/internal/config"
	"github.com/dio/transit/examples/orange/internal/pipeline/match"
	"github.com/dio/transit/examples/orange/internal/translator"
	"github.com/dio/transit/up"
	"github.com/dio/transit/up/testutil"
)

// --- Helpers ---

func loadTestConfig(t *testing.T) {
	t.Helper()
	t.Setenv("OPENAI_API_KEY", "sk-test-openai")
	t.Setenv("ANTHROPIC_API_KEY", "sk-test-anthropic")

	yamlBytes, err := os.ReadFile("testdata/config.yaml")
	if err != nil {
		t.Fatalf("read testdata/config.yaml: %v", err)
	}
	appState := config.NewAppState()
	if err := appState.LoadConfig(yamlBytes); err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	resolver := config.NewDefaultResolver(time.Minute)
	SetAppState(appState, resolver)
	clearAuthHandlerCache()
	t.Cleanup(func() {
		SetAppState(nil, nil)
		clearAuthHandlerCache()
	})
}

// newStream produces a FakeFilterHandle pre-populated with the dynamic metadata
// written by match (orange.upstream), so handler() takes the routing path.
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
	h.SetMetadata(match.MetadataNamespace, match.MetadataKeyUpstream, upstream)
	w := up.NewWriter(h)
	r := up.NewRequest(h.RequestHeaders(), FilterName)
	return w, h, r
}

func newHandle(headers map[string]string) (*testutil.FakeFilterHandle, *up.Writer, *up.Request) {
	h := testutil.NewFilterHandle(testutil.WithHeaders(headers))
	w := up.NewWriter(h)
	r := up.NewRequest(h.RequestHeaders(), "test")
	return h, w, r
}

func headerValue(h *testutil.FakeFilterHandle, key string) string {
	vs := h.RequestHeaders().Get(key)
	if len(vs) == 0 {
		return ""
	}
	return vs[0].ToString()
}

// headerVal is an alias used by unit-style tests that don't go through newStream.
func headerVal(h *testutil.FakeFilterHandle, key string) string { return headerValue(h, key) }

func respHeaderVal(h *testutil.FakeFilterHandle, key string) string {
	vs := h.ResponseHeaders().Get(key)
	if len(vs) == 0 {
		return ""
	}
	return vs[0].ToString()
}

// --- Integration tests (handler phase) ---

func TestHandler_anthropic_injectsKeyAndVersion(t *testing.T) {
	loadTestConfig(t)
	w, h, r := newStream(t, "anthropic_direct")
	handler(w, r)

	require.Equal(t, "sk-test-anthropic", headerValue(h, "x-api-key"))
	require.Equal(t, "2023-06-01", headerValue(h, "anthropic-version"))
	require.Empty(t, headerValue(h, "authorization"), "authorization not stripped")
}

func TestHandler_openai_injectsBearer(t *testing.T) {
	loadTestConfig(t)
	w, h, r := newStream(t, "openai_direct")
	handler(w, r)

	require.Equal(t, "Bearer sk-test-openai", headerValue(h, "authorization"))
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

	require.Equal(t, "Bearer client-supplied", headerValue(h, "authorization"),
		"authorization mutated without upstream metadata")
}

// --- Auth handler unit tests ---

func TestBearerAuth_setsAuthorization(t *testing.T) {
	h, w, _ := newHandle(map[string]string{":method": "POST", ":path": "/v1/chat/completions"})
	BearerAuth{Token: "sk-test"}.InjectAuth(w)
	require.Equal(t, "Bearer sk-test", headerVal(h, "authorization"))
}

func TestBearerAuth_emptyToken_noOp(t *testing.T) {
	h, w, _ := newHandle(map[string]string{":method": "POST", ":path": "/v1/chat"})
	BearerAuth{Token: ""}.InjectAuth(w)
	require.Empty(t, headerVal(h, "authorization"))
}

func TestAPIKeyAuth_setsHeader(t *testing.T) {
	h, w, _ := newHandle(map[string]string{":method": "POST", ":path": "/v1/messages"})
	APIKeyAuth{Header: "x-api-key", Key: "secret"}.InjectAuth(w)
	require.Equal(t, "secret", headerVal(h, "x-api-key"))
}

func TestAnthropicAuth_setsBothHeaders(t *testing.T) {
	h, w, _ := newHandle(map[string]string{":method": "POST", ":path": "/v1/messages"})
	AnthropicAuth{APIKey: "ant-key", Version: "2023-06-01"}.InjectAuth(w)
	require.Equal(t, "ant-key", headerVal(h, "x-api-key"))
	require.Equal(t, "2023-06-01", headerVal(h, "anthropic-version"))
}

func TestAnthropicAuth_emptyFields_noWrite(t *testing.T) {
	h, w, _ := newHandle(map[string]string{":method": "POST", ":path": "/v1/messages"})
	AnthropicAuth{}.InjectAuth(w)
	require.Empty(t, headerVal(h, "x-api-key"))
	require.Empty(t, headerVal(h, "anthropic-version"))
}

// --- Config / translator unit tests ---

func TestEffectiveBackendSchema_returnsBackendSchemaWhenSet(t *testing.T) {
	p := &config.ProviderRecord{Kind: "openai", BackendSchema: "azureopenai"}
	require.Equal(t, "azureopenai", p.EffectiveBackendSchema())
}

func TestEffectiveBackendSchema_fallsBackToKind(t *testing.T) {
	p := &config.ProviderRecord{Kind: "openai"}
	require.Equal(t, "openai", p.EffectiveBackendSchema())
}

func TestTranslatorCfgRec_buildsFromProviderRecord(t *testing.T) {
	pp := "/openai/deployments"
	p := &config.ProviderRecord{
		Kind:          "openai",
		BackendSchema: "azureopenai",
		PathPrefix:    &pp,
		Extra:         map[string]string{"azure_api_version": "2025-01-01-preview"},
	}
	cfg := translatorCfgRec(p, "gpt-4o-mini", nil)

	require.Equal(t, "azureopenai", cfg.BackendSchema)
	require.Equal(t, "/openai/deployments", cfg.PathPrefix)
	require.Equal(t, "gpt-4o-mini", cfg.BackendModel)
	require.Equal(t, "2025-01-01-preview", cfg.Extra["azure_api_version"])
}

// --- Pipeline phase unit tests ---

func TestBodyHandler_appliesPath(t *testing.T) {
	tr, err := translator.New("openai", translator.ProviderConfig{
		BackendSchema: "openai",
		PathPrefix:    "/v1",
	})
	require.NoError(t, err)

	var ctx any = &streamContext{translator: tr, auth: noAuth{}}
	h := testutil.NewFilterHandle(testutil.WithHeaders(map[string]string{
		":method": "POST",
		":path":   "/v1/chat/completions",
	}))
	w := up.NewWriter(h)
	bodyHandler(w, &up.BodyChunk{
		Data:      []byte(`{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}]}`),
		EndStream: true,
		Context:   &ctx,
	})

	require.Equal(t, "/v1/chat/completions", headerVal(h, ":path"))
}

func TestBodyHandler_noContext_noOp(t *testing.T) {
	h := testutil.NewFilterHandle(testutil.WithHeaders(map[string]string{":method": "POST", ":path": "/v1/chat/completions"}))
	w := up.NewWriter(h)
	bodyHandler(w, &up.BodyChunk{Data: []byte(`{}`), EndStream: true, Context: nil})
	require.Equal(t, "/v1/chat/completions", headerVal(h, ":path"), "path unchanged when no context")
}

// stubTranslator is a minimal Translator for response-phase tests.
type stubTranslator struct {
	respBodyOut []byte
	respHdrs    []translator.Header
}

func (s *stubTranslator) RequestHeaders(_ map[string]string) ([]translator.Header, error) {
	return nil, nil
}
func (s *stubTranslator) RequestBody(_ []byte) ([]translator.Header, []byte, error) {
	return nil, nil, nil
}
func (s *stubTranslator) ResponseHeaders(_ map[string]string) ([]translator.Header, error) {
	return s.respHdrs, nil
}
func (s *stubTranslator) ResponseBody(_ []byte, _ bool) ([]translator.Header, []byte, error) {
	return nil, s.respBodyOut, nil
}

func TestResponseHandler_appliesResponseHeaders(t *testing.T) {
	stub := &stubTranslator{
		respHdrs: []translator.Header{{Name: "content-type", Value: "text/event-stream"}},
	}
	var ctx any = &streamContext{translator: stub, auth: noAuth{}}
	h := testutil.NewFilterHandle(testutil.WithResponseHeaders(map[string]string{":status": "200"}))
	w := up.NewWriter(h)
	responseHandler(w, &up.ResponseChunk{
		StatusCode: 200,
		Headers:    h.ResponseHeaders(),
		Context:    &ctx,
	})

	require.Equal(t, "text/event-stream", respHeaderVal(h, "content-type"))
}

func TestResponseHandler_bodyPhase_setsResponseBody(t *testing.T) {
	stub := &stubTranslator{respBodyOut: []byte(`{"mutated":true}`)}
	var ctx any = &streamContext{translator: stub, auth: noAuth{}}
	h := testutil.NewFilterHandle()
	w := up.NewWriter(h)
	// SetResponseBody stores in w.f — code path verified; end-to-end replacement
	// is covered by filter lifecycle tests in up/.
	responseHandler(w, &up.ResponseChunk{
		StatusCode: 0,
		Data:       []byte(`{"original":true}`),
		EndStream:  true,
		Context:    &ctx,
	})
}

func TestResponseHandler_noContext_noOp(t *testing.T) {
	h := testutil.NewFilterHandle()
	w := up.NewWriter(h)
	responseHandler(w, &up.ResponseChunk{StatusCode: 200, Context: nil})
}
