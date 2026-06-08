package responsesws

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/dio/transit/examples/orange/internal/config"
	"github.com/dio/transit/examples/orange/internal/pipeline/match"
	"github.com/dio/transit/up"
	"github.com/dio/transit/up/testutil"
)

// testOrangeYAML is a minimal orange.yaml fixture for egress filter tests.
// Uses a literal bearer token so secret resolution succeeds without real env vars.
const testOrangeYAML = `
llm:
  providers:
    openai:
      kind: openai
      endpoint: https://api.openai.com
      auth:
        type: bearer
        secret_ref: env://ORANGE_RESPONSESWS_TEST_OPENAI_KEY

  models:
    gpt-4o-mini:
      provider: openai
`

// setupTestConfig loads testOrangeYAML into a fresh AppState and wires it as
// the responsesws AppState. Cleans up after the test.
func setupTestConfig(t *testing.T) {
	t.Helper()
	t.Setenv("ORANGE_RESPONSESWS_TEST_OPENAI_KEY", "sk-test-unit-only")

	appState := config.NewAppState()
	require.NoError(t, appState.LoadConfig([]byte(testOrangeYAML)))
	resolver := config.NewDefaultResolver(time.Minute)
	SetAppState(appState, resolver)
	t.Cleanup(func() { SetAppState(nil, nil) })
}

// newEgressWriter builds an up.Writer and up.Request backed by a FakeFilterHandle
// with the given request headers.
func newEgressWriter(headers map[string]string) (*up.Writer, *up.Request, *testutil.FakeFilterHandle) {
	handle := testutil.NewFilterHandle(testutil.WithHeaders(headers))
	w := up.NewWriter(handle)
	r := up.NewRequest(handle.RequestHeaders(), EgressFilterName)
	return w, r, handle
}

// ---- egressHandler: valid headers accept path ----------------------------------

func TestEgressHandler_validHeaders_writesDecision(t *testing.T) {
	setupTestConfig(t)

	w, r, handle := newEgressWriter(map[string]string{
		headerProvider:     "openai",
		headerKind:         "openai",
		headerModel:        "gpt-4o-mini",
		headerBackendModel: "gpt-4o-mini",
	})

	egressHandler(w, r)

	// No local reply should have been sent.
	assert.Empty(t, handle.LocalResponses, "expected no local reply for valid headers")

	// Decision.Apply writes filter state.
	v, ok := handle.FilterStateString(match.StateUpstream)
	require.True(t, ok)
	assert.Equal(t, "openai", v)

	v, ok = handle.FilterStateString(match.StateProvider)
	require.True(t, ok)
	assert.Equal(t, "openai", v)

	v, ok = handle.FilterStateString(match.StateModel)
	require.True(t, ok)
	assert.Equal(t, "gpt-4o-mini", v)
}

func TestEgressHandler_validHeaders_writesMetadata(t *testing.T) {
	setupTestConfig(t)

	w, r, handle := newEgressWriter(map[string]string{
		headerProvider:     "openai",
		headerKind:         "openai",
		headerModel:        "gpt-4o-mini",
		headerBackendModel: "gpt-4o-mini",
	})

	egressHandler(w, r)

	upstream, ok := handle.Metadata(match.MetadataNamespace, match.MetadataKeyUpstream)
	require.True(t, ok)
	assert.Equal(t, "openai", upstream)

	provider, ok := handle.Metadata(match.MetadataNamespace, match.MetadataKeyProvider)
	require.True(t, ok)
	assert.Equal(t, "openai", provider)

	bm, ok := handle.Metadata(match.MetadataNamespace, match.MetadataKeyBackendModel)
	require.True(t, ok)
	assert.Equal(t, "gpt-4o-mini", bm)

	endpoint, ok := handle.Metadata(match.MetadataNamespace, match.MetadataKeyEndpoint)
	require.True(t, ok)
	assert.Equal(t, match.EndpointResponses, endpoint)
}

func TestEgressHandler_validHeaders_injectsHeaderAuthAndAuthority(t *testing.T) {
	setupTestConfig(t)

	w, r, _ := newEgressWriter(map[string]string{
		headerProvider:     "openai",
		headerKind:         "openai",
		headerModel:        "gpt-4o-mini",
		headerBackendModel: "gpt-4o-mini",
	})

	egressHandler(w, r)

	assert.Equal(t, "api.openai.com", r.Header(up.HeaderAuthority))
	assert.Equal(t, "Bearer sk-test-unit-only", r.Header("authorization"))
}

// ---- egressHandler: header stripping ------------------------------------------

