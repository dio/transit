// Package e2e validates the three structural gates for the tiered-ws-proxy
// Envoy Gateway integration:
//
//   - Gate 1: WS upgrade routes end-to-end through L1 → L2 → mock (101 at
//     both hops; 426 would mean upgrade_configs is missing).
//   - Gate 2: response.create → response.completed round-trip proves the full
//     data path: harness → L1 tunnel → L2 embedded server → L2 egress
//     listener → mock → back. Token counts asserted against mock's fixed values.
//   - Gate 3: L2 session record written after close (SessionTap fired, model
//     and token fields extracted from response.completed).
//
// Run:
//
//	make -C integrations/tiered-ws-proxy-eg e2e
package e2e

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/stretchr/testify/require"
	"github.com/stretchr/testify/suite"

	"github.com/dio/transit/integrations/internal/egtest"
)

const (
	l1GatewayHost = "l1.ws-proxy.example.com"

	// XDSNameSchemeV2 cluster names derived from HTTPRoute names.
	l1ClusterName    = "httproute/default/l1/rule/0"
	l2InboundCluster = "httproute/default/l2-inbound/rule/0"
	l2EgressCluster  = "httproute/default/l2-egress/rule/0"

	// Fixed usage values returned by mock/cmd/main.go.
	mockInputTokens  = uint32(100)
	mockOutputTokens = uint32(42)
)

func TestTieredWsProxyEnvoyGateway(t *testing.T) {
	if os.Getenv("RUN_TIERED_WS_PROXY_EG_E2E") != "1" {
		t.Skip("set RUN_TIERED_WS_PROXY_EG_E2E=1 to run the k3d Envoy Gateway integration")
	}
	suite.Run(t, &tieredWsProxySuite{
		envoyGatewaySuite: envoyGatewaySuite{
			Suite: egtest.Suite{
				Timeout: 14 * time.Minute,
			},
		},
	})
}

type tieredWsProxySuite struct {
	envoyGatewaySuite
	l1Image   string
	l2Image   string
	mockImage string
	l2Deploy  string
}

func (s *tieredWsProxySuite) SetupSuite() {
	s.l1Image = egtest.RequireEnv(s.T(), "L1_IMAGE")
	s.l2Image = egtest.RequireEnv(s.T(), "L2_IMAGE")
	s.mockImage = egtest.RequireEnv(s.T(), "MOCK_IMAGE")
	s.envoyGatewaySuite.SetupSuite()
	s.ImportImages(s.l1Image, s.l2Image, s.mockImage)
}

