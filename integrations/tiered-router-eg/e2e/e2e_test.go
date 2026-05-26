package e2e

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/stretchr/testify/suite"

	"github.com/dio/transit/integrations/internal/egtest"
)

const gatewayHost = "tiered-router.example.com"

func TestTieredRouterEnvoyGateway(t *testing.T) {
	if os.Getenv("RUN_TIERED_ROUTER_EG_E2E") != "1" {
		t.Skip("set RUN_TIERED_ROUTER_EG_E2E=1 to run the k3d Envoy Gateway integration")
	}
	suite.Run(t, &tieredRouterSuite{
		envoyGatewaySuite: envoyGatewaySuite{
			Suite: egtest.Suite{
				Cluster: "transit-tiered-router-eg",
				Timeout: 14 * time.Minute,
			},
		},
	})
}

type tieredRouterSuite struct {
	envoyGatewaySuite

	envoyImage   string
	controlImage string
}

func (s *tieredRouterSuite) SetupSuite() {
	s.envoyImage = requireEnv(s.T(), "IMAGE")
	s.controlImage = requireEnv(s.T(), "CONTROL_PLANE_IMAGE")

	s.envoyGatewaySuite.SetupSuite()
	s.ImportImages(s.envoyImage, s.controlImage)
}

func (s *tieredRouterSuite) TestL1SelectsPhysicalL2ShardServices() {
	liveLogf(s.T(), "verifying Envoy Gateway install")
	s.verifyEnvoyGatewayInstall()

	liveLogf(s.T(), "applying tiered workloads")
	renderApply(s.Ctx, s.T(), filepath.Join(s.Dir, "k8s", "envoyproxies.tmpl.yaml"), map[string]string{
		"EnvoyImage": s.envoyImage,
	})
	renderApply(s.Ctx, s.T(), filepath.Join(s.Dir, "k8s", "demo.tmpl.yaml"), map[string]string{
		"ControlPlaneImage": s.controlImage,
	})
	apply(s.Ctx, s.T(), filepath.Join(s.Dir, "k8s", "gateways.yaml"))
	apply(s.Ctx, s.T(), filepath.Join(s.Dir, "k8s", "httproutes.yaml"))

	liveLogf(s.T(), "waiting for demo pods and Gateways")
	waitReady(s.Ctx, s.T(), dataplaneNamespace, "app=tiered-router-control")
	waitReady(s.Ctx, s.T(), dataplaneNamespace, "app=upstream-a")
	waitReady(s.Ctx, s.T(), dataplaneNamespace, "app=upstream-c")
	run(s.Ctx, s.T(), "", "kubectl", "wait", "gateway/l1", "-n", dataplaneNamespace, "--for=condition=Accepted", "--timeout=120s")
	run(s.Ctx, s.T(), "", "kubectl", "wait", "gateway/l2-a", "-n", dataplaneNamespace, "--for=condition=Accepted", "--timeout=120s")
	run(s.Ctx, s.T(), "", "kubectl", "wait", "gateway/l2-b", "-n", dataplaneNamespace, "--for=condition=Accepted", "--timeout=120s")
	egtest.WaitGatewayProgrammed(s.Ctx, s.T(), dataplaneNamespace, "l1")
	egtest.WaitGatewayProgrammed(s.Ctx, s.T(), dataplaneNamespace, "l2-a")
	egtest.WaitGatewayProgrammed(s.Ctx, s.T(), dataplaneNamespace, "l2-b")

	liveLogf(s.T(), "waiting for generated Envoy deployments")
	envoyDeploy := generatedResourceName(s.Ctx, s.T(), dataplaneNamespace, "l1", "deploy")
	waitDeployment(s.Ctx, s.T(), dataplaneNamespace, envoyDeploy)
	run(s.Ctx, s.T(), "", "kubectl", "wait", "pods", "--for=condition=Ready", "-n", dataplaneNamespace, "-l", "transit.dio/proxy=l1", "--timeout=180s")
	l2ADeploy := generatedResourceName(s.Ctx, s.T(), dataplaneNamespace, "l2-a", "deploy")
	waitDeployment(s.Ctx, s.T(), dataplaneNamespace, l2ADeploy)
	run(s.Ctx, s.T(), "", "kubectl", "wait", "pods", "--for=condition=Ready", "-n", dataplaneNamespace, "-l", "transit.dio/proxy=l2-a", "--timeout=180s")
	l2BDeploy := generatedResourceName(s.Ctx, s.T(), dataplaneNamespace, "l2-b", "deploy")
	waitDeployment(s.Ctx, s.T(), dataplaneNamespace, l2BDeploy)
	run(s.Ctx, s.T(), "", "kubectl", "wait", "pods", "--for=condition=Ready", "-n", dataplaneNamespace, "-l", "transit.dio/proxy=l2-b", "--timeout=180s")

	liveLogf(s.T(), "patching generated L2 clusters with cluster-router")
	l2AAdminURL, stopL2AAdmin := portForward(s.Ctx, s.T(), "deploy/"+l2ADeploy, 19000)
	defer stopL2AAdmin()
	l2AClusterName := egtest.DiscoverBackendCluster(s.Ctx, s.T(), l2AAdminURL, dataplaneNamespace, "l2-a")
	renderApply(s.Ctx, s.T(), filepath.Join(s.Dir, "k8s", "epp-l2.tmpl.yaml"), map[string]string{
		"PolicyName":    "l2-a-cluster-router",
		"GatewayName":   "l2-a",
		"ClusterName":   l2AClusterName,
		"Shard":         "a",
		"InitialTarget": "upstream-a.transit-dataplane.svc.cluster.local:8080",
		"Provider":      "openai",
		"AuthHeader":    "Bearer shard-a-openai-token",
		"Profile":       "profile-a",
		"BYOKKeyID":     "key-a-001",
	})
	waitEnvoyPatchPolicyProgrammed(s.Ctx, s.T(), dataplaneNamespace, "l2-a-cluster-router")
	l2AURL, stopL2A := portForward(s.Ctx, s.T(), "service/l2-a", 80)
	defer stopL2A()

	l2BAdminURL, stopL2BAdmin := portForward(s.Ctx, s.T(), "deploy/"+l2BDeploy, 19000)
	defer stopL2BAdmin()
	l2BClusterName := egtest.DiscoverBackendCluster(s.Ctx, s.T(), l2BAdminURL, dataplaneNamespace, "l2-b")
	renderApply(s.Ctx, s.T(), filepath.Join(s.Dir, "k8s", "epp-l2.tmpl.yaml"), map[string]string{
		"PolicyName":    "l2-b-cluster-router",
		"GatewayName":   "l2-b",
		"ClusterName":   l2BClusterName,
		"Shard":         "b",
		"InitialTarget": "upstream-c.transit-dataplane.svc.cluster.local:8080",
		"Provider":      "openai",
		"AuthHeader":    "Bearer shard-b-openai-token",
		"Profile":       "profile-b",
		"BYOKKeyID":     "key-b-001",
	})
	waitEnvoyPatchPolicyProgrammed(s.Ctx, s.T(), dataplaneNamespace, "l2-b-cluster-router")
	l2BURL, stopL2B := portForward(s.Ctx, s.T(), "service/l2-b", 80)
	defer stopL2B()

	liveLogf(s.T(), "opening L1 Envoy admin port-forward")
	adminURL, stopAdmin := portForward(s.Ctx, s.T(), "deploy/"+envoyDeploy, 19000)
	defer stopAdmin()
	clusterName := egtest.DiscoverBackendCluster(s.Ctx, s.T(), adminURL, dataplaneNamespace, "l1-public")
	liveLogf(s.T(), "patching generated L1 cluster %q", clusterName)
	renderApply(s.Ctx, s.T(), filepath.Join(s.Dir, "k8s", "epp-l1.tmpl.yaml"), map[string]string{
		"ClusterName": clusterName,
	})
	waitEnvoyPatchPolicyProgrammed(s.Ctx, s.T(), dataplaneNamespace, "l1-cluster-shard-router")

	liveLogf(s.T(), "opening L1 Gateway port-forward")
	gatewayURL, stopGateway := portForward(s.Ctx, s.T(), "service/l1", 80)
	defer stopGateway()

	liveLogf(s.T(), "asserting explicit tag shard routing through physical L2 services")
	assertL1Route(s.Ctx, s.T(), gatewayURL, expectedRoute{
		Tag:       "a-demo",
		Upstream:  "upstream-a",
		L1Shard:   "a",
		L1Target:  "l2-a.transit-dataplane.svc.cluster.local:80",
		Provider:  "openai",
		Profile:   "profile-a",
		BYOKKeyID: "key-a-001",
		Auth:      "Bearer shard-a-openai-token",
		L2Version: "bootstrap",
	})
	assertL1Route(s.Ctx, s.T(), gatewayURL, expectedRoute{
		Tag:       "b-demo",
		Upstream:  "upstream-c",
		L1Shard:   "b",
		L1Target:  "l2-b.transit-dataplane.svc.cluster.local:80",
		Provider:  "openai",
		Profile:   "profile-b",
		BYOKKeyID: "key-b-001",
		Auth:      "Bearer shard-b-openai-token",
		L2Version: "bootstrap",
	})

	liveLogf(s.T(), "asserting redacted active L2 config dumps")
	assertL2Dump(s.Ctx, s.T(), l2AURL, expectedL2Dump{
		Shard:     "a",
		Model:     "gpt-fast",
		Target:    "upstream-a.transit-dataplane.svc.cluster.local:8080",
		Provider:  "openai",
		Profile:   "profile-a",
		BYOKKeyID: "key-a-001",
		Secrets: []string{
			"Bearer ",
			"shard-a-openai-token",
			"shard-b-openai-token",
			"auth_header",
		},
	})
	assertL2Dump(s.Ctx, s.T(), l2BURL, expectedL2Dump{
		Shard:     "b",
		Model:     "gpt-fast",
		Target:    "upstream-c.transit-dataplane.svc.cluster.local:8080",
		Provider:  "openai",
		Profile:   "profile-b",
		BYOKKeyID: "key-b-001",
		Secrets: []string{
			"Bearer ",
			"shard-a-openai-token",
			"shard-b-openai-token",
			"auth_header",
		},
	})
}

