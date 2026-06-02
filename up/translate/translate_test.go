package translate_test

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/dio/transit/up"
	"github.com/dio/transit/up/testutil"
	"github.com/dio/transit/up/translate"
)

func newHandle(headers map[string]string) (*testutil.FakeFilterHandle, *up.Writer, *up.Request) {
	h := testutil.NewFilterHandle(testutil.WithHeaders(headers))
	w := up.NewWriter(h)
	r := up.NewRequest(h.RequestHeaders(), "test")
	return h, w, r
}

func headerVal(h *testutil.FakeFilterHandle, key string) string {
	vs := h.RequestHeaders().Get(key)
	if len(vs) == 0 {
		return ""
	}
	return vs[0].ToString()
}

// --- BackendAuthHandler tests ---

func TestBearerAuth_setsAuthorization(t *testing.T) {
	h, w, _ := newHandle(map[string]string{":method": "POST", ":path": "/v1/chat/completions"})
	translate.BearerAuth{Token: "sk-test"}.InjectAuth(w)
	require.Equal(t, "Bearer sk-test", headerVal(h, "authorization"))
}

func TestBearerAuth_emptyToken_noOp(t *testing.T) {
	h, w, _ := newHandle(map[string]string{":method": "POST", ":path": "/v1/chat"})
	translate.BearerAuth{Token: ""}.InjectAuth(w)
	require.Empty(t, headerVal(h, "authorization"))
}

func TestAPIKeyAuth_setsHeader(t *testing.T) {
	h, w, _ := newHandle(map[string]string{":method": "POST", ":path": "/v1/messages"})
	translate.APIKeyAuth{Header: "x-api-key", Key: "secret"}.InjectAuth(w)
	require.Equal(t, "secret", headerVal(h, "x-api-key"))
}

func TestAnthropicAuth_setsBothHeaders(t *testing.T) {
	h, w, _ := newHandle(map[string]string{":method": "POST", ":path": "/v1/messages"})
	translate.AnthropicAuth{APIKey: "ant-key", Version: "2023-06-01"}.InjectAuth(w)
	require.Equal(t, "ant-key", headerVal(h, "x-api-key"))
	require.Equal(t, "2023-06-01", headerVal(h, "anthropic-version"))
}

func TestAnthropicAuth_emptyFields_noWrite(t *testing.T) {
	h, w, _ := newHandle(map[string]string{":method": "POST", ":path": "/v1/messages"})
	translate.AnthropicAuth{}.InjectAuth(w)
	require.Empty(t, headerVal(h, "x-api-key"))
	require.Empty(t, headerVal(h, "anthropic-version"))
}

// --- RouteFor tests ---

func TestRouteFor_openAIToOpenAI_injectsBearer(t *testing.T) {
	h, w, r := newHandle(map[string]string{
		":method":       "POST",
		":path":         "/v1/chat/completions",
		"authorization": "Bearer client-token",
	})
	route := translate.RouteFor(translate.SchemaOpenAI, translate.ProviderConfig{
		Schema: translate.SchemaOpenAI,
		Secret: "upstream-token",
	})
	route.Apply(w, r, []string{"authorization"})

	require.Equal(t, "Bearer upstream-token", headerVal(h, "authorization"))
}

func TestRouteFor_openAIToAnthropic_injectsAnthropicAuth(t *testing.T) {
	h, w, r := newHandle(map[string]string{
		":method":       "POST",
		":path":         "/v1/chat/completions",
		"authorization": "Bearer client-token",
		"x-api-key":     "client-key",
	})
	route := translate.RouteFor(translate.SchemaOpenAI, translate.ProviderConfig{
		Schema:     translate.SchemaAnthropic,
		Secret:     "ant-secret",
		Extra:      "2023-06-01",
		PathPrefix: "/v1",
	})
	route.Apply(w, r, []string{"authorization", "x-api-key"})

	require.Empty(t, headerVal(h, "authorization"), "client auth not stripped")
	require.Equal(t, "ant-secret", headerVal(h, "x-api-key"))
	require.Equal(t, "2023-06-01", headerVal(h, "anthropic-version"))
}

func TestRouteFor_pathRewrite_appliedWhenPrefixDiffers(t *testing.T) {
	h, w, r := newHandle(map[string]string{
		":method": "POST",
		":path":   "/v1/messages",
	})
	route := translate.RouteFor(translate.SchemaOpenAI, translate.ProviderConfig{
		Schema:     translate.SchemaOpenAI,
		PathPrefix: "/api/v1",
		Secret:     "tok",
	})
	route.Apply(w, r, nil)
	require.Equal(t, "/api/v1/messages", headerVal(h, ":path"))
}

func TestRouteFor_noPathRewrite_whenPrefixIsV1(t *testing.T) {
	h, w, r := newHandle(map[string]string{
		":method": "POST",
		":path":   "/v1/chat/completions",
	})
	route := translate.RouteFor(translate.SchemaOpenAI, translate.ProviderConfig{
		Schema:     translate.SchemaOpenAI,
		PathPrefix: "/v1",
		Secret:     "tok",
	})
	route.Apply(w, r, nil)
	require.Equal(t, "/v1/chat/completions", headerVal(h, ":path"))
}

func TestRouteFor_unknownClientSchema_noAuth(t *testing.T) {
	h, w, r := newHandle(map[string]string{":method": "POST", ":path": "/v1/chat"})
	route := translate.RouteFor("grpc", translate.ProviderConfig{
		Schema: translate.SchemaOpenAI,
		Secret: "tok",
	})
	route.Apply(w, r, nil)
	require.Empty(t, headerVal(h, "authorization"))
}
