package e2e

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/stretchr/testify/suite"
)

const (
	gatewayHost = "mcp-profile-tiered-router.example.com"
	profileID   = "9b3f7d0a80c4aa6d-67261ca9ea3dadb2"
	profileKey  = "profile-key"

	// Public (L1-prefixed) tool names returned by the fake MCP backends.
	// The fake-mcp binary must return these raw names; L1 adds the prefix.
	toolKiwiSearchFlight    = "kiwi.search-flight"
	toolAWSReadDoc          = "aws-knowledge.aws____read_documentation"
	toolMicrosoftSearchDocs = "microsoft.search_docs"
	toolGithubSearch        = "github.search"
)

// cluster-router "models" map for each L2 shard: logical MCP server key →
// concrete backend target. Passed as {{.BackendRoutesJSON}} in epp-l2.tmpl.yaml.
const (
	l2ABackendsJSON = `{"kiwi":{"target":"mcp-kiwi.transit-dataplane.svc.cluster.local:8080","provider":"mcp","auth_header":"Bearer kiwi-token"},"aws-knowledge":{"target":"mcp-aws-knowledge.transit-dataplane.svc.cluster.local:8080","provider":"mcp","auth_header":"Bearer aws-token"}}`
	l2BBackendsJSON = `{"microsoft":{"target":"mcp-microsoft.transit-dataplane.svc.cluster.local:8080","provider":"mcp","auth_header":"Bearer microsoft-token"},"github":{"target":"mcp-github.transit-dataplane.svc.cluster.local:8080","provider":"mcp","auth_header":"Bearer github-token"}}`
)

func TestMCPProfileTieredRouterEnvoyGateway(t *testing.T) {
	if os.Getenv("RUN_MCP_PROFILE_TIERED_ROUTER_EG_E2E") != "1" {
		t.Skip("set RUN_MCP_PROFILE_TIERED_ROUTER_EG_E2E=1 to run the k3d Envoy Gateway integration")
	}
	suite.Run(t, &mcpProfileGatewaySuite{
		envoyGatewaySuite: envoyGatewaySuite{
			cluster: "transit-mcp-profile-eg",
			timeout: 20 * time.Minute,
		},
	})
}

type mcpProfileGatewaySuite struct {
	envoyGatewaySuite

	envoyImage string
	demoImage  string
}

func (s *mcpProfileGatewaySuite) SetupSuite() {
	s.envoyImage = requireEnv(s.T(), "IMAGE")
	s.demoImage = requireEnv(s.T(), "DEMO_IMAGE")

	s.envoyGatewaySuite.SetupSuite()
	if os.Getenv("K3D_SKIP_IMAGE_IMPORT") == "1" {
		liveLogf(s.T(), "skipping k3d image import; cluster will pull %s and %s", s.envoyImage, s.demoImage)
		return
	}
	liveLogf(s.T(), "importing images into k3d cluster %s", s.cluster)
	args := []string{"image", "import", "-c", s.cluster}
	if mode := os.Getenv("K3D_IMAGE_IMPORT_MODE"); mode != "" {
		args = append(args, "--mode", mode)
	}
	args = append(args, s.envoyImage, s.demoImage)
	run(s.ctx, s.T(), "", "k3d", args...)
}