func TestEgressHandler_stripsInternalHeaders_onAccept(t *testing.T) {
	setupTestConfig(t)

	handle := testutil.NewFilterHandle(testutil.WithHeaders(map[string]string{
		headerProvider:     "openai",
		headerKind:         "openai",
		headerModel:        "gpt-4o-mini",
		headerBackendModel: "gpt-4o-mini",
	}))
	w := up.NewWriter(handle)
	r := up.NewRequest(handle.RequestHeaders(), EgressFilterName)

	egressHandler(w, r)

	// All four internal headers must be removed regardless of accept/reject path.
	for _, h := range internalHeaders {
		assert.Empty(t, r.Header(h), "header %q must be stripped after egressHandler", h)
	}
}

func TestEgressHandler_stripsInternalHeaders_onReject(t *testing.T) {
	setupTestConfig(t)

	// Provide only partial headers — should trigger a local reply.
	w, r, handle := newEgressWriter(map[string]string{
		headerProvider: "openai",
		// kind, model, backend-model are missing
	})

	egressHandler(w, r)

	// A local reply should have been sent.
	require.Len(t, handle.LocalResponses, 1)
	assert.Equal(t, uint32(400), handle.LocalResponses[0].Status)

	// Headers are stripped even on the reject path.
	for _, h := range internalHeaders {
		assert.Empty(t, r.Header(h), "header %q must be stripped on reject path", h)
	}
}

// ---- egressHandler: missing headers -------------------------------------------

func TestEgressHandler_missingProvider_returns400(t *testing.T) {
	setupTestConfig(t)
	w, r, handle := newEgressWriter(map[string]string{
		// headerProvider intentionally missing
		headerKind:         "openai",
		headerModel:        "gpt-4o-mini",
		headerBackendModel: "gpt-4o-mini",
	})
	egressHandler(w, r)
	require.Len(t, handle.LocalResponses, 1)
	assert.Equal(t, uint32(400), handle.LocalResponses[0].Status)
}

func TestEgressHandler_missingKind_returns400(t *testing.T) {
	setupTestConfig(t)
	w, r, handle := newEgressWriter(map[string]string{
		headerProvider:     "openai",
		headerModel:        "gpt-4o-mini",
		headerBackendModel: "gpt-4o-mini",
	})
	egressHandler(w, r)
	require.Len(t, handle.LocalResponses, 1)
	assert.Equal(t, uint32(400), handle.LocalResponses[0].Status)
}

func TestEgressHandler_missingModel_returns400(t *testing.T) {
	setupTestConfig(t)
	w, r, handle := newEgressWriter(map[string]string{
		headerProvider:     "openai",
		headerKind:         "openai",
		headerBackendModel: "gpt-4o-mini",
	})
	egressHandler(w, r)
	require.Len(t, handle.LocalResponses, 1)
	assert.Equal(t, uint32(400), handle.LocalResponses[0].Status)
}

func TestEgressHandler_missingBackendModel_returns400(t *testing.T) {
	setupTestConfig(t)
	w, r, handle := newEgressWriter(map[string]string{
		headerProvider: "openai",
		headerKind:     "openai",
		headerModel:    "gpt-4o-mini",
	})
	egressHandler(w, r)
	require.Len(t, handle.LocalResponses, 1)
	assert.Equal(t, uint32(400), handle.LocalResponses[0].Status)
}

// ---- egressHandler: config-inconsistent headers --------------------------------

func TestEgressHandler_inconsistentProvider_returns400(t *testing.T) {
	setupTestConfig(t)
	w, r, handle := newEgressWriter(map[string]string{
		headerProvider:     "wrong-provider", // does not match config
		headerKind:         "openai",
		headerModel:        "gpt-4o-mini",
		headerBackendModel: "gpt-4o-mini",
	})
	egressHandler(w, r)
	require.Len(t, handle.LocalResponses, 1)
	assert.Equal(t, uint32(400), handle.LocalResponses[0].Status)
}

func TestEgressHandler_inconsistentKind_returns400(t *testing.T) {
	setupTestConfig(t)
	w, r, handle := newEgressWriter(map[string]string{
		headerProvider:     "openai",
		headerKind:         "wrong-kind", // does not match config
		headerModel:        "gpt-4o-mini",
		headerBackendModel: "gpt-4o-mini",
	})
	egressHandler(w, r)
	require.Len(t, handle.LocalResponses, 1)
	assert.Equal(t, uint32(400), handle.LocalResponses[0].Status)
}

func TestEgressHandler_unknownModel_returns400(t *testing.T) {
	setupTestConfig(t)
	w, r, handle := newEgressWriter(map[string]string{
		headerProvider:     "openai",
		headerKind:         "openai",
		headerModel:        "unknown-model-xyz",
		headerBackendModel: "unknown-model-xyz",
	})
	egressHandler(w, r)
	require.Len(t, handle.LocalResponses, 1)
	assert.Equal(t, uint32(400), handle.LocalResponses[0].Status)
}

// ---- EgressFilterName constant -------------------------------------------------

func TestEgressFilterName_constant(t *testing.T) {
	assert.Equal(t, "orange-responsesws-egress-match", EgressFilterName)
}