type upstreamResponse struct {
	Upstream  string `json:"upstream"`
	L1Tag     string `json:"l1_tag"`
	L1Source  string `json:"l1_source"`
	L1Shard   string `json:"l1_shard"`
	L1Target  string `json:"l1_target"`
	L1Version string `json:"l1_version"`
	Model     string `json:"model"`
	Provider  string `json:"provider"`
	Profile   string `json:"profile"`
	BYOKKeyID string `json:"byok_key_id"`
	Auth      string `json:"auth"`
	L2Version string `json:"l2_version"`
}

type expectedRoute struct {
	Tag       string
	Upstream  string
	L1Shard   string
	L1Target  string
	Provider  string
	Profile   string
	BYOKKeyID string
	Auth      string
	L2Version string
}

func assertL1Route(ctx context.Context, t *testing.T, gatewayURL string, want expectedRoute) {
	t.Helper()
	eventually(ctx, t, func() error {
		got, err := requestL1(ctx, gatewayURL, want.Tag)
		if err != nil {
			return err
		}
		if got.Upstream != want.Upstream {
			return fmt.Errorf("tag %s upstream = %q, want %q", want.Tag, got.Upstream, want.Upstream)
		}
		if got.L1Shard != want.L1Shard {
			return fmt.Errorf("tag %s l1_shard = %q, want %q", want.Tag, got.L1Shard, want.L1Shard)
		}
		if got.L1Target != want.L1Target {
			return fmt.Errorf("tag %s l1_target = %q, want %q", want.Tag, got.L1Target, want.L1Target)
		}
		if got.L1Tag != want.Tag {
			return fmt.Errorf("tag %s l1_tag = %q", want.Tag, got.L1Tag)
		}
		if got.L1Source != "tag" {
			return fmt.Errorf("tag %s l1_source = %q, want tag", want.Tag, got.L1Source)
		}
		if got.L1Version != "l1-to-l2" {
			return fmt.Errorf("tag %s l1_version = %q, want l1-to-l2", want.Tag, got.L1Version)
		}
		if got.Provider != want.Provider {
			return fmt.Errorf("tag %s provider = %q, want %q", want.Tag, got.Provider, want.Provider)
		}
		if got.Profile != want.Profile {
			return fmt.Errorf("tag %s profile = %q, want %q", want.Tag, got.Profile, want.Profile)
		}
		if got.BYOKKeyID != want.BYOKKeyID {
			return fmt.Errorf("tag %s byok_key_id = %q, want %q", want.Tag, got.BYOKKeyID, want.BYOKKeyID)
		}
		if got.Auth != want.Auth {
			return fmt.Errorf("tag %s auth = %q, want %q", want.Tag, got.Auth, want.Auth)
		}
		if got.L2Version != want.L2Version {
			return fmt.Errorf("tag %s l2_version = %q, want %q", want.Tag, got.L2Version, want.L2Version)
		}
		return nil
	})
}