func (s *tieredWsProxySuite) TestTieredWsProxyGates() {
	liveLogf(s.T(), "verifying Envoy Gateway install")
	s.verifyEnvoyGatewayInstall()

	// ── Phase 2: apply resources ─────────────────────────────────────────────

	liveLogf(s.T(), "applying L1 EnvoyProxy and Gateway resources")
	renderApply(s.Ctx, s.T(), filepath.Join(s.Dir, "k8s", "l1-envoyproxy.tmpl.yaml"), map[string]string{
		"L1Image": s.l1Image,
	})
	apply(s.Ctx, s.T(), filepath.Join(s.Dir, "k8s", "l1-gateway.yaml"))
	apply(s.Ctx, s.T(), filepath.Join(s.Dir, "k8s", "l1-httproute.yaml"))

	liveLogf(s.T(), "applying L2 EnvoyProxy and Gateway resources")
	renderApply(s.Ctx, s.T(), filepath.Join(s.Dir, "k8s", "l2-envoyproxy.tmpl.yaml"), map[string]string{
		"L2Image": s.l2Image,
	})
	apply(s.Ctx, s.T(), filepath.Join(s.Dir, "k8s", "l2-gateway.yaml"))
	renderApply(s.Ctx, s.T(), filepath.Join(s.Dir, "k8s", "l2-httproute.tmpl.yaml"), map[string]string{
		"MockImage": s.mockImage,
	})

	liveLogf(s.T(), "waiting for Gateways to be Accepted")
	run(s.Ctx, s.T(), "", "kubectl", "wait", "gateway/l1",
		"--for=condition=Accepted", "--timeout=120s")
	run(s.Ctx, s.T(), "", "kubectl", "wait", "gateway/l2",
		"--for=condition=Accepted", "--timeout=120s")

	// EPP replace on l2-inbound cluster requires a ready endpoint; wait for
	// the pause placeholder before applying the EPP.
	liveLogf(s.T(), "waiting for mock-upstream and L2 inbound placeholder deployments")
	waitDeployment(s.Ctx, s.T(), "default", "mock-upstream")
	waitDeployment(s.Ctx, s.T(), "default", "l2-inbound-backend")

	liveLogf(s.T(), "waiting for generated L1 and L2 Envoy deployments")
	l1Deploy := s.generatedDeployment("l1")
	waitDeployment(s.Ctx, s.T(), "envoy-gateway-system", l1Deploy)
	s.l2Deploy = s.generatedDeployment("l2")
	waitDeployment(s.Ctx, s.T(), "envoy-gateway-system", s.l2Deploy)

	// ── Phase 3: EPP patches ─────────────────────────────────────────────────

	liveLogf(s.T(), "opening L2 admin port-forward")
	l2AdminURL, stopL2Admin := portForward(s.Ctx, s.T(), "envoy-gateway-system", "deploy/"+s.l2Deploy, 19000)
	defer stopL2Admin()

	liveLogf(s.T(), "applying L1 EPP")
	renderApply(s.Ctx, s.T(), filepath.Join(s.Dir, "k8s", "l1-epp.tmpl.yaml"), map[string]string{
		"L1ClusterName": l1ClusterName,
	})
	waitEPP(s.Ctx, s.T(), "l1")

	liveLogf(s.T(), "applying L2 EPP")
	renderApply(s.Ctx, s.T(), filepath.Join(s.Dir, "k8s", "l2-epp.tmpl.yaml"), map[string]string{
		"L2InboundCluster": l2InboundCluster,
		"L2EgressCluster":  l2EgressCluster,
	})
	waitEPP(s.Ctx, s.T(), "l2")
	s.waitL2ConfigApplied(l2AdminURL)

	// ── Phase 4: gate assertions ─────────────────────────────────────────────

	liveLogf(s.T(), "opening L1 Gateway port-forward")
	l1Service := s.generatedService("l1")
	gatewayURL, stopGateway := portForward(s.Ctx, s.T(), "envoy-gateway-system",
		"service/"+l1Service, 80)
	defer stopGateway()

	wsURL := "ws://" + strings.TrimPrefix(gatewayURL, "http://") + "/v1/responses"

	// Gate 1 ─────────────────────────────────────────────────────────────────
	// 426 = upgrade_configs missing at L1 or L2.
	// 502 = L1 cannot reach L2.
	// Successful dial proves upgrade propagated end-to-end.
	liveLogf(s.T(), "Gate 1: WS dial through L1 → L2 → mock")
	conn, _, err := websocket.Dial(s.Ctx, wsURL, &websocket.DialOptions{
		Host: l1GatewayHost,
	})
	require.NoError(s.T(), err, "Gate 1 FAIL: WS dial failed (426=upgrade_configs missing, 502=L2 unreachable)")
	defer conn.CloseNow()
	liveLogf(s.T(), "Gate 1 PASS: 101 end-to-end through L1 → L2 → mock")

	// Gate 2 ─────────────────────────────────────────────────────────────────
	// response.create propagates harness→L1→L2 embedded server→L2 egress→mock.
	// mock replies with response.completed (fixed usage). The reply travels
	// mock→L2 egress→L2 embedded server (SessionTap fires)→L1→harness.
	liveLogf(s.T(), "Gate 2: response.create → response.completed through full chain")
	err = conn.Write(s.Ctx, websocket.MessageText,
		[]byte(`{"type":"response.create","model":"gpt-4.1","input":[]}`))
	require.NoError(s.T(), err)

	_, raw, err := conn.Read(s.Ctx)
	require.NoError(s.T(), err)

	var completed struct {
		Type     string `json:"type"`
		Response struct {
			Model string `json:"model"`
			Usage struct {
				InputTokens  uint32 `json:"input_tokens"`
				OutputTokens uint32 `json:"output_tokens"`
			} `json:"usage"`
		} `json:"response"`
	}
	require.NoError(s.T(), json.Unmarshal(raw, &completed), "Gate 2 FAIL: response.completed not valid JSON")
	require.Equal(s.T(), "response.completed", completed.Type,
		"Gate 2 FAIL: expected response.completed, got %s", completed.Type)
	require.Equal(s.T(), "gpt-4.1", completed.Response.Model,
		"Gate 2 FAIL: model not propagated through chain")
	require.Equal(s.T(), mockInputTokens, completed.Response.Usage.InputTokens,
		"Gate 2 FAIL: input_tokens mismatch")
	require.Equal(s.T(), mockOutputTokens, completed.Response.Usage.OutputTokens,
		"Gate 2 FAIL: output_tokens mismatch")
	liveLogf(s.T(), "Gate 2 PASS: response.completed arrived with correct model and usage")

	// Gate 3 ─────────────────────────────────────────────────────────────────
	// Close the connection cleanly so recordActorSession fires at L2, then
	// poll the session log file inside the L2 pod.
	conn.Close(websocket.StatusNormalClosure, "done")
	liveLogf(s.T(), "Gate 3: session record written at L2 after close")
	s.waitSessionRecord("gpt-4.1", mockInputTokens, mockOutputTokens)
	liveLogf(s.T(), "Gate 3 PASS: session record found in L2 pod")

	liveLogf(s.T(), "all three gates PASS")
}

