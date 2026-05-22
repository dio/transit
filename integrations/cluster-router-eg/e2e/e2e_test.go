package e2e

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
	"strings"
	"testing"
	"text/template"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/stretchr/testify/suite"
)

const gatewayHost = "cluster-router.example.com"

func TestClusterRouterEnvoyGateway(t *testing.T) {
	if os.Getenv("RUN_CLUSTER_ROUTER_EG_E2E") != "1" {
		t.Skip("set RUN_CLUSTER_ROUTER_EG_E2E=1 to run the k3d Envoy Gateway integration")
	}
	suite.Run(t, &clusterRouterSuite{
		envoyGatewaySuite: envoyGatewaySuite{
			cluster: "transit-cluster-router-eg",
			timeout: 14 * time.Minute,
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
	liveLogf(s.T(), "importing images into k3d cluster %s", s.cluster)
	args := []string{"image", "import", "-c", s.cluster}
	if mode := os.Getenv("K3D_IMAGE_IMPORT_MODE"); mode != "" {
		args = append(args, "--mode", mode)
	}
	args = append(args, s.envoyImage, s.controlImage)
	run(s.ctx, s.T(), "", "k3d", args...)
}

func (s *clusterRouterSuite) TestClusterRouterEnvoyGateway() {
	liveLogf(s.T(), "verifying Envoy Gateway install")
	s.verifyEnvoyGatewayInstall()

	liveLogf(s.T(), "applying EnvoyProxy and demo workloads")
	renderApply(s.ctx, s.T(), filepath.Join(s.dir, "k8s", "envoyproxy.tmpl.yaml"), map[string]string{
		"EnvoyImage": s.envoyImage,
	})
	renderApply(s.ctx, s.T(), filepath.Join(s.dir, "k8s", "demo.tmpl.yaml"), map[string]string{
		"ControlPlaneImage": s.controlImage,
	})
	apply(s.ctx, s.T(), filepath.Join(s.dir, "k8s", "gateway.yaml"))
	apply(s.ctx, s.T(), filepath.Join(s.dir, "k8s", "httproute.yaml"))

	liveLogf(s.T(), "waiting for demo pods and Gateway")
	waitReady(s.ctx, s.T(), "default", "app=cluster-router-control")
	waitReady(s.ctx, s.T(), "default", "app=upstream-a")
	waitReady(s.ctx, s.T(), "default", "app=upstream-b")
	waitReady(s.ctx, s.T(), "default", "app=upstream-c")
	run(s.ctx, s.T(), "", "kubectl", "wait", "gateway/cluster-router", "--for=condition=Accepted", "--timeout=120s")

	liveLogf(s.T(), "waiting for generated Envoy deployment")
	envoyDeploy := envoyDeployment(s.ctx, s.T())
	waitDeployment(s.ctx, s.T(), "envoy-gateway-system", envoyDeploy)
	liveLogf(s.T(), "opening Envoy admin port-forward")
	adminURL, stopAdmin := portForward(s.ctx, s.T(), "envoy-gateway-system", "deploy/"+envoyDeploy, 19000)
	defer stopAdmin()
	clusterName := discoverBackendCluster(s.ctx, s.T(), adminURL)
	liveLogf(s.T(), "patching generated cluster %q", clusterName)
	renderApply(s.ctx, s.T(), filepath.Join(s.dir, "k8s", "epp.tmpl.yaml"), map[string]string{
		"ClusterName": clusterName,
	})
	waitEnvoyPatchPolicyProgrammed(s.ctx, s.T(), "cluster-router")

	liveLogf(s.T(), "opening Gateway and control-plane port-forwards")
	gatewayURL, stopGateway := portForward(s.ctx, s.T(), "envoy-gateway-system", "service/"+envoyService(s.ctx, s.T()), 80)
	defer stopGateway()
	controlURL, stopControl := portForward(s.ctx, s.T(), "default", "service/cluster-router-control", 8080)
	defer stopControl()

	liveLogf(s.T(), "asserting bootstrap routes")
	assertRoute(s.ctx, s.T(), gatewayURL, "gpt-fast", upstreamResponse{
		Upstream: "upstream-a",
		Auth:     "Bearer openai-token",
		Provider: "openai",
		Version:  "bootstrap",
	})
	assertRoute(s.ctx, s.T(), gatewayURL, "claude-safe", upstreamResponse{
		Upstream: "upstream-b",
		Auth:     "Bearer anthropic-token",
		Provider: "anthropic",
		Version:  "bootstrap",
	})

	liveLogf(s.T(), "posting updated model routes")
	postModel(s.ctx, s.T(), controlURL, modelUpdate{
		Name:       "gpt-slow",
		Target:     "upstream-a.default.svc.cluster.local:8080",
		Provider:   "openai",
		AuthHeader: "Bearer slow-token",
		Version:    "updated",
	})
	postModel(s.ctx, s.T(), controlURL, modelUpdate{
		Name:       "kimi-fast",
		Target:     "upstream-c.default.svc.cluster.local:8080",
		Provider:   "moonshot",
		AuthHeader: "Bearer moonshot-token",
		Version:    "updated",
	})

	liveLogf(s.T(), "waiting for gpt-slow to use updated config")
	eventually(s.ctx, s.T(), func() error {
		return checkRoute(s.ctx, gatewayURL, "gpt-slow", upstreamResponse{
			Upstream: "upstream-a",
			Auth:     "Bearer slow-token",
			Provider: "openai",
			Version:  "updated",
		})
	})
	liveLogf(s.T(), "waiting for kimi-fast to use updated config")
	eventually(s.ctx, s.T(), func() error {
		return checkRoute(s.ctx, gatewayURL, "kimi-fast", upstreamResponse{
			Upstream: "upstream-c",
			Auth:     "Bearer moonshot-token",
			Provider: "moonshot",
			Version:  "updated",
		})
	})

	liveLogf(s.T(), "checking redacted control-plane dump")
	dumpBody := get(s.ctx, s.T(), controlURL+"/dump", nil)
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
	raw, status, err := do(req)
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
	body, status, err := do(req)
	require.NoError(t, err)
	require.Equalf(t, http.StatusOK, status, "POST /models body %s", body)
}

func discoverBackendCluster(ctx context.Context, t *testing.T, adminURL string) string {
	t.Helper()
	body := get(ctx, t, adminURL+"/config_dump", nil)
	names := clusterNames(body)
	for _, name := range names {
		if strings.HasPrefix(name, "httproute/default/cluster-router/rule/") {
			return name
		}
	}
	for _, name := range names {
		if strings.Contains(name, "cluster-router-backend") {
			return name
		}
	}
	require.Failf(t, "backend cluster not found", "could not find generated backend cluster in config dump; clusters: %s", strings.Join(names, ", "))
	return ""
}

func clusterNames(body []byte) []string {
	var dump struct {
		Configs []json.RawMessage `json:"configs"`
	}
	if err := json.Unmarshal(body, &dump); err != nil {
		return nil
	}
	var names []string
	for _, raw := range dump.Configs {
		var cfg struct {
			Type                  string `json:"@type"`
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
		for _, cluster := range cfg.DynamicActiveClusters {
			if cluster.Cluster.Name != "" {
				names = append(names, cluster.Cluster.Name)
			}
		}
		for _, cluster := range cfg.StaticClusters {
			if cluster.Cluster.Name != "" {
				names = append(names, cluster.Cluster.Name)
			}
		}
	}
	return names
}

func get(ctx context.Context, t *testing.T, url string, headers map[string]string) []byte {
	t.Helper()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	require.NoError(t, err)
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	body, status, err := do(req)
	require.NoError(t, err)
	require.Equalf(t, http.StatusOK, status, "GET %s body %s", url, body)
	return body
}

func do(req *http.Request) ([]byte, int, error) {
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

func waitReady(ctx context.Context, t *testing.T, namespace, label string) {
	t.Helper()
	run(ctx, t, "", "kubectl", "wait", "pods", "--for=condition=Ready", "-n", namespace, "-l", label, "--timeout=120s")
}

func waitDeployment(ctx context.Context, t *testing.T, namespace, name string) {
	t.Helper()
	run(ctx, t, "", "kubectl", "rollout", "status", "deployment/"+name, "-n", namespace, "--timeout=180s")
	run(ctx, t, "", "kubectl", "wait", "deployment/"+name, "-n", namespace, "--for=condition=Available", "--timeout=180s")
}

func waitEnvoyPatchPolicyProgrammed(ctx context.Context, t *testing.T, name string) {
	t.Helper()
	deadline := time.Now().Add(120 * time.Second)
	var last string
	for time.Now().Before(deadline) {
		last = output(ctx, t, "", "kubectl", "get", "envoypatchpolicy", name,
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

func envoyDeployment(ctx context.Context, t *testing.T) string {
	t.Helper()
	return generatedResourceName(ctx, t, "deploy")
}

func envoyService(ctx context.Context, t *testing.T) string {
	t.Helper()
	return generatedResourceName(ctx, t, "svc")
}

func generatedResourceName(ctx context.Context, t *testing.T, kind string) string {
	t.Helper()
	liveLogf(t, "waiting for generated Envoy %s", kind)
	deadline := time.Now().Add(120 * time.Second)
	var last string
	for time.Now().Before(deadline) {
		last = output(ctx, t, "", "kubectl", "get", kind, "-n", "envoy-gateway-system",
			"-l", "gateway.envoyproxy.io/owning-gateway-namespace=default,gateway.envoyproxy.io/owning-gateway-name=cluster-router",
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
	require.Failf(t, "generated Envoy resource not found", "%s not found; last output: %q", kind, last)
	return ""
}

func portForward(ctx context.Context, t *testing.T, namespace, target string, remotePort int) (string, func()) {
	t.Helper()
	port := freePort(t)
	pctx, cancel := context.WithCancel(ctx)
	args := append([]string{"--context", kubeContext(t), "-n", namespace, "port-forward", target}, fmt.Sprintf("%d:%d", port, remotePort))
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
	require.Failf(t, "port-forward not ready", "port-forward %s/%s did not become ready: %s", namespace, target, stderr.String())
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

func liveLogf(t *testing.T, format string, args ...any) {
	t.Helper()
	msg := fmt.Sprintf(format, args...)
	fmt.Fprintf(os.Stderr, "e2e: %s\n", msg)
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