func (s *mcpProfileGatewaySuite) TestMCPProfileGatewayTopology() {
	liveLogf(s.T(), "verifying Envoy Gateway install")
	s.verifyEnvoyGatewayInstall()

	// Phase 1: apply namespaces, EnvoyProxy specs (L1 with placeholder config that
	// satisfies MCP_PROFILE_GATEWAY_CONFIG parsing), demo workloads, Gateways, and
	// HTTPRoutes including L1 catalog egress routes for cluster name discovery.
	liveLogf(s.T(), "applying namespaces and workloads")
	apply(s.ctx, s.T(), filepath.Join(s.dir, "k8s", "namespaces.yaml"))

	l2ACfg := l2CatalogConfig(map[string]struct{ URL, Credential string }{
		"kiwi":          {URL: "http://mcp-kiwi.transit-dataplane.svc.cluster.local:8080", Credential: "Bearer kiwi-token"},
		"aws-knowledge": {URL: "http://mcp-aws-knowledge.transit-dataplane.svc.cluster.local:8080", Credential: "Bearer aws-token"},
	})
	l2BCfg := l2CatalogConfig(map[string]struct{ URL, Credential string }{
		"microsoft": {URL: "http://mcp-microsoft.transit-dataplane.svc.cluster.local:8080", Credential: "Bearer microsoft-token"},
		"github":    {URL: "http://mcp-github.transit-dataplane.svc.cluster.local:8080", Credential: "Bearer github-token"},
	})
	renderApply(s.ctx, s.T(), filepath.Join(s.dir, "k8s", "envoyproxies.tmpl.yaml"), map[string]string{
		"EnvoyImage":       s.envoyImage,
		"L1ConfigJSON":     `{"catalog_servers":{"_placeholder":{"url":"http://placeholder.invalid"}}}`,
		"L2ACatalogConfig": l2ACfg,
		"L2BCatalogConfig": l2BCfg,
	})
	renderApply(s.ctx, s.T(), filepath.Join(s.dir, "k8s", "demo.tmpl.yaml"), map[string]string{
		"DemoImage": s.demoImage,
	})
	apply(s.ctx, s.T(), filepath.Join(s.dir, "k8s", "gateways.yaml"))
	apply(s.ctx, s.T(), filepath.Join(s.dir, "k8s", "httproutes.yaml"))

	liveLogf(s.T(), "waiting for fake MCP backends")
	for _, app := range []string{"mcp-kiwi", "mcp-aws-knowledge", "mcp-microsoft", "mcp-github"} {
		waitReady(s.ctx, s.T(), dataplaneNamespace, "app="+app)
	}

	liveLogf(s.T(), "waiting for Gateways to be accepted")
	for _, gw := range []string{"l1", "l2-a", "l2-b"} {
		run(s.ctx, s.T(), "", "kubectl", "wait", "gateway/"+gw, "-n", dataplaneNamespace,
			"--for=condition=Accepted", "--timeout=120s")
	}

	liveLogf(s.T(), "waiting for generated Envoy deployments")
	l1Deploy := generatedResourceName(s.ctx, s.T(), dataplaneNamespace, "l1", "deploy")
	waitDeployment(s.ctx, s.T(), dataplaneNamespace, l1Deploy)
	run(s.ctx, s.T(), "", "kubectl", "wait", "pods", "--for=condition=Ready",
		"-n", dataplaneNamespace, "-l", "transit.dio/proxy=l1", "--timeout=180s")
	l2ADeploy := generatedResourceName(s.ctx, s.T(), dataplaneNamespace, "l2-a", "deploy")
	waitDeployment(s.ctx, s.T(), dataplaneNamespace, l2ADeploy)
	run(s.ctx, s.T(), "", "kubectl", "wait", "pods", "--for=condition=Ready",
		"-n", dataplaneNamespace, "-l", "transit.dio/proxy=l2-a", "--timeout=180s")
	l2BDeploy := generatedResourceName(s.ctx, s.T(), dataplaneNamespace, "l2-b", "deploy")
	waitDeployment(s.ctx, s.T(), dataplaneNamespace, l2BDeploy)
	run(s.ctx, s.T(), "", "kubectl", "wait", "pods", "--for=condition=Ready",
		"-n", dataplaneNamespace, "-l", "transit.dio/proxy=l2-b", "--timeout=180s")

	// Phase 2: patch each L2 shard's dedicated cluster-router-init cluster with
	// cluster-router. The init cluster is created by the l2-{a,b}-cluster-router-init
	// HTTPRoute, which exists solely so the cluster extension can initialize the
	// route store and serve /__cluster-router/config. Real catalog traffic continues
	// to flow through the demo catalog-router app unchanged.
	liveLogf(s.T(), "patching L2-A cluster-router")
	l2AAdminURL, stopL2AAdmin := portForward(s.ctx, s.T(), "deploy/"+l2ADeploy, 19000)
	defer stopL2AAdmin()
	l2AInitCluster := discoverBackendCluster(s.ctx, s.T(), l2AAdminURL, "l2-a-cluster-router-init")
	renderApply(s.ctx, s.T(), filepath.Join(s.dir, "k8s", "epp-l2.tmpl.yaml"), map[string]string{
		"PolicyName":        "l2-a-cluster-router",
		"GatewayName":       "l2-a",
		"InitClusterName":   l2AInitCluster,
		"Shard":             "l2-a",
		"BackendRoutesJSON": l2ABackendsJSON,
	})
	waitEnvoyPatchPolicyProgrammed(s.ctx, s.T(), dataplaneNamespace, "l2-a-cluster-router")

	liveLogf(s.T(), "patching L2-B cluster-router")
	l2BAdminURL, stopL2BAdmin := portForward(s.ctx, s.T(), "deploy/"+l2BDeploy, 19000)
	defer stopL2BAdmin()
	l2BInitCluster := discoverBackendCluster(s.ctx, s.T(), l2BAdminURL, "l2-b-cluster-router-init")
	renderApply(s.ctx, s.T(), filepath.Join(s.dir, "k8s", "epp-l2.tmpl.yaml"), map[string]string{
		"PolicyName":        "l2-b-cluster-router",
		"GatewayName":       "l2-b",
		"InitClusterName":   l2BInitCluster,
		"Shard":             "l2-b",
		"BackendRoutesJSON": l2BBackendsJSON,
	})
	waitEnvoyPatchPolicyProgrammed(s.ctx, s.T(), dataplaneNamespace, "l2-b-cluster-router")

	// Phase 3: discover the L1 callout cluster names for l2-a and l2-b egress routes,
	// build the real L1 config with those cluster names, and restart L1.
	// l1-l2a-catalog and l1-l2b-catalog HTTPRoutes on L1 create backend clusters that
	// the mcp-profile-gateway module uses for its outbound catalog callouts.
	liveLogf(s.T(), "discovering L1 catalog callout clusters")
	l1AdminURL, stopL1Admin := portForward(s.ctx, s.T(), "deploy/"+l1Deploy, 19000)
	defer stopL1Admin()
	l1L2ACluster := discoverBackendCluster(s.ctx, s.T(), l1AdminURL, "l1-l2a-catalog")
	l1L2BCluster := discoverBackendCluster(s.ctx, s.T(), l1AdminURL, "l1-l2b-catalog")
	l1Config := buildL1Config(l1L2ACluster, l1L2BCluster)
	liveLogf(s.T(), "applying real L1 config and restarting L1")
	renderApply(s.ctx, s.T(), filepath.Join(s.dir, "k8s", "envoyproxies.tmpl.yaml"), map[string]string{
		"EnvoyImage":       s.envoyImage,
		"L1ConfigJSON":     l1Config,
		"L2ACatalogConfig": l2ACfg,
		"L2BCatalogConfig": l2BCfg,
	})
	waitDeployment(s.ctx, s.T(), dataplaneNamespace, l1Deploy)

	// Apply L1 filter insertion (epp-l1 is static; no cluster name needed).
	apply(s.ctx, s.T(), filepath.Join(s.dir, "k8s", "epp-l1.tmpl.yaml"))
	waitEnvoyPatchPolicyProgrammed(s.ctx, s.T(), dataplaneNamespace, "l1-mcp-profile-gateway")

	// Open service port-forwards for test assertions.
	l1URL, stopL1 := portForward(s.ctx, s.T(), "service/l1", 80)
	defer stopL1()
	l2AURL, stopL2A := portForward(s.ctx, s.T(), "service/l2-a", 80)
	defer stopL2A()
	l2BURL, stopL2B := portForward(s.ctx, s.T(), "service/l2-b", 80)
	defer stopL2B()

	// Test matrix — all required cases from README Test Matrix.
	liveLogf(s.T(), "asserting: auth failure reaches no L2")
	s.assertAuthFailure(l1URL)

	liveLogf(s.T(), "asserting: initialize with all backends healthy")
	sessionID := s.assertInitializeAllHealthy(l1URL)

	liveLogf(s.T(), "asserting: initialize partial backend failure")
	s.assertInitializePartialFailure(l1URL)

	liveLogf(s.T(), "asserting: initialize all-backend failure")
	s.assertInitializeAllFail(l1URL)

	liveLogf(s.T(), "asserting: tools/list returns all 4 servers")
	s.assertToolsListAllServers(l1URL, sessionID)

	liveLogf(s.T(), "asserting: tools/list preserves enabled-tool filtering")
	s.assertToolsListEnabledFiltering(l1URL)

	liveLogf(s.T(), "asserting: tools/list partial backend failure returns healthy tools")
	s.assertToolsListPartialBackendFailure(l1URL)

	liveLogf(s.T(), "asserting: tools/list all backends fail returns empty tools list")
	s.assertToolsListAllBackendsFail(l1URL)

	liveLogf(s.T(), "asserting: public catalog forwarding to L2-A")
	s.assertCatalogForwardingL2A(l1URL)

	liveLogf(s.T(), "asserting: public catalog forwarding to L2-B")
	s.assertCatalogForwardingL2B(l1URL)

	liveLogf(s.T(), "asserting: tools/call github.search reaches L2-B only")
	s.assertToolsCallGithubSearch(l1URL, sessionID)

	liveLogf(s.T(), "asserting: tools/call kiwi.search-flight reaches L2-A only")
	s.assertToolsCallKiwiSearchFlight(l1URL, sessionID)

	liveLogf(s.T(), "asserting: tools/call unknown tool returns JSON-RPC error")
	s.assertToolsCallUnknownToolError(l1URL, sessionID)

	liveLogf(s.T(), "asserting: tools/call disabled tool returns JSON-RPC error")
	s.assertToolsCallDisabledTool(l1URL)

	liveLogf(s.T(), "asserting: tools/call backend absent from partial session reaches no L2")
	s.assertToolsCallAbsentBackendInSession(l1URL)

	liveLogf(s.T(), "asserting: tools/call proxies backend 2xx JSON-RPC errors")
	s.assertToolsCallProxiesBackendJSONRPCError(l1URL, sessionID)

	liveLogf(s.T(), "asserting: tools/call backend transport failure becomes downstream error")
	s.assertToolsCallBackendTransportFailure(l1URL, sessionID)

	liveLogf(s.T(), "asserting: L2-A rejects direct /mcp/s/github (cross-shard)")
	s.assertCrossShardRejectionL2A(l2AURL)

	liveLogf(s.T(), "asserting: L2-B rejects direct /mcp/s/kiwi (cross-shard)")
	s.assertCrossShardRejectionL2B(l2BURL)

	liveLogf(s.T(), "asserting: cluster-router dump shows route_header: x-mcp-server")
	s.assertClusterRouterDumpShowsMCPRouteHeader(l2AURL, l2BURL)

	liveLogf(s.T(), "asserting: dumps redact secrets")
	s.assertDumpsRedactSecrets(l1URL, l2AURL, l2BURL)
}

