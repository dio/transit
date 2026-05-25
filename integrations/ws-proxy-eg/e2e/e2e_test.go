// Package e2e validates the three P1 structural gates for the ws-proxy Envoy
// Gateway integration:
//
//   - Gate 1: upgrade_configs injected via EPP (Envoy accepts WS upgrades
//     without returning 426).
//   - Gate 2: WS frames pass through the EPP-replaced STATIC loopback cluster
//     intact and in order (echo test, 10 frames of varying sizes).
//   - Gate 3: plain HTTP to /v1/responses via EG returns 400 (non-WS rejected
//     by the embedded server before websocket.Accept).
//
// No real upstream dial. No auth. No metering. The embedded server is a minimal
// echo proxy (integrations/ws-proxy-eg/echo) built into the custom Envoy image.
//
// Run:
//
//	make -C integrations/ws-proxy-eg e2e
package e2e

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/stretchr/testify/require"
	"github.com/stretchr/testify/suite"

	"github.com/dio/transit/integrations/internal/egtest"
)

const gatewayHost = "ws-proxy.example.com"
const backendClusterName = "httproute/default/ws-proxy/rule/0"

func TestWsProxyEnvoyGateway(t *testing.T) {
	if os.Getenv("RUN_WS_PROXY_EG_E2E") != "1" {
		t.Skip("set RUN_WS_PROXY_EG_E2E=1 to run the k3d Envoy Gateway integration")
	}
	suite.Run(t, &wsProxySuite{
		envoyGatewaySuite: envoyGatewaySuite{
			Suite: egtest.Suite{
				Cluster: "transit-ws-proxy-eg",
				Timeout: 14 * time.Minute,
			},
		},
	})
}

type wsProxySuite struct {
	envoyGatewaySuite
	envoyImage string
	pauseImage string
}

func (s *wsProxySuite) SetupSuite() {
	s.envoyImage = egtest.RequireEnv(s.T(), "IMAGE")
	s.pauseImage = egtest.RequireEnv(s.T(), "PAUSE_IMAGE")
	s.envoyGatewaySuite.SetupSuite()
	s.ImportImages(s.envoyImage, s.pauseImage)
}

func (s *wsProxySuite) TestWsProxyGates() {
	liveLogf(s.T(), "verifying Envoy Gateway install")
	s.verifyEnvoyGatewayInstall()

	liveLogf(s.T(), "applying EnvoyProxy and Gateway resources")
	renderApply(s.Ctx, s.T(), filepath.Join(s.Dir, "k8s", "envoyproxy.tmpl.yaml"), map[string]string{
		"EnvoyImage": s.envoyImage,
	})
	apply(s.Ctx, s.T(), filepath.Join(s.Dir, "k8s", "gateway.yaml"))
	renderApply(s.Ctx, s.T(), filepath.Join(s.Dir, "k8s", "httproute.tmpl.yaml"), map[string]string{
		"PauseImage": s.pauseImage,
	})

	liveLogf(s.T(), "waiting for Gateway to be Accepted")
	run(s.Ctx, s.T(), "", "kubectl", "wait", "gateway/ws-proxy",
		"--for=condition=Accepted", "--timeout=120s")

	liveLogf(s.T(), "waiting for backend placeholder deployment")
	waitDeployment(s.Ctx, s.T(), "default", "ws-proxy-backend")

	liveLogf(s.T(), "waiting for generated Envoy deployment")
	envoyDeploy := s.envoyDeployment()
	waitDeployment(s.Ctx, s.T(), "envoy-gateway-system", envoyDeploy)

	liveLogf(s.T(), "opening Envoy admin port-forward")
	adminURL, stopAdmin := portForward(s.Ctx, s.T(), "envoy-gateway-system", "deploy/"+envoyDeploy, 19000)
	defer stopAdmin()

	clusterName := backendClusterName
	liveLogf(s.T(), "using generated backend cluster name: %q", clusterName)

	liveLogf(s.T(), "applying EPP (upgrade_configs + STATIC loopback cluster)")
	renderApply(s.Ctx, s.T(), filepath.Join(s.Dir, "k8s", "epp.tmpl.yaml"), map[string]string{
		"ClusterName": clusterName,
	})
	waitEnvoyPatchPolicyProgrammed(s.Ctx, s.T(), "ws-proxy")
	liveLogf(s.T(), "EPP programmed")
	s.waitEnvoyConfigApplied(adminURL, clusterName)

	liveLogf(s.T(), "opening Gateway port-forward")
	gatewayURL, stopGateway := portForward(s.Ctx, s.T(), "envoy-gateway-system",
		"service/"+s.envoyService(), 80)
	defer stopGateway()

	wsURL := "ws://" + strings.TrimPrefix(gatewayURL, "http://") + "/v1/responses"
	httpURL := gatewayURL + "/v1/responses"

	// ── Gate 1 verification ──────────────────────────────────────────────────
	// Attempt a WS dial. If upgrade_configs is missing, Envoy returns 426.
	// If Gate 1 passed the EPP injection, we get through to the echo server.
	liveLogf(s.T(), "Gate 1: verifying upgrade_configs via config_dump")
	s.assertUpgradeConfigsPresent(adminURL)

	// ── Gate 3 verification ──────────────────────────────────────────────────
	// Plain HTTP to /v1/responses — must return 400 from the embedded server,
	// not 404 from EG or 426 from Envoy.
	liveLogf(s.T(), "Gate 3: plain HTTP to /v1/responses → expect 400")
	req, err := http.NewRequest(http.MethodGet, httpURL, nil)
	require.NoError(s.T(), err)
	req.Host = gatewayHost
	resp, err := http.DefaultClient.Do(req)
	require.NoError(s.T(), err)
	defer resp.Body.Close()
	require.Equal(s.T(), http.StatusBadRequest, resp.StatusCode,
		"Gate 3 FAIL: expected 400 from embedded server, got %d", resp.StatusCode)
	liveLogf(s.T(), "Gate 3 PASS: plain HTTP returned 400")

	// ── Gate 2 verification ──────────────────────────────────────────────────
	// WS echo: send 10 frames of varying sizes, verify all arrive intact.
	liveLogf(s.T(), "Gate 2: WS echo through STATIC loopback cluster")
	conn, _, err := websocket.Dial(s.Ctx, wsURL, &websocket.DialOptions{
		Host: gatewayHost,
	})
	require.NoError(s.T(), err, "Gate 2 FAIL: WS dial failed")
	defer conn.CloseNow()

	for i := 0; i < 10; i++ {
		payload := fmt.Sprintf(`{"type":"ping","seq":%d,"pad":"%s"}`, i, strings.Repeat("x", i*100))
		err := conn.Write(s.Ctx, websocket.MessageText, []byte(payload))
		require.NoError(s.T(), err)
		_, data, err := conn.Read(s.Ctx)
		require.NoError(s.T(), err)
		require.Equal(s.T(), payload, string(data),
			"Gate 2 FAIL: frame %d arrived corrupted or out of order", i)
	}
	conn.Close(websocket.StatusNormalClosure, "done")
	liveLogf(s.T(), "Gate 2 PASS: 10 frames echoed intact")

	liveLogf(s.T(), "all three gates PASS")
}

