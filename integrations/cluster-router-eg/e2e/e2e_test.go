package e2e

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/stretchr/testify/suite"

	"github.com/dio/transit/integrations/internal/egtest"
)

const gatewayHost = "cluster-router.example.com"

func TestClusterRouterEnvoyGateway(t *testing.T) {
	if os.Getenv("RUN_CLUSTER_ROUTER_EG_E2E") != "1" {
		t.Skip("set RUN_CLUSTER_ROUTER_EG_E2E=1 to run the k3d Envoy Gateway integration")
	}
	suite.Run(t, &clusterRouterSuite{
		envoyGatewaySuite: envoyGatewaySuite{
			Suite: egtest.Suite{
				Cluster: "transit-cluster-router-eg",
				Timeout: 14 * time.Minute,
			},
		},
	})
}

type clusterRouterSuite struct {
	envoyGatewaySuite

	envoyImage   string
	controlImage string
}

func (s *clusterRouterSuite) SetupSuite() {
	s.envoyImage = requireEnv(s.T(), "IMAGE")
	s.controlImage = requireEnv(s.T(), "CONTROL_PLANE_IMAGE")

	s.envoyGatewaySuite.SetupSuite()
	s.ImportImages(s.envoyImage, s.controlImage)
}

func (s *clusterRouterSuite) TestClusterRouterEnvoyGateway() {
	liveLogf(s.T(), "verifying Envoy Gateway install")
	s.verifyEnvoyGatewayInstall()

	liveLogf(s.T(), "applying EnvoyProxy and demo workloads")
	renderApply(s.Ctx, s.T(), filepath.Join(s.Dir, "k8s", "envoyproxy.tmpl.yaml"), map[string]string{
		"EnvoyImage": s.envoyImage,
	})
	renderApply(s.Ctx, s.T(), filepath.Join(s.Dir, "k8s", "demo.tmpl.yaml"), map[string]string{
		"ControlPlaneImage": s.controlImage,
	})
	apply(s.Ctx, s.T(), filepath.Join(s.Dir, "k8s", "gateway.yaml"))
	apply(s.Ctx, s.T(), filepath.Join(s.Dir, "k8s", "httproute.yaml"))

	liveLogf(s.T(), "waiting for demo pods and Gateway")
	waitReady(s.Ctx, s.T(), "app=cluster-router-control")
	waitReady(s.Ctx, s.T(), "app=upstream-a")
	waitReady(s.Ctx, s.T(), "app=upstream-b")
	waitReady(s.Ctx, s.T(), "app=upstream-c")
	run(s.Ctx, s.T(), "", "kubectl", "wait", "gateway/cluster-router", "--for=condition=Accepted", "--timeout=120s")

	liveLogf(s.T(), "waiting for generated Envoy deployment")
	envoyDeploy := envoyDeployment(s.Ctx, s.T())
	waitDeployment(s.Ctx, s.T(), "envoy-gateway-system", envoyDeploy)
	liveLogf(s.T(), "opening Envoy admin port-forward")
	adminURL, stopAdmin := portForward(s.Ctx, s.T(), "envoy-gateway-system", "deploy/"+envoyDeploy, 19000)
	defer stopAdmin()
	clusterName := egtest.DiscoverBackendCluster(s.Ctx, s.T(), adminURL, "default", "cluster-router-backend")
	liveLogf(s.T(), "patching generated cluster %q", clusterName)
	renderApply(s.Ctx, s.T(), filepath.Join(s.Dir, "k8s", "epp.tmpl.yaml"), map[string]string{
		"ClusterName": clusterName,
	})
	waitEnvoyPatchPolicyProgrammed(s.Ctx, s.T(), "cluster-router")

	liveLogf(s.T(), "opening Gateway and control-plane port-forwards")
	gatewayURL, stopGateway := portForward(s.Ctx, s.T(), "envoy-gateway-system", "service/"+envoyService(s.Ctx, s.T()), 80)
	defer stopGateway()
	controlURL, stopControl := portForward(s.Ctx, s.T(), "default", "service/cluster-router-control", 8080)
	defer stopControl()

	liveLogf(s.T(), "asserting bootstrap routes")
	assertRoute(s.Ctx, s.T(), gatewayURL, "gpt-fast", upstreamResponse{
		Upstream: "upstream-a",
		Auth:     "Bearer openai-token",
		Provider: "openai",
		Version:  "bootstrap",
	})
	assertRoute(s.Ctx, s.T(), gatewayURL, "claude-safe", upstreamResponse{
		Upstream: "upstream-b",
		Auth:     "Bearer anthropic-token",
		Provider: "anthropic",
		Version:  "bootstrap",
	})

	liveLogf(s.T(), "posting updated model routes")
	postModel(s.Ctx, s.T(), controlURL, modelUpdate{
		Name:       "gpt-slow",
		Target:     "upstream-a.default.svc.cluster.local:8080",
		Provider:   "openai",
		AuthHeader: "Bearer slow-token",
		Version:    "updated",
	})
	postModel(s.Ctx, s.T(), controlURL, modelUpdate{
		Name:       "kimi-fast",
		Target:     "upstream-c.default.svc.cluster.local:8080",
		Provider:   "moonshot",
		AuthHeader: "Bearer moonshot-token",
		Version:    "updated",
	})

	liveLogf(s.T(), "waiting for gpt-slow to use updated config")
	eventually(s.Ctx, s.T(), func() error {
		return checkRoute(s.Ctx, gatewayURL, "gpt-slow", upstreamResponse{
			Upstream: "upstream-a",
			Auth:     "Bearer slow-token",
			Provider: "openai",
			Version:  "updated",
		})
	})
	liveLogf(s.T(), "waiting for kimi-fast to use updated config")
	eventually(s.Ctx, s.T(), func() error {
		return checkRoute(s.Ctx, gatewayURL, "kimi-fast", upstreamResponse{
			Upstream: "upstream-c",
			Auth:     "Bearer moonshot-token",
			Provider: "moonshot",
			Version:  "updated",
		})
	})

	liveLogf(s.T(), "checking redacted control-plane dump")
	dumpBody := get(s.Ctx, s.T(), controlURL+"/dump", nil)
	require.NotContains(s.T(), string(dumpBody), "Bearer ", "dump leaked bearer token")
	require.Contains(s.T(), string(dumpBody), "kimi-fast")
	require.Contains(s.T(), string(dumpBody), "gpt-slow")
}