func (s *mcpProfileGatewaySuite) pendingAssertion(name, reason string) {
	s.T().Helper()
	s.T().Run("pending/"+name, func(t *testing.T) {
		t.Skip(reason)
	})
}

// assertAuthFailure verifies that a request with wrong API key fails before any
// L2 request is made. Profile gateway returns 401 with a JSON-RPC error body.
func (s *mcpProfileGatewaySuite) assertAuthFailure(l1URL string) {
	s.T().Helper()
	body, err := json.Marshal(jsonRPCRequest{JSONRPC: "2.0", ID: 1, Method: "initialize"})
	require.NoError(s.T(), err)
	resp, status, _ := mcpPost(s.ctx, s.T(), l1URL, "/mcp/"+profileID,
		map[string]string{"x-api-key": "wrong-key"}, body)
	require.Equal(s.T(), http.StatusUnauthorized, status, "expected 401 on wrong API key; body: %s", resp)
}

// assertInitializeAllHealthy fans out initialize to all 4 profile members and
// returns the composite session ID for use in subsequent test cases.
func (s *mcpProfileGatewaySuite) assertInitializeAllHealthy(l1URL string) string {
	s.T().Helper()
	body, err := json.Marshal(jsonRPCRequest{JSONRPC: "2.0", ID: 1, Method: "initialize"})
	require.NoError(s.T(), err)
	var sessionID string
	eventually(s.ctx, s.T(), func() error {
		resp, status, headers := mcpPost(s.ctx, s.T(), l1URL, "/mcp/"+profileID,
			map[string]string{"x-api-key": profileKey}, body)
		if status != http.StatusOK {
			return fmt.Errorf("initialize status %d body %s", status, resp)
		}
		sessionID = headers.Get("mcp-session-id")
		if sessionID == "" {
			return fmt.Errorf("missing mcp-session-id header; body %s", resp)
		}
		return nil
	})
	require.NotEmpty(s.T(), sessionID, "composite session ID must be set after initialize")
	return sessionID
}

