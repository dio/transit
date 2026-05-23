package e2e

import (
	"bytes"
	"context"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"text/template"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/stretchr/testify/suite"
)

const (
	systemNamespace    = "transit-system"
	dataplaneNamespace = "transit-dataplane"
)

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
	s.dir = filepath.Join(s.root, "integrations", "mcp-profile-tiered-router-eg")
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
		"-n", systemNamespace,
		"--create-namespace")
	s.waitEnvoyGateway()

	liveLogf(s.T(), "enabling EnvoyPatchPolicy and Gateway Namespace mode")
	apply(s.ctx, s.T(), filepath.Join(s.dir, "k8s", "envoy-gateway-config.yaml"))
	run(s.ctx, s.T(), "", "kubectl", "rollout", "restart", "deployment/envoy-gateway", "-n", systemNamespace)
	s.waitEnvoyGateway()
}

func (s *envoyGatewaySuite) waitEnvoyGateway() {
	run(s.ctx, s.T(), "", "kubectl", "rollout", "status", "deployment/envoy-gateway", "-n", systemNamespace, "--timeout=120s")
	run(s.ctx, s.T(), "", "kubectl", "wait", "deployment/envoy-gateway", "-n", systemNamespace, "--for=condition=Available", "--timeout=120s")
}

func (s *envoyGatewaySuite) verifyEnvoyGatewayInstall() {
	run(s.ctx, s.T(), "", "kubectl", "get", "crd", "gatewayclasses.gateway.networking.k8s.io")
	run(s.ctx, s.T(), "", "kubectl", "get", "crd", "gateways.gateway.networking.k8s.io")
	run(s.ctx, s.T(), "", "kubectl", "get", "crd", "httproutes.gateway.networking.k8s.io")
	run(s.ctx, s.T(), "", "kubectl", "get", "crd", "envoyproxies.gateway.envoyproxy.io")
	run(s.ctx, s.T(), "", "kubectl", "get", "crd", "envoypatchpolicies.gateway.envoyproxy.io")

	config := output(s.ctx, s.T(), "", "kubectl", "get", "configmap", "envoy-gateway-config", "-n", systemNamespace, "-o", "yaml")
	require.Contains(s.T(), config, "enableEnvoyPatchPolicy: true")
	require.Contains(s.T(), config, "GatewayNamespace")
	require.Contains(s.T(), config, "XDSNameSchemeV2")
}

func apply(ctx context.Context, t *testing.T, path string) {
	t.Helper()
	run(ctx, t, "", "kubectl", "apply", "-f", path)
}

func renderApply(ctx context.Context, t *testing.T, path string, data any) {
	t.Helper()
	raw, err := os.ReadFile(path)
	require.NoError(t, err)
	tmpl, err := template.New(filepath.Base(path)).Parse(string(raw))
	require.NoError(t, err)
	var out bytes.Buffer
	require.NoError(t, tmpl.Execute(&out, data))
	run(ctx, t, out.String(), "kubectl", "apply", "-f", "-")
}

func waitReady(ctx context.Context, t *testing.T, namespace, label string) {
	t.Helper()
	run(ctx, t, "", "kubectl", "wait", "pods", "--for=condition=Ready", "-n", namespace, "-l", label, "--timeout=120s")
}

func waitDeployment(ctx context.Context, t *testing.T, namespace, name string) {
	t.Helper()
	run(ctx, t, "", "kubectl", "rollout", "status", "deployment/"+name, "-n", namespace, "--timeout=180s")
	run(ctx, t, "", "kubectl", "wait", "deployment/"+name, "-n", namespace, "--for=condition=Available", "--timeout=180s")
}

func waitEnvoyPatchPolicyProgrammed(ctx context.Context, t *testing.T, namespace, name string) {
	t.Helper()
	deadline := time.Now().Add(120 * time.Second)
	var last string
	for time.Now().Before(deadline) {
		last = output(ctx, t, "", "kubectl", "get", "envoypatchpolicy", name, "-n", namespace,
			"-o", "jsonpath={range .status.ancestors[*].conditions[*]}{.type}={.status}:{.reason}:{.message}{'\\n'}{end}")
		if strings.Contains(last, "Programmed=True") {
			return
		}
		select {
		case <-ctx.Done():
			require.NoError(t, ctx.Err())
		case <-time.After(500 * time.Millisecond):
		}
	}
	require.Failf(t, "EnvoyPatchPolicy not programmed", "last conditions:\n%s", last)
}

func generatedResourceName(ctx context.Context, t *testing.T, namespace, gateway, kind string) string {
	t.Helper()
	liveLogf(t, "waiting for generated Envoy %s for Gateway %s", kind, gateway)
	deadline := time.Now().Add(120 * time.Second)
	var last string
	for time.Now().Before(deadline) {
		last = output(ctx, t, "", "kubectl", "get", kind, "-n", namespace,
			"-l", "gateway.envoyproxy.io/owning-gateway-namespace="+namespace+",gateway.envoyproxy.io/owning-gateway-name="+gateway,
			"-o", "jsonpath={range .items[*]}{.metadata.name}{'\\n'}{end}")
		fields := strings.Fields(last)
		if len(fields) > 0 {
			return fields[0]
		}
		select {
		case <-ctx.Done():
			require.NoError(t, ctx.Err())
		case <-time.After(500 * time.Millisecond):
		}
	}
	require.Failf(t, "generated Envoy resource not found", "%s/%s for Gateway %s not found; last output: %q", namespace, kind, gateway, last)
	return ""
}

