// Package egtest holds the shared k3d and Envoy Gateway harness for Transit
// integration e2e suites. Each integration embeds Suite, sets integration-
// specific fields, and calls SetupSuiteBase from its own SetupSuite.
package egtest

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"text/template"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/stretchr/testify/suite"
)

// Suite is the shared k3d + Envoy Gateway harness. Embed it in an integration-
// specific suite struct, set Dir, Cluster, and namespace fields, then call
// SetupSuiteBase from SetupSuite.
type Suite struct {
	suite.Suite

	Ctx    context.Context
	Cancel context.CancelFunc

	// Root is the repository root. SetupSuiteBase populates it.
	Root string

	// Dir is the absolute path to the integration directory.
	// SetupSuiteBase derives it as filepath.Join(Root, "integrations", Name).
	Dir string

	// Name is the integration directory basename, e.g. "cluster-router-eg".
	// Required before calling SetupSuiteBase.
	Name string

	// Cluster is the k3d cluster name, e.g. "transit-cluster-router-eg".
	// Required before calling SetupSuiteBase.
	Cluster string

	// SystemNamespace is the namespace for the Envoy Gateway controller.
	// Defaults to "envoy-gateway-system" when empty.
	SystemNamespace string

	// DataplaneNamespace is the namespace for Gateway and Envoy data-plane
	// resources. Defaults to "default" when empty.
	DataplaneNamespace string

	// Timeout caps the suite context. Defaults to 10 minutes.
	Timeout time.Duration

	EGVersion string
	K3STag    string
	K3DAgents string
	Reset     bool
}

// InstallSmokeTest is a minimal suite that only runs SetupSuiteBase +
// VerifyEnvoyGatewayInstall. Embed envoyGatewaySuite, set RunEnvKey and
// InstallCluster, then call RunInstallSmokeTest from the test function.
//
// Each integration's eg_install_test.go uses this instead of duplicating
// the boilerplate.
type InstallSmokeTest struct {
	// RunEnvKey is the environment variable that gates the test, e.g.
	// "RUN_CLUSTER_ROUTER_EG_INSTALL".
	RunEnvKey string

	// InstallCluster is the k3d cluster name used for this smoke test, e.g.
	// "transit-cr-eg-install". Should be distinct from the full e2e cluster.
	InstallCluster string

	// ConfigAssertions are extra strings to require in the EG configmap, e.g.
	// "GatewayNamespace", "XDSNameSchemeV2".
	ConfigAssertions []string
}

// SetupSuiteBase initialises the suite context, resolves Root and Dir, reads
// environment overrides, creates the k3d cluster, and installs Envoy Gateway.
// Call this from your integration's SetupSuite after setting Name and Cluster.
func (s *Suite) SetupSuiteBase(installEG func()) {
	timeout := s.Timeout
	if timeout == 0 {
		timeout = 10 * time.Minute
	}
	s.Ctx, s.Cancel = context.WithTimeout(context.Background(), timeout)
	s.Root = RepoRoot(s.T())
	s.Dir = filepath.Join(s.Root, "integrations", s.Name)

	if s.SystemNamespace == "" {
		s.SystemNamespace = "envoy-gateway-system"
	}
	if s.DataplaneNamespace == "" {
		s.DataplaneNamespace = "default"
	}

	s.EGVersion = EnvOr("ENVOY_GATEWAY_VERSION", "v1.8.0")
	s.K3STag = EnvOr("K3S_TAG", "v1.31.6-k3s1")
	s.K3DAgents = EnvOr("K3D_AGENTS", "0")
	s.Reset = EnvOr("RESET_CLUSTER", "1") != "0"

	require.NotEmpty(s.T(), s.Cluster)
	require.NoError(s.T(), os.Setenv("KUBECTL_CONTEXT", "k3d-"+s.Cluster))

	s.CreateK3d()
	installEG()
}