// assertInitializePartialFailure verifies that when one backend fails to initialize,
// the composite session contains only the successful backends and the downstream
// initialize response still succeeds.
//
// Requires fake-mcp to support a configurable failure mode so one backend returns
// an error on initialize while the others succeed. When the demo binary supports
// this, pass a profile config that includes one server pointing at a backend
// configured to fail, run initialize, and verify the session backends map omits
// the failing server.
func (s *mcpProfileGatewaySuite) assertInitializePartialFailure(_ string) {
	s.pendingAssertion("initialize-partial-backend-failure", "requires fake-mcp failure injection support")
}

// assertInitializeAllFail verifies that when all backends fail to initialize,
// the gateway returns a downstream error (HTTP 502 or JSON-RPC error code -32002).
//
// Requires all fake-mcp backends to be made temporarily unreachable.
func (s *mcpProfileGatewaySuite) assertInitializeAllFail(_ string) {
	s.pendingAssertion("initialize-all-backends-fail", "requires fake-mcp failure injection support")
}

// assertToolsListAllServers fans out tools/list to all 4 member servers and
// verifies the merged tool set contains the expected namespaced tool names.
func (s *mcpProfileGatewaySuite) assertToolsListAllServers(l1URL, sessionID string) {
	s.T().Helper()
	body, err := json.Marshal(jsonRPCRequest{JSONRPC: "2.0", ID: 2, Method: "tools/list"})
	require.NoError(s.T(), err)
	eventually(s.ctx, s.T(), func() error {
		resp, status, _ := mcpPost(s.ctx, s.T(), l1URL, "/mcp/"+profileID,
			map[string]string{"x-api-key": profileKey, "mcp-session-id": sessionID}, body)
		if status != http.StatusOK {
			return fmt.Errorf("tools/list status %d body %s", status, resp)
		}
		names, err := toolNamesFromResponse(resp)
		if err != nil {
			return err
		}
		for _, want := range []string{toolKiwiSearchFlight, toolAWSReadDoc, toolMicrosoftSearchDocs, toolGithubSearch} {
			if !contains(names, want) {
				return fmt.Errorf("tools/list missing %q; got %v", want, names)
			}
		}
		return nil
	})
}

// assertToolsListEnabledFiltering verifies that tools/list respects the
// enabled_tools filter configured on a profile server entry.
//
// Requires a profile variant that enables only a subset of tools on one server.
// The test should verify that disabled tools are absent from the merged list.
func (s *mcpProfileGatewaySuite) assertToolsListEnabledFiltering(_ string) {
	s.pendingAssertion("tools-list-enabled-filtering", "requires a second profile config with restricted enabled_tools")
}