// waitL2ConfigApplied polls the L2 Envoy /config_dump until all EPP patches
// are visible: DynamicModuleFilter, STATIC loopback :10001, egress listener
// :10002.
func (s *tieredWsProxySuite) waitL2ConfigApplied(adminURL string) {
	s.T().Helper()
	liveLogf(s.T(), "waiting for L2 active config to reflect EPP patches")
	deadline := time.Now().Add(60 * time.Second)
	var last string
	for time.Now().Before(deadline) {
		last = httpGetString(s.Ctx, s.T(), adminURL+"/config_dump")
		filterOK := strings.Contains(last, "envoy.extensions.filters.http.dynamic_modules.v3.DynamicModuleFilter") &&
			(strings.Contains(last, `"filterName": "ws-proxy"`) || strings.Contains(last, `"filter_name": "ws-proxy"`))
		loopbackOK := strings.Contains(last, "127.0.0.1") && strings.Contains(last, "10001")
		egressOK := strings.Contains(last, "10002")
		if filterOK && loopbackOK && egressOK {
			liveLogf(s.T(), "L2 active config includes all EPP patches")
			return
		}
		select {
		case <-s.Ctx.Done():
			require.NoError(s.T(), s.Ctx.Err())
		case <-time.After(500 * time.Millisecond):
		}
	}
	require.Contains(s.T(), last,
		"envoy.extensions.filters.http.dynamic_modules.v3.DynamicModuleFilter",
		"L2 listener does not include ws-proxy DynamicModuleFilter")
	require.Contains(s.T(), last, "10001",
		"L2 cluster does not point at embedded server :10001")
	require.Contains(s.T(), last, "10002",
		"L2 egress listener :10002 not present in config_dump")
}

// sessionRecord is the JSON shape written by recordActorSession when
// WSPROXY_SESSION_LOG is set in the L2 Envoy pod.
type sessionRecord struct {
	Path         string `json:"path"`
	Model        string `json:"model"`
	InputTokens  uint32 `json:"input_tokens"`
	OutputTokens uint32 `json:"output_tokens"`
	DurationMS   int64  `json:"duration_ms"`
	Result       string `json:"result"`
}

// waitSessionRecord polls /tmp/ws-sessions.jsonl inside the L2 Envoy pod
// until a record matching model and token counts appears or 10s elapses.
func (s *tieredWsProxySuite) waitSessionRecord(model string, wantInput, wantOutput uint32) {
	s.T().Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		cmd := exec.CommandContext(s.Ctx, "kubectl",
			"--context", egtest.KubeContext(s.T()),
			"-n", "envoy-gateway-system",
			"exec", "deploy/"+s.l2Deploy, "--",
			"cat", "/tmp/ws-sessions.jsonl")
		out, err := cmd.Output()
		if err == nil {
			for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
				var rec sessionRecord
				if json.Unmarshal([]byte(line), &rec) == nil &&
					rec.Model == model &&
					rec.InputTokens == wantInput &&
					rec.OutputTokens == wantOutput {
					return
				}
			}
		}
		select {
		case <-s.Ctx.Done():
			require.NoError(s.T(), s.Ctx.Err())
		case <-time.After(300 * time.Millisecond):
		}
	}
	require.Failf(s.T(), "Gate 3 FAIL",
		"session record with model=%s input=%d output=%d not found in L2 pod /tmp/ws-sessions.jsonl within 10s",
		model, wantInput, wantOutput)
}

// generatedDeployment waits for and returns the EG-generated Envoy Deployment
// name owned by the given Gateway (in default namespace).
func (s *tieredWsProxySuite) generatedDeployment(gateway string) string {
	s.T().Helper()
	return egtest.GeneratedResourceName(s.Ctx, s.T(), "envoy-gateway-system", gateway, "deploy")
}

// generatedService waits for and returns the EG-generated Envoy Service name
// owned by the given Gateway (in default namespace).
func (s *tieredWsProxySuite) generatedService(gateway string) string {
	s.T().Helper()
	return egtest.GeneratedResourceName(s.Ctx, s.T(), "envoy-gateway-system", gateway, "svc")
}

func httpGetString(ctx context.Context, t *testing.T, url string) string {
	t.Helper()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	require.NoError(t, err)
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	return string(b)
}
