package e2e

import (
	"context"
	"os"
	"path/filepath"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/stretchr/testify/suite"
)

// envoyGatewaySuite is the shared k3d plus Envoy Gateway harness for this
// integration. It deliberately owns the cluster lifecycle so each suite starts
// from a clean Gateway API and Envoy Gateway install.
type envoyGatewaySuite struct {
	suite.Suite

	ctx    context.Context
	cancel context.CancelFunc

	root      string
	dir       string
	cluster   string
	timeout   time.Duration
	egVersion string
	k3sTag    string
	k3dAgents string
	reset     bool
}

func (s *envoyGatewaySuite) SetupSuite() {
	timeout := s.timeout
	if timeout == 0 {
		timeout = 10 * time.Minute
	}
	s.ctx, s.cancel = context.WithTimeout(context.Background(), timeout)
	s.root = repoRoot(s.T())
	s.dir = filepath.Join(s.root, "integrations", "cluster-router-eg")
	s.egVersion = envOr("ENVOY_GATEWAY_VERSION", "v1.8.0")
	s.k3sTag = envOr("K3S_TAG", "v1.31.6-k3s1")
	s.k3dAgents = envOr("K3D_AGENTS", "0")
	s.reset = envOr("RESET_CLUSTER", "1") != "0"
	require.NotEmpty(s.T(), s.cluster)
	require.NoError(s.T(), os.Setenv("KUBECTL_CONTEXT", "k3d-"+s.cluster))

	s.createK3d()
	s.installEnvoyGateway()
}

func (s *envoyGatewaySuite) TearDownSuite() {
	if s.cancel != nil {
		defer s.cancel()
	}
	defer func() { _ = os.Unsetenv("KUBECTL_CONTEXT") }()
	if os.Getenv("KEEP_CLUSTER") != "1" {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), time.Minute)
		defer cleanupCancel()
		deleteK3dCluster(cleanupCtx, s.T(), s.cluster)
	}
}

func (s *envoyGatewaySuite) createK3d() {
	// Delete first so a failed or KEEP_CLUSTER debugging run never pollutes the
	// next suite execution. KEEP_CLUSTER only affects teardown; RESET_CLUSTER=0
	// is the explicit escape hatch when inspecting a preserved cluster.
	if s.reset {
		liveLogf(s.T(), "resetting k3d cluster %s before test start; KEEP_CLUSTER only affects teardown", s.cluster)
		deleteK3dCluster(s.ctx, s.T(), s.cluster)
	} else {
		liveLogf(s.T(), "RESET_CLUSTER=0: checking for reusable k3d cluster %s", s.cluster)
		if k3dClusterExists(s.ctx, s.cluster) {
			liveLogf(s.T(), "reusing existing k3d cluster %s", s.cluster)
			run(s.ctx, s.T(), "", "kubectl", "wait", "--for=condition=Ready", "nodes/k3d-"+s.cluster+"-server-0", "--timeout=60s")
			return
		}
		liveLogf(s.T(), "k3d cluster %s does not exist; creating it", s.cluster)
	}
	liveLogf(s.T(), "creating single-node k3d cluster %s", s.cluster)
	run(s.ctx, s.T(), "", "k3d", "cluster", "create", s.cluster,
		"--agents", s.k3dAgents,
		"--image", "rancher/k3s:"+s.k3sTag,
		"--k3s-arg", "--disable=traefik@server:*",
		"--k3s-arg", "--kubelet-arg=allowed-unsafe-sysctls=net.ipv4.ip_unprivileged_port_start@server:*")
	run(s.ctx, s.T(), "", "kubectl", "wait", "--for=condition=Ready", "nodes/k3d-"+s.cluster+"-server-0", "--timeout=60s")
}

func (s *envoyGatewaySuite) installEnvoyGateway() {
	liveLogf(s.T(), "installing Envoy Gateway %s", s.egVersion)
	run(s.ctx, s.T(), "", "helm", "upgrade", "--install", "eg", "oci://docker.io/envoyproxy/gateway-helm",
		"--version", s.egVersion,
		"-n", "envoy-gateway-system",
		"--create-namespace")
	s.waitEnvoyGateway()

	liveLogf(s.T(), "enabling EnvoyPatchPolicy in Envoy Gateway")
	apply(s.ctx, s.T(), filepath.Join(s.dir, "k8s", "envoy-gateway-config.yaml"))
	run(s.ctx, s.T(), "", "kubectl", "rollout", "restart", "deployment/envoy-gateway", "-n", "envoy-gateway-system")
	s.waitEnvoyGateway()
}

func (s *envoyGatewaySuite) waitEnvoyGateway() {
	// Deployment readiness is less noisy than waiting on pods by label. Old
	// Completed pods or restart history should not affect this assertion.
	run(s.ctx, s.T(), "", "kubectl", "rollout", "status", "deployment/envoy-gateway", "-n", "envoy-gateway-system", "--timeout=120s")
	run(s.ctx, s.T(), "", "kubectl", "wait", "deployment/envoy-gateway", "-n", "envoy-gateway-system", "--for=condition=Available", "--timeout=120s")
}

func (s *envoyGatewaySuite) verifyEnvoyGatewayInstall() {
	run(s.ctx, s.T(), "", "kubectl", "get", "crd", "gatewayclasses.gateway.networking.k8s.io")
	run(s.ctx, s.T(), "", "kubectl", "get", "crd", "gateways.gateway.networking.k8s.io")
	run(s.ctx, s.T(), "", "kubectl", "get", "crd", "httproutes.gateway.networking.k8s.io")
	run(s.ctx, s.T(), "", "kubectl", "get", "crd", "envoyproxies.gateway.envoyproxy.io")
	run(s.ctx, s.T(), "", "kubectl", "get", "crd", "envoypatchpolicies.gateway.envoyproxy.io")

	config := output(s.ctx, s.T(), "", "kubectl", "get", "configmap", "envoy-gateway-config", "-n", "envoy-gateway-system", "-o", "yaml")
	require.Contains(s.T(), config, "enableEnvoyPatchPolicy: true")
	require.Contains(s.T(), config, "XDSNameSchemeV2")
}