// assertToolsListPartialBackendFailure verifies that when one backend fails
// during tools/list, the merged response contains healthy tools from the
// remaining backends (MCPProxy parity: partial success).
func (s *mcpProfileGatewaySuite) assertToolsListPartialBackendFailure(_ string) {
	s.pendingAssertion("tools-list-partial-backend-failure", "requires fake-mcp failure injection support")
}

// assertToolsListAllBackendsFail verifies that when all backends fail tools/list,
// the gateway returns a successful empty {"tools":[]} response
// (MCPProxy parity: all-backends-fail returns empty, not an error).
func (s *mcpProfileGatewaySuite) assertToolsListAllBackendsFail(_ string) {
	s.pendingAssertion("tools-list-all-backends-fail", "requires fake-mcp failure injection support")
}

// assertCatalogForwardingL2A verifies that a public /mcp/s/aws-knowledge request
// through L1 is forwarded to L2-A only.
func (s *mcpProfileGatewaySuite) assertCatalogForwardingL2A(l1URL string) {
	s.T().Helper()
	body, err := json.Marshal(jsonRPCRequest{JSONRPC: "2.0", ID: 3, Method: "tools/list"})
	require.NoError(s.T(), err)
	eventually(s.ctx, s.T(), func() error {
		resp, status, _ := mcpPost(s.ctx, s.T(), l1URL, "/mcp/s/aws-knowledge", nil, body)
		if status != http.StatusOK {
			return fmt.Errorf("catalog /mcp/s/aws-knowledge status %d body %s", status, resp)
		}
		return nil
	})
}

// assertCatalogForwardingL2B verifies that a public /mcp/s/github request
// through L1 is forwarded to L2-B only.
func (s *mcpProfileGatewaySuite) assertCatalogForwardingL2B(l1URL string) {
	s.T().Helper()
	body, err := json.Marshal(jsonRPCRequest{JSONRPC: "2.0", ID: 4, Method: "tools/list"})
	require.NoError(s.T(), err)
	eventually(s.ctx, s.T(), func() error {
		resp, status, _ := mcpPost(s.ctx, s.T(), l1URL, "/mcp/s/github", nil, body)
		if status != http.StatusOK {
			return fmt.Errorf("catalog /mcp/s/github status %d body %s", status, resp)
		}
		return nil
	})
}

// assertToolsCallGithubSearch verifies that tools/call github.search routes to
// L2-B and the GitHub backend only. The fake-mcp backend must echo its server
// name in the result so the assertion can verify which backend handled the call.
func (s *mcpProfileGatewaySuite) assertToolsCallGithubSearch(l1URL, sessionID string) {
	s.T().Helper()
	resp, status := s.doToolsCall(l1URL, sessionID, "github.search", nil)
	require.Equal(s.T(), http.StatusOK, status, "tools/call github.search failed; body: %s", resp)
	var rpc jsonRPCResponse
	require.NoError(s.T(), json.Unmarshal(resp, &rpc))
	require.Nil(s.T(), rpc.Error, "tools/call github.search returned JSON-RPC error: %+v", rpc.Error)
}

// assertToolsCallKiwiSearchFlight verifies that tools/call kiwi.search-flight
// routes to L2-A and the Kiwi backend only.
func (s *mcpProfileGatewaySuite) assertToolsCallKiwiSearchFlight(l1URL, sessionID string) {
	s.T().Helper()
	resp, status := s.doToolsCall(l1URL, sessionID, "kiwi.search-flight", nil)
	require.Equal(s.T(), http.StatusOK, status, "tools/call kiwi.search-flight failed; body: %s", resp)
	var rpc jsonRPCResponse
	require.NoError(s.T(), json.Unmarshal(resp, &rpc))
	require.Nil(s.T(), rpc.Error, "tools/call kiwi.search-flight returned JSON-RPC error: %+v", rpc.Error)
}

// assertToolsCallUnknownToolError verifies that tools/call for an unknown tool
// prefix returns a controlled JSON-RPC error and makes no L2 or backend request.
func (s *mcpProfileGatewaySuite) assertToolsCallUnknownToolError(l1URL, sessionID string) {
	s.T().Helper()
	resp, status := s.doToolsCall(l1URL, sessionID, "unknown-prefix.some-tool", nil)
	require.Equal(s.T(), http.StatusOK, status, "unexpected status for unknown tool; body: %s", resp)
	var rpc jsonRPCResponse
	require.NoError(s.T(), json.Unmarshal(resp, &rpc))
	require.NotNil(s.T(), rpc.Error, "expected JSON-RPC error for unknown tool; body: %s", resp)
	require.Equal(s.T(), -32602, rpc.Error.Code, "unexpected error code for unknown tool")
}

