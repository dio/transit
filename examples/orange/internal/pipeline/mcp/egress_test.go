package mcp

import (
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/dio/transit/examples/orange/internal/config"
	"github.com/dio/transit/up"
	"github.com/dio/transit/up/testutil"
)

const testMCPYAML = `
providers: {}
models: {}
mcp:
  routes:
    default:
      backends:
        kiwi:
          cluster: orange-mcp-kiwi
          endpoint: https://mcp.kiwi.com
        github:
          cluster: orange-mcp-github
          endpoint: https://api.githubcopilot.com/mcp/
          credential_ref: env://ORANGE_MCP_TEST_GITHUB_TOKEN
`

func setupEgressConfig(t *testing.T) {
	t.Helper()
	t.Setenv("ORANGE_MCP_TEST_GITHUB_TOKEN", "github-test-token")
	cfg, err := config.Load([]byte(testMCPYAML))
	require.NoError(t, err)
	f, err := os.CreateTemp(t.TempDir(), "orange-mcp-egress-*.yaml")
	require.NoError(t, err)
	_, err = f.WriteString(testMCPYAML)
	require.NoError(t, err)
	require.NoError(t, f.Close())
	t.Setenv(config.EnvVar, f.Name())
	config.InitLogger()
	config.MustReload()
	require.Equal(t, "github-test-token", cfg.MCPCredential("default", "github"))
}

func newMCPWriter(headers map[string]string) (*up.Writer, *up.Request, *testutil.FakeFilterHandle) {
	handle := testutil.NewFilterHandle(testutil.WithHeaders(headers))
	w := up.NewWriter(handle)
	r := up.NewRequest(handle.RequestHeaders(), EgressFilterName)
	return w, r, handle
}

func TestMCPEgressHandlerValidHeadersInjectsAuthAuthorityAndMetadata(t *testing.T) {
	setupEgressConfig(t)
	w, r, handle := newMCPWriter(map[string]string{
		headerRoute:     "default",
		headerBackend:   "github",
		headerMethod:    methodToolsCall,
		headerRequestID: "req-1",
		headerTool:      "search_repositories",
	})

	egressHandler(w, r)

	assert.Empty(t, handle.LocalResponses)
	assert.Equal(t, "api.githubcopilot.com", r.Header(up.HeaderAuthority))
	assert.Equal(t, "Bearer github-test-token", r.Header("authorization"))
	assertMCPMetadata(t, handle, metadataRoute, "default")
	assertMCPMetadata(t, handle, metadataBackend, "github")
	assertMCPMetadata(t, handle, metadataMethod, methodToolsCall)
	assertMCPMetadata(t, handle, metadataRequestID, "req-1")
	assertMCPMetadata(t, handle, metadataTool, "search_repositories")
	for _, h := range internalHeaders {
		assert.Empty(t, r.Header(h), "header %q must be stripped", h)
	}
}

func TestMCPEgressHandlerPublicBackendDoesNotInjectAuth(t *testing.T) {
	setupEgressConfig(t)
	w, r, handle := newMCPWriter(map[string]string{
		headerRoute:     "default",
		headerBackend:   "kiwi",
		headerMethod:    methodToolsList,
		headerRequestID: "req-2",
	})

	egressHandler(w, r)

	assert.Empty(t, handle.LocalResponses)
	assert.Equal(t, "mcp.kiwi.com", r.Header(up.HeaderAuthority))
	assert.Empty(t, r.Header("authorization"))
}

func TestMCPEgressHandlerMissingHeadersReturns400AndStrips(t *testing.T) {
	setupEgressConfig(t)
	w, r, handle := newMCPWriter(map[string]string{
		headerRoute: "default",
	})

	egressHandler(w, r)

	require.Len(t, handle.LocalResponses, 1)
	assert.Equal(t, uint32(400), handle.LocalResponses[0].Status)
	for _, h := range internalHeaders {
		assert.Empty(t, r.Header(h), "header %q must be stripped", h)
	}
}

func TestMCPEgressHandlerUnknownBackendReturns400(t *testing.T) {
	setupEgressConfig(t)
	w, _, handle := newMCPWriter(map[string]string{
		headerRoute:     "default",
		headerBackend:   "missing",
		headerMethod:    methodToolsList,
		headerRequestID: "req-3",
	})

	egressHandler(w, up.NewRequest(handle.RequestHeaders(), EgressFilterName))

	require.Len(t, handle.LocalResponses, 1)
	assert.Equal(t, uint32(400), handle.LocalResponses[0].Status)
}

func assertMCPMetadata(t *testing.T, handle *testutil.FakeFilterHandle, key, want string) {
	t.Helper()
	got, ok := handle.Metadata(metadataNamespace, key)
	require.True(t, ok)
	assert.Equal(t, want, got)
}