// assertUpgradeConfigsPresent reads the Envoy config_dump and verifies that
// upgrade_configs: websocket is present on the tcp-80 listener. This is the
// definitive Gate 1 check -- if the EPP injection failed, this assertion fails
// before any WS dial is attempted.
func (s *wsProxySuite) assertUpgradeConfigsPresent(adminURL string) {
	s.T().Helper()
	body := httpGet(s.Ctx, s.T(), adminURL+"/config_dump")
	require.Contains(s.T(), string(body), "upgrade_type",
		"Gate 1 FAIL: upgrade_configs not found in Envoy config_dump; EPP injection may have failed")
	liveLogf(s.T(), "Gate 1 PASS: upgrade_configs present in config_dump")
}

func (s *wsProxySuite) waitEnvoyConfigApplied(adminURL, clusterName string) {
	s.T().Helper()
	liveLogf(s.T(), "waiting for Envoy active config to include EPP listener and cluster patches")
	var last string
	deadline := time.Now().Add(60 * time.Second)
	for time.Now().Before(deadline) {
		last = string(httpGet(s.Ctx, s.T(), adminURL+"/config_dump"))
		filterNamePresent := strings.Contains(last, `"filterName": "ws-proxy"`) ||
			strings.Contains(last, `"filter_name": "ws-proxy"`)
		if strings.Contains(last, "envoy.extensions.filters.http.dynamic_modules.v3.DynamicModuleFilter") &&
			filterNamePresent &&
			strings.Contains(last, clusterName) &&
			strings.Contains(last, "127.0.0.1") &&
			strings.Contains(last, "10001") {
			liveLogf(s.T(), "Envoy active config includes EPP listener and cluster patches")
			return
		}
		select {
		case <-s.Ctx.Done():
			require.NoError(s.T(), s.Ctx.Err())
		case <-time.After(500 * time.Millisecond):
		}
	}
	require.Contains(s.T(), last, "envoy.extensions.filters.http.dynamic_modules.v3.DynamicModuleFilter",
		"active listener does not include ws-proxy DynamicModuleFilter")
	require.Truef(s.T(),
		strings.Contains(last, `"filterName": "ws-proxy"`) ||
			strings.Contains(last, `"filter_name": "ws-proxy"`),
		"active listener does not include ws-proxy filter_name")
	require.Contains(s.T(), last, clusterName,
		"active config does not include discovered backend cluster")
	require.Contains(s.T(), last, "127.0.0.1",
		"active cluster replacement does not point at loopback")
	require.Contains(s.T(), last, "10001",
		"active cluster replacement does not point at ws-proxy port")
	liveLogf(s.T(), "Envoy active config includes EPP listener and cluster patches")
}

// envoyDeployment waits for and returns the name of the EG-generated Envoy deployment.
func (s *wsProxySuite) envoyDeployment() string {
	s.T().Helper()
	return s.generatedResourceName("deploy")
}

// envoyService waits for and returns the name of the EG-generated Envoy service.
func (s *wsProxySuite) envoyService() string {
	s.T().Helper()
	return s.generatedResourceName("svc")
}

func (s *wsProxySuite) generatedResourceName(kind string) string {
	s.T().Helper()
	liveLogf(s.T(), "waiting for generated Envoy %s", kind)
	deadline := time.Now().Add(120 * time.Second)
	var last string
	for time.Now().Before(deadline) {
		last = egtest.Output(s.Ctx, s.T(), "", "kubectl", "get", kind,
			"-n", "envoy-gateway-system",
			"-l", "gateway.envoyproxy.io/owning-gateway-namespace=default,gateway.envoyproxy.io/owning-gateway-name=ws-proxy",
			"-o", `jsonpath={range .items[*]}{.metadata.name}{'\n'}{end}`)
		if fields := strings.Fields(last); len(fields) > 0 {
			return fields[0]
		}
		select {
		case <-s.Ctx.Done():
			require.NoError(s.T(), s.Ctx.Err())
		case <-time.After(500 * time.Millisecond):
		}
	}
	require.Failf(s.T(), "generated resource not found",
		"Envoy %s for Gateway ws-proxy not found; last output: %q", kind, last)
	return ""
}

func httpGet(ctx context.Context, t *testing.T, url string) []byte {
	t.Helper()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	require.NoError(t, err)
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	return body
}