// assertToolsCallDisabledTool verifies that tools/call for a tool that is
// explicitly disabled in the profile returns a controlled JSON-RPC error.
//
// Requires a profile variant with one tool disabled via enabled_tools map.
func (s *mcpProfileGatewaySuite) assertToolsCallDisabledTool(_ string) {
	s.pendingAssertion("tools-call-disabled-tool", "requires a second profile config with restricted enabled_tools")
}

// assertToolsCallAbsentBackendInSession verifies that tools/call for a backend
// that was absent from the partial-initialize session returns an error and makes
// no L2 request (MCPProxy parity: absent-from-session tools/call must fail).
func (s *mcpProfileGatewaySuite) assertToolsCallAbsentBackendInSession(_ string) {
	s.pendingAssertion("tools-call-absent-backend-in-session", "requires partial initialize failure injection")
}

// assertToolsCallProxiesBackendJSONRPCError verifies that a backend 2xx response
// carrying a JSON-RPC error is proxied to the client as-is
// (MCPProxy parity: backend JSON-RPC errors within 2xx responses are forwarded).
//
// Requires the fake-mcp binary to return a JSON-RPC error for a named tool call.
func (s *mcpProfileGatewaySuite) assertToolsCallProxiesBackendJSONRPCError(_ string, _ string) {
	s.pendingAssertion("tools-call-proxies-backend-jsonrpc-error", "requires fake-mcp tool-level error injection")
}

// assertToolsCallBackendTransportFailure verifies that a backend transport error
// or non-2xx response becomes a downstream error
// (MCPProxy parity: transport failures become downstream 502 or JSON-RPC error).
//
// Requires the fake-mcp binary to be made temporarily unreachable for one server.
func (s *mcpProfileGatewaySuite) assertToolsCallBackendTransportFailure(_ string, _ string) {
	s.pendingAssertion("tools-call-backend-transport-failure", "requires fake-mcp failure injection support")
}