// TearDownSuite cancels the suite context and deletes the k3d cluster unless
// KEEP_CLUSTER=1.
func (s *Suite) TearDownSuite() {
	if s.Cancel != nil {
		defer s.Cancel()
	}
	defer func() { _ = os.Unsetenv("KUBECTL_CONTEXT") }()
	if os.Getenv("KEEP_CLUSTER") != "1" {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), time.Minute)
		defer cleanupCancel()
		DeleteK3dCluster(cleanupCtx, s.T(), s.Cluster)
	}
}

// CreateK3d creates the k3d cluster, or reuses an existing one when
// RESET_CLUSTER=0. Called automatically by SetupSuiteBase.
func (s *Suite) CreateK3d() {
	if s.Reset {
		LiveLogf(s.T(), "resetting k3d cluster %s before test start; KEEP_CLUSTER only affects teardown", s.Cluster)
		DeleteK3dCluster(s.Ctx, s.T(), s.Cluster)
	} else {
		LiveLogf(s.T(), "RESET_CLUSTER=0: checking for reusable k3d cluster %s", s.Cluster)
		if K3dClusterExists(s.Ctx, s.Cluster) {
			LiveLogf(s.T(), "reusing existing k3d cluster %s", s.Cluster)
			Run(s.Ctx, s.T(), "", "kubectl", "wait", "--for=condition=Ready", "nodes/k3d-"+s.Cluster+"-server-0", "--timeout=60s")
			return
		}
		LiveLogf(s.T(), "k3d cluster %s does not exist; creating it", s.Cluster)
	}
	LiveLogf(s.T(), "creating single-node k3d cluster %s", s.Cluster)
	Run(s.Ctx, s.T(), "", "k3d", "cluster", "create", s.Cluster,
		"--agents", s.K3DAgents,
		"--image", "rancher/k3s:"+s.K3STag,
		"--k3s-arg", "--disable=traefik@server:*",
		"--k3s-arg", "--kubelet-arg=allowed-unsafe-sysctls=net.ipv4.ip_unprivileged_port_start@server:*")
	Run(s.Ctx, s.T(), "", "kubectl", "wait", "--for=condition=Ready", "nodes/k3d-"+s.Cluster+"-server-0", "--timeout=60s")
}

// InstallEnvoyGateway installs Envoy Gateway via Helm into SystemNamespace,
// applies the integration's k8s/envoy-gateway-config.yaml, and waits for the
// deployment to be ready. Suitable for integrations that do not use Gateway
// Namespace mode.
func (s *Suite) InstallEnvoyGateway() {
	LiveLogf(s.T(), "installing Envoy Gateway %s", s.EGVersion)
	Run(s.Ctx, s.T(), "", "helm", "upgrade", "--install", "eg", "oci://docker.io/envoyproxy/gateway-helm",
		"--version", s.EGVersion,
		"-n", s.SystemNamespace,
		"--create-namespace")
	s.WaitEnvoyGateway()

	LiveLogf(s.T(), "enabling EnvoyPatchPolicy")
	Apply(s.Ctx, s.T(), filepath.Join(s.Dir, "k8s", "envoy-gateway-config.yaml"))
	Run(s.Ctx, s.T(), "", "kubectl", "rollout", "restart", "deployment/envoy-gateway", "-n", s.SystemNamespace)
	s.WaitEnvoyGateway()
}

// WaitEnvoyGateway waits for the envoy-gateway deployment to be rolled out and
// available in SystemNamespace.
func (s *Suite) WaitEnvoyGateway() {
	Run(s.Ctx, s.T(), "", "kubectl", "rollout", "status", "deployment/envoy-gateway", "-n", s.SystemNamespace, "--timeout=120s")
	Run(s.Ctx, s.T(), "", "kubectl", "wait", "deployment/envoy-gateway", "-n", s.SystemNamespace, "--for=condition=Available", "--timeout=120s")
}