type modelUpdate struct {
	Name       string `json:"name"`
	Target     string `json:"target"`
	Provider   string `json:"provider"`
	AuthHeader string `json:"auth_header"`
	Version    string `json:"version"`
}

type upstreamResponse struct {
	Upstream string `json:"upstream"`
	Auth     string `json:"auth"`
	Provider string `json:"provider"`
	Version  string `json:"version"`
}

func assertRoute(ctx context.Context, t *testing.T, gatewayURL, model string, want upstreamResponse) {
	t.Helper()
	require.NoError(t, checkRoute(ctx, gatewayURL, model, want))
}

func checkRoute(ctx context.Context, gatewayURL, model string, want upstreamResponse) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, gatewayURL+"/", nil)
	if err != nil {
		return err
	}
	req.Host = gatewayHost
	req.Header.Set("x-model", model)
	raw, status, err := egtest.DoRequest(req)
	if err != nil {
		return err
	}
	if status != http.StatusOK {
		return fmt.Errorf("model %s status %d body %s", model, status, raw)
	}
	var got upstreamResponse
	if err := json.Unmarshal(raw, &got); err != nil {
		return err
	}
	if got != want {
		return fmt.Errorf("model %s response = %+v, want %+v", model, got, want)
	}
	return nil
}

func postModel(ctx context.Context, t *testing.T, controlURL string, update modelUpdate) {
	t.Helper()
	raw, err := json.Marshal(update)
	require.NoError(t, err)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, controlURL+"/models", bytes.NewReader(raw))
	require.NoError(t, err)
	req.Header.Set("content-type", "application/json")
	body, status, err := egtest.DoRequest(req)
	require.NoError(t, err)
	require.Equalf(t, http.StatusOK, status, "POST /models body %s", body)
}

func get(ctx context.Context, t *testing.T, url string, headers map[string]string) []byte {
	t.Helper()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	require.NoError(t, err)
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	body, status, err := egtest.DoRequest(req)
	require.NoError(t, err)
	require.Equalf(t, http.StatusOK, status, "GET %s body %s", url, body)
	return body
}

func envoyDeployment(ctx context.Context, t *testing.T) string {
	t.Helper()
	return egtest.GeneratedResourceName(ctx, t, "envoy-gateway-system", "cluster-router", "deploy")
}

func envoyService(ctx context.Context, t *testing.T) string {
	t.Helper()
	return egtest.GeneratedResourceName(ctx, t, "envoy-gateway-system", "cluster-router", "svc")
}