func portForward(ctx context.Context, t *testing.T, target string, remotePort int) (string, func()) {
	t.Helper()
	port := freePort(t)
	pctx, cancel := context.WithCancel(ctx)
	args := append([]string{"--context", kubeContext(t), "-n", dataplaneNamespace, "port-forward", target}, fmt.Sprintf("%d:%d", port, remotePort))
	cmd := exec.CommandContext(pctx, "kubectl", args...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	cmd.Stdout = &stderr
	if err := cmd.Start(); err != nil {
		cancel()
		require.NoError(t, err)
	}
	stop := func() {
		cancel()
		_ = cmd.Wait()
	}
	url := fmt.Sprintf("http://127.0.0.1:%d", port)
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		conn, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", port), 200*time.Millisecond)
		if err == nil {
			_ = conn.Close()
			return url, stop
		}
		time.Sleep(100 * time.Millisecond)
	}
	stop()
	require.Failf(t, "port-forward not ready", "port-forward %s/%s did not become ready: %s", dataplaneNamespace, target, stderr.String())
	return "", func() {}
}

func eventually(ctx context.Context, t *testing.T, check func() error) {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	var last error
	for time.Now().Before(deadline) {
		if err := check(); err == nil {
			return
		} else {
			last = err
		}
		select {
		case <-ctx.Done():
			require.NoError(t, ctx.Err())
		case <-time.After(200 * time.Millisecond):
		}
	}
	require.NoErrorf(t, last, "condition did not pass")
}

func run(ctx context.Context, t *testing.T, stdin, name string, args ...string) {
	t.Helper()
	_ = output(ctx, t, stdin, name, args...)
}

func output(ctx context.Context, t *testing.T, stdin, name string, args ...string) string {
	t.Helper()
	args = scopedCommandArgs(t, name, args)
	liveLogf(t, "$ %s %s%s", name, strings.Join(args, " "), stdinNote(stdin))
	start := time.Now()
	cmd := exec.CommandContext(ctx, name, args...)
	if stdin != "" {
		cmd.Stdin = strings.NewReader(stdin)
	}
	out, err := cmd.CombinedOutput()
	require.NoErrorf(t, err, "%s %s failed:\n%s", name, strings.Join(args, " "), out)
	liveLogf(t, "ok (%s)", time.Since(start).Round(time.Millisecond))
	return strings.TrimSpace(string(out))
}

func deleteK3dCluster(ctx context.Context, t *testing.T, cluster string) {
	t.Helper()
	liveLogf(t, "$ k3d cluster delete %s", cluster)
	start := time.Now()
	cmd := exec.CommandContext(ctx, "k3d", "cluster", "delete", cluster)
	out, err := cmd.CombinedOutput()
	if err == nil {
		liveLogf(t, "ok k3d cluster delete %s (%s)", cluster, time.Since(start).Round(time.Millisecond))
		return
	}
	text := string(out)
	if strings.Contains(text, "No nodes found") || strings.Contains(text, "not found") {
		liveLogf(t, "ok k3d cluster delete %s: already absent (%s)", cluster, time.Since(start).Round(time.Millisecond))
		return
	}
	require.NoErrorf(t, err, "k3d cluster delete %s failed:\n%s", cluster, out)
}

func k3dClusterExists(ctx context.Context, cluster string) bool {
	cmd := exec.CommandContext(ctx, "k3d", "cluster", "list", cluster, "--no-headers")
	out, err := cmd.CombinedOutput()
	return err == nil && strings.Contains(string(out), cluster)
}

func liveLogf(t *testing.T, format string, args ...any) {
	t.Helper()
	fmt.Fprintf(os.Stderr, "e2e: %s\n", fmt.Sprintf(format, args...))
}

func stdinNote(stdin string) string {
	if stdin == "" {
		return ""
	}
	return fmt.Sprintf(" <stdin:%d bytes>", len(stdin))
}

func scopedCommandArgs(t *testing.T, name string, args []string) []string {
	t.Helper()
	switch name {
	case "kubectl":
		return append([]string{"--context", kubeContext(t)}, args...)
	case "helm":
		return append([]string{"--kube-context", kubeContext(t)}, args...)
	default:
		return args
	}
}

func kubeContext(t *testing.T) string {
	t.Helper()
	contextName := os.Getenv("KUBECTL_CONTEXT")
	require.NotEmpty(t, contextName, "KUBECTL_CONTEXT must be set for integration e2e kubectl/helm commands")
	require.Truef(t, strings.HasPrefix(contextName, "k3d-"), "refusing to run kubectl/helm against non-k3d context %q", contextName)
	return contextName
}

func freePort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	defer func() { _ = l.Close() }()
	return l.Addr().(*net.TCPAddr).Port
}

func repoRoot(t *testing.T) string {
	t.Helper()
	root := output(context.Background(), t, "", "git", "rev-parse", "--show-toplevel")
	require.NotEmpty(t, root)
	return root
}

func requireEnv(t *testing.T, name string) string {
	t.Helper()
	value := os.Getenv(name)
	require.NotEmptyf(t, value, "%s is required", name)
	return value
}

func envOr(name, fallback string) string {
	if value := os.Getenv(name); value != "" {
		return value
	}
	return fallback
}