// VerifyEnvoyGatewayInstall checks that Gateway API CRDs and the Envoy Gateway
// configmap are present. Pass assertions for the expected configmap contents,
// e.g. "GatewayNamespace" or "XDSNameSchemeV2".
func (s *Suite) VerifyEnvoyGatewayInstall(configAssertions ...string) {
	Run(s.Ctx, s.T(), "", "kubectl", "get", "crd", "gatewayclasses.gateway.networking.k8s.io")
	Run(s.Ctx, s.T(), "", "kubectl", "get", "crd", "gateways.gateway.networking.k8s.io")
	Run(s.Ctx, s.T(), "", "kubectl", "get", "crd", "httproutes.gateway.networking.k8s.io")
	Run(s.Ctx, s.T(), "", "kubectl", "get", "crd", "envoyproxies.gateway.envoyproxy.io")
	Run(s.Ctx, s.T(), "", "kubectl", "get", "crd", "envoypatchpolicies.gateway.envoyproxy.io")

	config := Output(s.Ctx, s.T(), "", "kubectl", "get", "configmap", "envoy-gateway-config",
		"-n", s.SystemNamespace, "-o", "yaml")
	require.Contains(s.T(), config, "enableEnvoyPatchPolicy: true")
	for _, assertion := range configAssertions {
		require.Contains(s.T(), config, assertion)
	}
}

// PortForward starts a kubectl port-forward to target in DataplaneNamespace,
// waits for the local port to be ready, and returns the base URL and a stop
// function.
func (s *Suite) PortForward(target string, remotePort int) (string, func()) {
	s.T().Helper()
	return PortForwardIn(s.Ctx, s.T(), s.DataplaneNamespace, target, remotePort)
}

// PortForwardIn starts a kubectl port-forward to target in the given namespace,
// waits for the local port to be ready, and returns the base URL and a stop
// function.
func PortForwardIn(ctx context.Context, t *testing.T, namespace, target string, remotePort int) (string, func()) {
	t.Helper()
	port := FreePort(t)
	pctx, cancel := context.WithCancel(ctx)
	args := append([]string{"--context", KubeContext(t), "-n", namespace, "port-forward", target},
		fmt.Sprintf("%d:%d", port, remotePort))
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
	require.Failf(t, "port-forward not ready", "port-forward %s/%s did not become ready: %s",
		namespace, target, stderr.String())
	return "", func() {}
}

// ImportImages imports one or more Docker images into the k3d cluster unless
// K3D_SKIP_IMAGE_IMPORT=1, in which case the cluster is expected to pull them
// from a remote registry. K3D_IMAGE_IMPORT_MODE overrides the import mode.
func (s *Suite) ImportImages(images ...string) {
	s.T().Helper()
	if os.Getenv("K3D_SKIP_IMAGE_IMPORT") == "1" {
		LiveLogf(s.T(), "skipping k3d image import; cluster will pull %s", strings.Join(images, ", "))
		return
	}
	LiveLogf(s.T(), "importing images into k3d cluster %s", s.Cluster)
	args := []string{"image", "import", "-c", s.Cluster}
	if mode := os.Getenv("K3D_IMAGE_IMPORT_MODE"); mode != "" {
		args = append(args, "--mode", mode)
	}
	args = append(args, images...)
	Run(s.Ctx, s.T(), "", "k3d", args...)
}

// envoyGatewayInstallSuite is the minimal suite used by eg_install_test.go in
// each integration. It embeds the integration's envoyGatewaySuite and calls
// verifyEnvoyGatewayInstall (defined as a method on that type) as its only test.
//
// Integrations do not embed this directly — they define a thin local type that
// composes envoyGatewaySuite and delegates to this method. See
// RunEnvoyGatewayInstallSmokeTest for the entry point.

// RunEnvoyGatewayInstallSmokeTest is the entry point for eg_install_test.go
// files. It skips unless runEnvKey is set, then runs suite as a testify suite.
// suite must be a *envoyGatewayInstallSuite (local to each integration) that
// composes envoyGatewaySuite and implements TestEnvoyGatewayInstallOnly.
func RunEnvoyGatewayInstallSmokeTest(t *testing.T, runEnvKey string, s interface {
	suite.TestingSuite
}) {
	t.Helper()
	if os.Getenv(runEnvKey) != "1" {
		t.Skipf("set %s=1 to run the Envoy Gateway install smoke test", runEnvKey)
	}
	suite.Run(t, s)
}