func requestL1(ctx context.Context, gatewayURL, tag string) (upstreamResponse, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, gatewayURL+"/", nil)
	if err != nil {
		return upstreamResponse{}, err
	}
	req.Host = gatewayHost
	req.Header.Set("x-transit-tag", tag)
	req.Header.Set("x-model", "gpt-fast")
	raw, status, err := egtest.DoRequest(req)
	if err != nil {
		return upstreamResponse{}, err
	}
	if status != http.StatusOK {
		return upstreamResponse{}, fmt.Errorf("tag %s status %d body %s", tag, status, raw)
	}
	var got upstreamResponse
	if err := json.Unmarshal(raw, &got); err != nil {
		return upstreamResponse{}, err
	}
	return got, nil
}

type expectedL2Dump struct {
	Shard     string
	Model     string
	Target    string
	Provider  string
	Profile   string
	BYOKKeyID string
	Secrets   []string
}

type l2DebugDump struct {
	Version string                  `json:"version"`
	Models  map[string]l2DebugModel `json:"models"`
}

type l2DebugModel struct {
	Target    string `json:"target"`
	Address   string `json:"address"`
	Provider  string `json:"provider"`
	Profile   string `json:"profile"`
	BYOKKeyID string `json:"byok_key_id"`
}

func assertL2Dump(ctx context.Context, t *testing.T, l2URL string, want expectedL2Dump) {
	t.Helper()
	eventually(ctx, t, func() error {
		raw, err := requestDebugDump(ctx, l2URL)
		if err != nil {
			return err
		}
		body := string(raw)
		for _, secret := range want.Secrets {
			if strings.Contains(body, secret) {
				return fmt.Errorf("l2 %s active dump leaked %q: %s", want.Shard, secret, body)
			}
		}
		var dump l2DebugDump
		if err := json.Unmarshal(raw, &dump); err != nil {
			return err
		}
		if dump.Version != "bootstrap" {
			return fmt.Errorf("l2 %s version = %q, want bootstrap", want.Shard, dump.Version)
		}
		got, ok := dump.Models[want.Model]
		if !ok {
			return fmt.Errorf("l2 %s missing model %q in dump: %s", want.Shard, want.Model, body)
		}
		if got.Target != want.Target {
			return fmt.Errorf("l2 %s model %s target = %q, want %q", want.Shard, want.Model, got.Target, want.Target)
		}
		if got.Provider != want.Provider {
			return fmt.Errorf("l2 %s model %s provider = %q, want %q", want.Shard, want.Model, got.Provider, want.Provider)
		}
		if got.Profile != want.Profile {
			return fmt.Errorf("l2 %s model %s profile = %q, want %q", want.Shard, want.Model, got.Profile, want.Profile)
		}
		if got.BYOKKeyID != want.BYOKKeyID {
			return fmt.Errorf("l2 %s model %s byok_key_id = %q, want %q", want.Shard, want.Model, got.BYOKKeyID, want.BYOKKeyID)
		}
		if got.Address == "" {
			return fmt.Errorf("l2 %s model %s address is empty", want.Shard, want.Model)
		}
		return nil
	})
}

func requestDebugDump(ctx context.Context, l2URL string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, l2URL+"/__cluster-router/config", nil)
	if err != nil {
		return nil, err
	}
	raw, status, err := egtest.DoRequest(req)
	if err != nil {
		return nil, err
	}
	if status != http.StatusOK {
		return nil, fmt.Errorf("debug dump status %d body %s", status, raw)
	}
	return raw, nil
}

func get(ctx context.Context, t *testing.T, url string) []byte {
	t.Helper()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	require.NoError(t, err)
	body, status, err := egtest.DoRequest(req)
	require.NoError(t, err)
	require.Equalf(t, http.StatusOK, status, "GET %s body %s", url, body)
	return body
}