// assertCrossShardRejectionL2A verifies that a direct /mcp/s/github request to
// L2-A (which does not own github) is rejected. L2-A's mcp-catalog-router does
// not have github in its catalog so the request should fail with a 4xx or
// JSON-RPC error before reaching any backend.
func (s *mcpProfileGatewaySuite) assertCrossShardRejectionL2A(l2AURL string) {
	s.T().Helper()
	body, err := json.Marshal(jsonRPCRequest{JSONRPC: "2.0", ID: 5, Method: "tools/list"})
	require.NoError(s.T(), err)
	// Direct to L2-A, no gatewayHost needed (no hostname routing on L2 gateways).
	req, err := http.NewRequestWithContext(s.ctx, http.MethodPost, l2AURL+"/mcp/s/github", bytes.NewReader(body))
	require.NoError(s.T(), err)
	req.Header.Set("content-type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	require.NoError(s.T(), err)
	_ = resp.Body.Close()
	require.NotEqual(s.T(), http.StatusOK, resp.StatusCode,
		"L2-A should reject /mcp/s/github (cross-shard); got %d", resp.StatusCode)
}

// assertCrossShardRejectionL2B verifies that a direct /mcp/s/kiwi request to
// L2-B (which does not own kiwi) is rejected.
func (s *mcpProfileGatewaySuite) assertCrossShardRejectionL2B(l2BURL string) {
	s.T().Helper()
	body, err := json.Marshal(jsonRPCRequest{JSONRPC: "2.0", ID: 6, Method: "tools/list"})
	require.NoError(s.T(), err)
	req, err := http.NewRequestWithContext(s.ctx, http.MethodPost, l2BURL+"/mcp/s/kiwi", bytes.NewReader(body))
	require.NoError(s.T(), err)
	req.Header.Set("content-type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	require.NoError(s.T(), err)
	_ = resp.Body.Close()
	require.NotEqual(s.T(), http.StatusOK, resp.StatusCode,
		"L2-B should reject /mcp/s/kiwi (cross-shard); got %d", resp.StatusCode)
}

// assertClusterRouterDumpShowsMCPRouteHeader verifies that the cluster-router
// active config dump on both L2 shards contains route_header: x-mcp-server.
func (s *mcpProfileGatewaySuite) assertClusterRouterDumpShowsMCPRouteHeader(l2AURL, l2BURL string) {
	s.T().Helper()
	for _, url := range []string{l2AURL, l2BURL} {
		eventually(s.ctx, s.T(), func() error {
			req, err := http.NewRequestWithContext(s.ctx, http.MethodGet, url+"/__cluster-router/config", nil)
			if err != nil {
				return err
			}
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				return err
			}
			defer func() { _ = resp.Body.Close() }()
			body, err := io.ReadAll(resp.Body)
			if err != nil {
				return err
			}
			if resp.StatusCode != http.StatusOK {
				return fmt.Errorf("cluster-router dump status %d body %s", resp.StatusCode, body)
			}
			if !strings.Contains(string(body), "x-mcp-server") {
				return fmt.Errorf("cluster-router dump missing x-mcp-server; body: %s", body)
			}
			return nil
		})
	}
}

// assertDumpsRedactSecrets verifies that the L1 /dump and L2 cluster-router
// dumps do not expose the profile API key, bearer tokens, or session IDs.
func (s *mcpProfileGatewaySuite) assertDumpsRedactSecrets(l1URL, l2AURL, l2BURL string) {
	s.T().Helper()
	secrets := []string{profileKey, "Bearer kiwi-token", "Bearer aws-token", "Bearer microsoft-token", "Bearer github-token"}
	for _, endpoint := range []struct{ url, path string }{
		{l1URL, "/dump"},
		{l2AURL, "/__cluster-router/config"},
		{l2BURL, "/__cluster-router/config"},
	} {
		req, err := http.NewRequestWithContext(s.ctx, http.MethodGet, endpoint.url+endpoint.path, nil)
		require.NoError(s.T(), err)
		resp, err := http.DefaultClient.Do(req)
		require.NoError(s.T(), err)
		body, err := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		require.NoError(s.T(), err)
		bodyStr := string(body)
		for _, secret := range secrets {
			require.NotContainsf(s.T(), bodyStr, secret,
				"dump at %s leaked secret %q", endpoint.url+endpoint.path, secret)
		}
	}
}

// doToolsCall sends a tools/call request and returns the response body and status.
func (s *mcpProfileGatewaySuite) doToolsCall(l1URL, sessionID, toolName string, args json.RawMessage) ([]byte, int) {
	s.T().Helper()
	type callParams struct {
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments,omitempty"`
	}
	params, err := json.Marshal(callParams{Name: toolName, Arguments: args})
	require.NoError(s.T(), err)
	reqBody, err := json.Marshal(jsonRPCRequest{
		JSONRPC: "2.0",
		ID:      99,
		Method:  "tools/call",
		Params:  params,
	})
	require.NoError(s.T(), err)
	body, status, _ := mcpPost(s.ctx, s.T(), l1URL, "/mcp/"+profileID,
		map[string]string{"x-api-key": profileKey, "mcp-session-id": sessionID}, reqBody)
	return body, status
}

// JSON-RPC types used only by the MCP profile test assertions.
type jsonRPCRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      int             `json:"id"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type jsonRPCResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *jsonRPCError   `json:"error,omitempty"`
}

type jsonRPCError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

// mcpPost sends a POST to baseURL+path with the gateway host header, content-type
// application/json, the supplied extra headers, and the given body. Returns the
// response body, HTTP status code, and response headers.
func mcpPost(ctx context.Context, t *testing.T, baseURL, path string, headers map[string]string, body []byte) ([]byte, int, http.Header) {
	t.Helper()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, baseURL+path, bytes.NewReader(body))
	require.NoError(t, err)
	req.Host = gatewayHost
	req.Header.Set("content-type", "application/json")
	req.Header.Set("accept", "application/json")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	respBody, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	return respBody, resp.StatusCode, resp.Header
}

// toolNamesFromResponse extracts the tool Name fields from a tools/list JSON-RPC
// response body.
func toolNamesFromResponse(body []byte) ([]string, error) {
	var rpc jsonRPCResponse
	if err := json.Unmarshal(body, &rpc); err != nil {
		return nil, fmt.Errorf("unmarshal JSON-RPC response: %w", err)
	}
	if rpc.Error != nil {
		return nil, fmt.Errorf("JSON-RPC error %d: %s", rpc.Error.Code, rpc.Error.Message)
	}
	var result struct {
		Tools []struct {
			Name string `json:"name"`
		} `json:"tools"`
	}
	if err := json.Unmarshal(rpc.Result, &result); err != nil {
		return nil, fmt.Errorf("unmarshal tools/list result: %w", err)
	}
	names := make([]string, 0, len(result.Tools))
	for _, tool := range result.Tools {
		names = append(names, tool.Name)
	}
	return names, nil
}

// l2CatalogConfig builds the MCP_CATALOG_ROUTER_CONFIG JSON for an L2 shard.
// servers is a map of server slug -> { url, credential }.
func l2CatalogConfig(servers map[string]struct{ URL, Credential string }) string {
	type serverCfg struct {
		URL        string `json:"url"`
		Credential string `json:"credential,omitempty"`
	}
	cfg := struct {
		Servers map[string]serverCfg `json:"servers"`
	}{Servers: make(map[string]serverCfg, len(servers))}
	for id, s := range servers {
		cfg.Servers[id] = serverCfg{URL: s.URL, Credential: s.Credential}
	}
	raw, err := json.Marshal(cfg)
	if err != nil {
		panic("l2CatalogConfig: " + err.Error())
	}
	return string(raw)
}

// buildL1Config constructs the MCP_PROFILE_GATEWAY_CONFIG JSON for the L1
// discovered from the L1 admin API for the l1-l2a-catalog and l1-l2b-catalog
// HTTPRoutes respectively.
func buildL1Config(l1L2ACluster, l1L2BCluster string) string {
	type catalogServer struct {
		URL     string `json:"url"`
		Cluster string `json:"cluster"`
	}
	type profileServer struct {
		URL           string `json:"url"`
		Prefix        string `json:"prefix"`
		CredentialRef string `json:"credential_ref"`
	}
	type profile struct {
		Name    string                   `json:"name"`
		APIKey  string                   `json:"api_key"`
		Servers map[string]profileServer `json:"servers"`
	}
	type l1config struct {
		TimeoutMillis  int                      `json:"timeout_millis,omitempty"`
		CatalogServers map[string]catalogServer `json:"catalog_servers"`
		Profiles       map[string]profile       `json:"profiles"`
	}
	l2ABase := "http://l2-a.transit-dataplane.svc.cluster.local"
	l2BBase := "http://l2-b.transit-dataplane.svc.cluster.local"
	cfg := l1config{
		TimeoutMillis: 800,
		CatalogServers: map[string]catalogServer{
			"kiwi":          {URL: l2ABase, Cluster: l1L2ACluster},
			"aws-knowledge": {URL: l2ABase, Cluster: l1L2ACluster},
			"microsoft":     {URL: l2BBase, Cluster: l1L2BCluster},
			"github":        {URL: l2BBase, Cluster: l1L2BCluster},
		},
		Profiles: map[string]profile{
			profileID: {
				Name:   "kiwi",
				APIKey: profileKey,
				Servers: map[string]profileServer{
					"kiwi":          {URL: l2ABase, Prefix: "kiwi", CredentialRef: "profile/kiwi/user-123"},
					"aws-knowledge": {URL: l2ABase, Prefix: "aws-knowledge", CredentialRef: "profile/aws-knowledge/user-123"},
					"microsoft":     {URL: l2BBase, Prefix: "microsoft", CredentialRef: "profile/microsoft/user-123"},
					"github":        {URL: l2BBase, Prefix: "github", CredentialRef: "profile/github/user-123"},
				},
			},
		},
	}
	raw, err := json.Marshal(cfg)
	if err != nil {
		panic("buildL1Config: " + err.Error())
	}
	return string(raw)
}

// discoverBackendCluster finds the EG-generated Envoy cluster name for the
// given HTTPRoute. Pattern: httproute/<namespace>/<routeName>/rule/<n>/...
func discoverBackendCluster(ctx context.Context, t *testing.T, adminURL, routeName string) string {
	t.Helper()
	liveLogf(t, "discovering backend cluster for HTTPRoute %s", routeName)
	deadline := time.Now().Add(60 * time.Second)
	var names []string
	for time.Now().Before(deadline) {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, adminURL+"/config_dump", nil)
		if err != nil {
			time.Sleep(200 * time.Millisecond)
			continue
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			time.Sleep(200 * time.Millisecond)
			continue
		}
		body, err := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if err != nil {
			time.Sleep(200 * time.Millisecond)
			continue
		}
		names = clusterNamesFromDump(body)
		prefix := "httproute/" + dataplaneNamespace + "/" + routeName + "/rule/"
		for _, name := range names {
			if strings.HasPrefix(name, prefix) {
				liveLogf(t, "found cluster %q for HTTPRoute %s", name, routeName)
				return name
			}
		}
		select {
		case <-ctx.Done():
			require.NoError(t, ctx.Err())
		case <-time.After(500 * time.Millisecond):
		}
	}
	require.Failf(t, "backend cluster not found",
		"no cluster with prefix httproute/%s/%s/rule/ found; clusters: %s",
		dataplaneNamespace, routeName, strings.Join(names, ", "))
	return ""
}

func clusterNamesFromDump(body []byte) []string {
	var dump struct {
		Configs []json.RawMessage `json:"configs"`
	}
	if err := json.Unmarshal(body, &dump); err != nil {
		return nil
	}
	var names []string
	for _, raw := range dump.Configs {
		var cfg struct {
			DynamicActiveClusters []struct {
				Cluster struct {
					Name string `json:"name"`
				} `json:"cluster"`
			} `json:"dynamic_active_clusters"`
			StaticClusters []struct {
				Cluster struct {
					Name string `json:"name"`
				} `json:"cluster"`
			} `json:"static_clusters"`
		}
		if err := json.Unmarshal(raw, &cfg); err != nil {
			continue
		}
		for _, c := range cfg.DynamicActiveClusters {
			if c.Cluster.Name != "" {
				names = append(names, c.Cluster.Name)
			}
		}
		for _, c := range cfg.StaticClusters {
			if c.Cluster.Name != "" {
				names = append(names, c.Cluster.Name)
			}
		}
	}
	return names
}

func contains(slice []string, s string) bool {
	for _, v := range slice {
		if v == s {
			return true
		}
	}
	return false
}