// ── Free functions ────────────────────────────────────────────────────────────

// Run executes a command and fails the test on error. kubectl and helm
// automatically receive the k3d context from KubeContext.
func Run(ctx context.Context, t *testing.T, stdin, name string, args ...string) {
	t.Helper()
	_ = Output(ctx, t, stdin, name, args...)
}

// Output executes a command, logs it, and returns trimmed combined output.
// Fails the test on error.
func Output(ctx context.Context, t *testing.T, stdin, name string, args ...string) string {
	t.Helper()
	args = ScopedCommandArgs(t, name, args)
	LiveLogf(t, "$ %s %s%s", name, strings.Join(args, " "), stdinNote(stdin))
	start := time.Now()
	cmd := exec.CommandContext(ctx, name, args...)
	if stdin != "" {
		cmd.Stdin = strings.NewReader(stdin)
	}
	out, err := cmd.CombinedOutput()
	require.NoErrorf(t, err, "%s %s failed:\n%s", name, strings.Join(args, " "), out)
	LiveLogf(t, "ok (%s)", time.Since(start).Round(time.Millisecond))
	return strings.TrimSpace(string(out))
}

// Apply runs kubectl apply -f path.
func Apply(ctx context.Context, t *testing.T, path string) {
	t.Helper()
	Run(ctx, t, "", "kubectl", "apply", "-f", path)
}

// RenderApply renders a Go template file and pipes the result to kubectl apply.
func RenderApply(ctx context.Context, t *testing.T, path string, data any) {
	t.Helper()
	raw, err := os.ReadFile(path)
	require.NoError(t, err)
	tmpl, err := template.New(filepath.Base(path)).Parse(string(raw))
	require.NoError(t, err)
	var out bytes.Buffer
	require.NoError(t, tmpl.Execute(&out, data))
	Run(ctx, t, out.String(), "kubectl", "apply", "-f", "-")
}

// WaitReady waits for pods matching label in namespace to be Ready.
func WaitReady(ctx context.Context, t *testing.T, namespace, label string) {
	t.Helper()
	Run(ctx, t, "", "kubectl", "wait", "pods", "--for=condition=Ready", "-n", namespace, "-l", label, "--timeout=120s")
}

// WaitGatewayProgrammed waits for a Gateway to reach the Programmed condition,
// meaning Envoy Gateway has translated it and provisioned the underlying infra.
func WaitGatewayProgrammed(ctx context.Context, t *testing.T, namespace, name string) {
	t.Helper()
	Run(ctx, t, "", "kubectl", "wait", "gateway/"+name, "-n", namespace,
		"--for=condition=Programmed", "--timeout=120s")
}

// WaitDeployment waits for a named deployment to roll out and be Available.
func WaitDeployment(ctx context.Context, t *testing.T, namespace, name string) {
	t.Helper()
	Run(ctx, t, "", "kubectl", "rollout", "status", "deployment/"+name, "-n", namespace, "--timeout=180s")
	Run(ctx, t, "", "kubectl", "wait", "deployment/"+name, "-n", namespace, "--for=condition=Available", "--timeout=180s")
}

// WaitEnvoyPatchPolicyProgrammed polls the EnvoyPatchPolicy ancestor conditions
// until Programmed=True or 120s elapses.
func WaitEnvoyPatchPolicyProgrammed(ctx context.Context, t *testing.T, namespace, name string) {
	t.Helper()
	deadline := time.Now().Add(120 * time.Second)
	var last string
	for time.Now().Before(deadline) {
		last = Output(ctx, t, "", "kubectl", "get", "envoypatchpolicy", name, "-n", namespace,
			"-o", `jsonpath={range .status.ancestors[*].conditions[*]}{.type}={.status}:{.reason}:{.message}{'\n'}{end}`)
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

// GeneratedResourceName polls for an Envoy Gateway generated resource owned by
// the given Gateway and returns its name.
// resourceNamespace: the namespace where the resource will be created (typically envoy-gateway-system)
// gatewayNamespace: the namespace of the Gateway resource
func GeneratedResourceName(ctx context.Context, t *testing.T, resourceNamespace, gatewayNamespace, gateway, kind string) string {
	t.Helper()
	LiveLogf(t, "waiting for generated Envoy %s for Gateway %s", kind, gateway)
	deadline := time.Now().Add(120 * time.Second)
	var last string
	for time.Now().Before(deadline) {
		last = Output(ctx, t, "", "kubectl", "get", kind, "-n", resourceNamespace,
			"-l", "gateway.envoyproxy.io/owning-gateway-namespace="+gatewayNamespace+",gateway.envoyproxy.io/owning-gateway-name="+gateway,
			"-o", `jsonpath={range .items[*]}{.metadata.name}{'\n'}{end}`)
		if fields := strings.Fields(last); len(fields) > 0 {
			return fields[0]
		}
		select {
		case <-ctx.Done():
			require.NoError(t, ctx.Err())
		case <-time.After(500 * time.Millisecond):
		}
	}
	require.Failf(t, "generated Envoy resource not found", "%s/%s for Gateway %s not found; last output: %q",
		resourceNamespace, kind, gateway, last)
	return ""
}

// Eventually polls check every 200ms until it returns nil or 20s elapses.
func Eventually(ctx context.Context, t *testing.T, check func() error) {
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

// DeleteK3dCluster deletes a k3d cluster, tolerating "already absent" errors.
func DeleteK3dCluster(ctx context.Context, t *testing.T, cluster string) {
	t.Helper()
	LiveLogf(t, "$ k3d cluster delete %s", cluster)
	start := time.Now()
	cmd := exec.CommandContext(ctx, "k3d", "cluster", "delete", cluster)
	out, err := cmd.CombinedOutput()
	if err == nil {
		LiveLogf(t, "ok k3d cluster delete %s (%s)", cluster, time.Since(start).Round(time.Millisecond))
		return
	}
	text := string(out)
	if strings.Contains(text, "No nodes found") || strings.Contains(text, "not found") {
		LiveLogf(t, "ok k3d cluster delete %s: already absent (%s)", cluster, time.Since(start).Round(time.Millisecond))
		return
	}
	require.NoErrorf(t, err, "k3d cluster delete %s failed:\n%s", cluster, out)
}

// K3dClusterExists reports whether a k3d cluster with the given name exists.
func K3dClusterExists(ctx context.Context, cluster string) bool {
	cmd := exec.CommandContext(ctx, "k3d", "cluster", "list", cluster, "--no-headers")
	out, err := cmd.CombinedOutput()
	return err == nil && strings.Contains(string(out), cluster)
}

// ScopedCommandArgs prepends the kube context to kubectl and helm invocations.
func ScopedCommandArgs(t *testing.T, name string, args []string) []string {
	t.Helper()
	switch name {
	case "kubectl":
		return append([]string{"--context", KubeContext(t)}, args...)
	case "helm":
		return append([]string{"--kube-context", KubeContext(t)}, args...)
	default:
		return args
	}
}

// KubeContext returns the KUBECTL_CONTEXT env var, asserting it is set and
// points at a k3d cluster.
func KubeContext(t *testing.T) string {
	t.Helper()
	contextName := os.Getenv("KUBECTL_CONTEXT")
	require.NotEmpty(t, contextName, "KUBECTL_CONTEXT must be set for integration e2e kubectl/helm commands")
	require.Truef(t, strings.HasPrefix(contextName, "k3d-"),
		"refusing to run kubectl/helm against non-k3d context %q", contextName)
	return contextName
}

// FreePort reserves a loopback port long enough for the caller to learn it.
func FreePort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	defer func() { _ = l.Close() }()
	addr, ok := l.Addr().(*net.TCPAddr)
	require.True(t, ok, "FreePort: listener did not return a TCP address")
	return addr.Port
}

// RepoRoot returns the repository root via git rev-parse. The function itself
// uses exec directly (not Output) to avoid the kube context check.
func RepoRoot(t *testing.T) string {
	t.Helper()
	cmd := exec.Command("git", "rev-parse", "--show-toplevel")
	out, err := cmd.Output()
	require.NoError(t, err)
	root := strings.TrimSpace(string(out))
	require.NotEmpty(t, root)
	return root
}

// RuntimeRepoRoot returns the repository root derived from the caller's source
// file path, with no subprocess. Useful when git is unavailable.
func RuntimeRepoRoot() string {
	_, file, _, _ := runtime.Caller(0)
	// integrations/internal/egtest/egtest.go → repo root
	return filepath.Join(filepath.Dir(file), "../../..")
}

// LiveLogf writes a prefixed line to stderr immediately, bypassing t.Log
// buffering so progress is visible during long k3d operations.
func LiveLogf(t *testing.T, format string, args ...any) {
	t.Helper()
	fmt.Fprintf(os.Stderr, "e2e: %s\n", fmt.Sprintf(format, args...))
}

// RequireEnv returns the value of an environment variable, failing the test if
// it is unset or empty.
func RequireEnv(t *testing.T, name string) string {
	t.Helper()
	value := os.Getenv(name)
	require.NotEmptyf(t, value, "%s is required", name)
	return value
}

// EnvOr returns the value of an environment variable, or fallback if unset.
func EnvOr(name, fallback string) string {
	if value := os.Getenv(name); value != "" {
		return value
	}
	return fallback
}

func stdinNote(stdin string) string {
	if stdin == "" {
		return ""
	}
	return fmt.Sprintf(" <stdin:%d bytes>", len(stdin))
}

// ClusterNamesFromConfigDump parses Envoy's /config_dump response body and
// returns all cluster names from dynamic_active_clusters and static_clusters.
func ClusterNamesFromConfigDump(body []byte) []string {
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

// DoRequest executes req and returns the response body, HTTP status code, and
// any transport error.
func DoRequest(req *http.Request) ([]byte, int, error) {
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, 0, err
	}
	return body, resp.StatusCode, nil
}

// DiscoverBackendCluster polls adminURL/config_dump until a cluster whose name
// begins with "httproute/<namespace>/<routeName>/rule/" appears, then returns
// the name. Falls back to a substring match on routeName when the prefix is not
// found. Times out after 60 seconds.
func DiscoverBackendCluster(ctx context.Context, t *testing.T, adminURL, namespace, routeName string) string {
	t.Helper()
	deadline := time.Now().Add(60 * time.Second)
	var names []string
	for time.Now().Before(deadline) {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, adminURL+"/config_dump", nil)
		if err != nil {
			time.Sleep(200 * time.Millisecond)
			continue
		}
		body, status, err := DoRequest(req)
		if err != nil || status != http.StatusOK {
			time.Sleep(200 * time.Millisecond)
			continue
		}
		names = ClusterNamesFromConfigDump(body)
		prefix := "httproute/" + namespace + "/" + routeName + "/rule/"
		for _, name := range names {
			if strings.HasPrefix(name, prefix) {
				return name
			}
		}
		if routeName != "" {
			for _, name := range names {
				if strings.Contains(name, routeName) {
					return name
				}
			}
		}
		select {
		case <-ctx.Done():
			require.NoError(t, ctx.Err())
		case <-time.After(500 * time.Millisecond):
		}
	}
	require.Failf(t, "backend cluster not found",
		"no cluster matching httproute/%s/%s/rule/ found after 60s; clusters: %s",
		namespace, routeName, strings.Join(names, ", "))
	return ""
}
