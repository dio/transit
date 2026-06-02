package e2e

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/stretchr/testify/suite"

	"github.com/dio/transit/integrations/cluster-async-router-eg/internal/demo"
	"github.com/dio/transit/integrations/internal/egtest"
)

const gatewayHost = "cluster-async-router.example.com"

func TestClusterAsyncRouterEnvoyGateway(t *testing.T) {
	if os.Getenv("RUN_CLUSTER_ASYNC_ROUTER_EG_E2E") != "1" {
		t.Skip("set RUN_CLUSTER_ASYNC_ROUTER_EG_E2E=1 to run the k3d Envoy Gateway integration")
	}
	suite.Run(t, &clusterAsyncRouterSuite{
		envoyGatewaySuite: envoyGatewaySuite{
			Suite: egtest.Suite{
				Cluster: "transit-cluster-async-router-eg",
				Timeout: 14 * time.Minute,
			},
		},
	})
}

type clusterAsyncRouterSuite struct {
	envoyGatewaySuite

	envoyImage string
	demoImage  string
	caPEM      []byte // stored by applyTLSSecrets; consumed by runSDSVariant
}

func (s *clusterAsyncRouterSuite) SetupSuite() {
	s.envoyImage = requireEnv(s.T(), "IMAGE")
	s.demoImage = requireEnv(s.T(), "DEMO_IMAGE")

	s.envoyGatewaySuite.SetupSuite()
	s.ImportImages(s.envoyImage, s.demoImage)
}

// applyTLSSecrets generates a self-signed CA + per-host leaf certs and applies
// them as Secrets. The CA Secret is mounted into Envoy (validates upstream
// leaves via /etc/envoy/tls/ca.pem); the leaf Secrets are mounted into the
// upstream-c / upstream-d Pods so their TLS listeners can serve.
// The raw CA PEM is stored on s.caPEM for later use by runSDSVariant.
func (s *clusterAsyncRouterSuite) applyTLSSecrets() {
	caPEM, caKey, err := genCA()
	require.NoError(s.T(), err)
	s.caPEM = caPEM
	hostC, err := genLeaf("host-c.test", caPEM, caKey)
	require.NoError(s.T(), err)
	hostD, err := genLeaf("host-d.test", caPEM, caKey)
	require.NoError(s.T(), err)

	// CA Secret goes in envoy-gateway-system because that's where EG generates
	// the Envoy Deployment; kubelet resolves volume Secrets from the pod's own
	// namespace. The leaf Secrets stay in default — they're mounted by the
	// upstream Pods which live there.
	run(s.Ctx, s.T(), caSecretManifest("cluster-async-router-ca", "envoy-gateway-system", caPEM),
		"kubectl", "apply", "-f", "-")
	run(s.Ctx, s.T(), tlsSecretManifest("tls-host-c", "default", hostC),
		"kubectl", "apply", "-f", "-")
	run(s.Ctx, s.T(), tlsSecretManifest("tls-host-d", "default", hostD),
		"kubectl", "apply", "-f", "-")
}

// applySDSConfigMap creates a ConfigMap in envoy-gateway-system that holds an
// SDS resource JSON file (ca-sds.json). Envoy reads the file via
// path_config_source to supply the trusted CA when combined_validation_context
// is used (delta-e / epp-sds.tmpl.yaml).
func (s *clusterAsyncRouterSuite) applySDSConfigMap(caPEM []byte) {
	s.T().Helper()
	b64CA := base64.StdEncoding.EncodeToString(caPEM)
	sdsJSON := fmt.Sprintf(
		`{"resources":[{"@type":"type.googleapis.com/envoy.extensions.transport_sockets.tls.v3.Secret","name":"cluster-async-router-ca","validation_context":{"trusted_ca":{"inline_bytes":%q}}}]}`,
		b64CA,
	)
	manifest := fmt.Sprintf(`apiVersion: v1
kind: ConfigMap
metadata:
  name: cluster-async-router-sds-ca
  namespace: envoy-gateway-system
data:
  ca-sds.json: |
%s
`, indent(sdsJSON, "    "))
	run(s.Ctx, s.T(), manifest, "kubectl", "apply", "-f", "-")
}

// runSDSVariant runs the SDS-backed CA validation variant (WS-B delta-e).
// It applies envoyproxy-sds.tmpl.yaml (which mounts the SDS ConfigMap),
// waits for the Envoy deployment to roll, re-opens port-forwards, applies
// epp-sds.tmpl.yaml, and asserts a/b/c/d all return 200.
func (s *clusterAsyncRouterSuite) runSDSVariant(clusterName string) {
	s.T().Helper()

	liveLogf(s.T(), "[delta-e] applying SDS ConfigMap")
	s.applySDSConfigMap(s.caPEM)

	liveLogf(s.T(), "[delta-e] applying envoyproxy-sds.tmpl.yaml")
	renderApply(s.Ctx, s.T(), filepath.Join(s.Dir, "k8s", "envoyproxy-sds.tmpl.yaml"), map[string]string{
		"EnvoyImage": s.envoyImage,
	})

	liveLogf(s.T(), "[delta-e] waiting for Envoy deployment rollout")
	envoyDeploy := egtest.GeneratedResourceName(s.Ctx, s.T(), "envoy-gateway-system", "default", "cluster-async-router", "deploy")
	waitDeployment(s.Ctx, s.T(), "envoy-gateway-system", envoyDeploy)

	liveLogf(s.T(), "[delta-e] opening new admin + gateway port-forwards")
	adminURL, stopAdmin := portForward(s.Ctx, s.T(), "envoy-gateway-system", "deploy/"+envoyDeploy, 19000)
	defer stopAdmin()
	envoySvc := egtest.GeneratedResourceName(s.Ctx, s.T(), "envoy-gateway-system", "default", "cluster-async-router", "svc")
	gatewayURL, stopGateway := portForward(s.Ctx, s.T(), "envoy-gateway-system", "service/"+envoySvc, 80)
	defer stopGateway()
	_ = adminURL // available for future config_dump assertions

	liveLogf(s.T(), "[delta-e] applying epp-sds.tmpl.yaml")
	renderApply(s.Ctx, s.T(), filepath.Join(s.Dir, "k8s", "epp-sds.tmpl.yaml"), map[string]string{
		"ClusterName": clusterName,
	})
	waitEnvoyPatchPolicyProgrammed(s.Ctx, s.T(), "cluster-async-router")

	liveLogf(s.T(), "[delta-e] asserting a/b/c/d all 200 via SDS CA")
	assertTarget(s.Ctx, s.T(), gatewayURL, "a", "upstream-a")
	assertTarget(s.Ctx, s.T(), gatewayURL, "b", "upstream-b")
	assertTarget(s.Ctx, s.T(), gatewayURL, "c", "upstream-c")
	assertTarget(s.Ctx, s.T(), gatewayURL, "d", "upstream-d")
}

func (s *clusterAsyncRouterSuite) TestClusterAsyncRouterEnvoyGateway() {
	liveLogf(s.T(), "verifying Envoy Gateway install")
	s.verifyEnvoyGatewayInstall()

	liveLogf(s.T(), "generating CA + per-host TLS Secrets")
	// Secrets must exist before EnvoyProxy (Envoy mounts cluster-async-router-ca)
	// and before the TLS upstream Pods (they mount tls-host-{c,d}).
	s.applyTLSSecrets()

	liveLogf(s.T(), "applying EnvoyProxy and demo workloads")
	renderApply(s.Ctx, s.T(), filepath.Join(s.Dir, "k8s", "envoyproxy.tmpl.yaml"), map[string]string{
		"EnvoyImage": s.envoyImage,
	})
	renderApply(s.Ctx, s.T(), filepath.Join(s.Dir, "k8s", "demo.tmpl.yaml"), map[string]string{
		"DemoImage": s.demoImage,
	})
	apply(s.Ctx, s.T(), filepath.Join(s.Dir, "k8s", "gateway.yaml"))
	apply(s.Ctx, s.T(), filepath.Join(s.Dir, "k8s", "httproute.yaml"))

	liveLogf(s.T(), "waiting for upstream pods and Gateway")
	waitReady(s.Ctx, s.T(), "app=upstream-a")
	waitReady(s.Ctx, s.T(), "app=upstream-b")
	waitReady(s.Ctx, s.T(), "app=upstream-c")
	waitReady(s.Ctx, s.T(), "app=upstream-d")
	run(s.Ctx, s.T(), "", "kubectl", "wait", "gateway/cluster-async-router", "--for=condition=Accepted", "--timeout=120s")
	egtest.WaitGatewayProgrammed(s.Ctx, s.T(), "default", "cluster-async-router")

	liveLogf(s.T(), "waiting for generated Envoy deployment")
	envoyDeploy := egtest.GeneratedResourceName(s.Ctx, s.T(), "envoy-gateway-system", "default", "cluster-async-router", "deploy")
	waitDeployment(s.Ctx, s.T(), "envoy-gateway-system", envoyDeploy)

	liveLogf(s.T(), "opening Envoy admin port-forward to discover backend cluster")
	adminURL, stopAdmin := portForward(s.Ctx, s.T(), "envoy-gateway-system", "deploy/"+envoyDeploy, 19000)
	defer stopAdmin()
	clusterName := egtest.DiscoverBackendCluster(s.Ctx, s.T(), adminURL, "default", "cluster-async-router")
	liveLogf(s.T(), "patching generated cluster %q", clusterName)
	renderApply(s.Ctx, s.T(), filepath.Join(s.Dir, "k8s", "epp.tmpl.yaml"), map[string]string{
		"ClusterName": clusterName,
	})
	waitEnvoyPatchPolicyProgrammed(s.Ctx, s.T(), "cluster-async-router")

	liveLogf(s.T(), "asserting patched cluster shape via Envoy admin /config_dump")
	assertPatchedClusterShape(s.Ctx, s.T(), adminURL, clusterName)

	liveLogf(s.T(), "opening Gateway port-forward")
	envoySvc := egtest.GeneratedResourceName(s.Ctx, s.T(), "envoy-gateway-system", "default", "cluster-async-router", "svc")
	gatewayURL, stopGateway := portForward(s.Ctx, s.T(), "envoy-gateway-system", "service/"+envoySvc, 80)
	defer stopGateway()

	liveLogf(s.T(), "asserting body-driven routing reaches each upstream")
	// {"target":"a"} → upstream-a; {"target":"b"} → upstream-b. Body-driven
	// host selection is what this integration proves, so both targets are
	// asserted independently.
	assertTarget(s.Ctx, s.T(), gatewayURL, "a", "upstream-a")
	assertTarget(s.Ctx, s.T(), gatewayURL, "b", "upstream-b")
	// target=c/d are the showcase: HostSpec.Metadata writes sni into the
	// envoy.transport_socket_match namespace, the patched cluster's
	// transport_socket_matches picks the matching UpstreamTlsContext, and the
	// upstream's GetCertificate tripwire rejects any handshake whose SNI
	// doesn't match host-{c,d}.test. A 200 here means every link held.
	assertTarget(s.Ctx, s.T(), gatewayURL, "c", "upstream-c")
	assertTarget(s.Ctx, s.T(), gatewayURL, "d", "upstream-d")

	liveLogf(s.T(), "asserting negative paths fail closed")
	requireNon2xx(s.Ctx, s.T(), gatewayURL, []byte(`{"target":"nope"}`))
	requireNon2xx(s.Ctx, s.T(), gatewayURL, []byte(`{}`))

	// [WS-B delta-b] auto_host_sni: switch EPP to derive SNI from :authority.
	// With :authority="cluster-async-router.example.com" (gateway hostname),
	// SNI will be the gateway hostname, not host-c.test / host-d.test.
	// Expected: a/b still 200 (plaintext); c/d non-2xx (SNI mismatch —
	// proves auto_sni reads :authority, not host metadata).
	liveLogf(s.T(), "[delta-b] applying epp-auto-host-sni.tmpl.yaml")
	renderApply(s.Ctx, s.T(), filepath.Join(s.Dir, "k8s", "epp-auto-host-sni.tmpl.yaml"), map[string]string{
		"ClusterName": clusterName,
	})
	waitEnvoyPatchPolicyProgrammed(s.Ctx, s.T(), "cluster-async-router")
	liveLogf(s.T(), "[delta-b] asserting a/b → 200, c/d → non-2xx (auto_sni reads :authority)")
	assertTarget(s.Ctx, s.T(), gatewayURL, "a", "upstream-a")
	assertTarget(s.Ctx, s.T(), gatewayURL, "b", "upstream-b")
	requireNon2xx(s.Ctx, s.T(), gatewayURL, []byte(`{"target":"c"}`))
	requireNon2xx(s.Ctx, s.T(), gatewayURL, []byte(`{"target":"d"}`))

	// [WS-B delta-c] timing trap test: Lua sets :authority from body phase,
	// auto_sni reads it for SNI.
	// Research question: does auto_sni evaluate :authority before or after
	// the body phase?
	//   If AFTER (cluster-level, at connection time): c/d → 200 (no trap).
	//   If BEFORE (router-level, at headers phase): c/d → non-2xx (trap confirmed).
	// Observation is logged; the test does NOT assert a specific outcome for c/d.
	// Finding feeds the WS-B verdict.
	liveLogf(s.T(), "[delta-c] applying epp-authority-rewrite.tmpl.yaml")
	renderApply(s.Ctx, s.T(), filepath.Join(s.Dir, "k8s", "epp-authority-rewrite.tmpl.yaml"), map[string]string{
		"ClusterName": clusterName,
	})
	waitEnvoyPatchPolicyProgrammed(s.Ctx, s.T(), "cluster-async-router")
	liveLogf(s.T(), "[delta-c] asserting a/b → 200")
	assertTarget(s.Ctx, s.T(), gatewayURL, "a", "upstream-a")
	assertTarget(s.Ctx, s.T(), gatewayURL, "b", "upstream-b")
	liveLogf(s.T(), "[delta-c] probing c and d (outcome logged, not asserted)")
	statusC := probeTarget(s.Ctx, s.T(), gatewayURL, "c")
	statusD := probeTarget(s.Ctx, s.T(), gatewayURL, "d")
	liveLogf(s.T(), "[delta-c] target=c → %s; target=d → %s (auto_sni timing observation)", statusC, statusD)

	// Restore baseline EPP before the SDS variant so the SDS test starts from
	// a known-good state with explicit sni in UpstreamTlsContext.
	liveLogf(s.T(), "restoring baseline epp.tmpl.yaml before SDS variant")
	renderApply(s.Ctx, s.T(), filepath.Join(s.Dir, "k8s", "epp.tmpl.yaml"), map[string]string{
		"ClusterName": clusterName,
	})
	waitEnvoyPatchPolicyProgrammed(s.Ctx, s.T(), "cluster-async-router")

	if os.Getenv("TLS_VIA_SDS") == "1" {
		liveLogf(s.T(), "[WS-B delta-e] running SDS variant")
		s.runSDSVariant(clusterName)
	}
}

// probeTarget sends a POST {"target":<target>} to gatewayURL and returns "2xx"
// if the response status is in the 2xx range, or the numeric status code string
// otherwise. It does not call t.Fatal on non-2xx responses, making it suitable
// for observation-only probes (e.g. delta-c timing trap research).
func probeTarget(ctx context.Context, t *testing.T, gatewayURL, target string) string {
	t.Helper()
	body, _ := json.Marshal(map[string]string{"target": target})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, gatewayURL+"/", bytes.NewReader(body))
	if err != nil {
		return fmt.Sprintf("err:%v", err)
	}
	req.Host = gatewayHost
	req.Header.Set("content-type", "application/json")
	_, status, err := egtest.DoRequest(req)
	if err != nil {
		return fmt.Sprintf("err:%v", err)
	}
	if status >= 200 && status < 300 {
		return "2xx"
	}
	return fmt.Sprintf("%d", status)
}

// assertPatchedClusterShape pulls the patched cluster out of Envoy's
// /config_dump and verifies the structural pieces that make per-host TLS
// work: cluster type, lb policy, transport_socket_matches keyed on
// host-{c,d}.test, and the 4-host cluster_config with sni metadata on c/d.
// If any of these regress, body-driven routing might still pass but the TLS
// path silently breaks.
func assertPatchedClusterShape(ctx context.Context, t *testing.T, adminURL, clusterName string) {
	t.Helper()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, adminURL+"/config_dump?resource=dynamic_active_clusters", nil)
	require.NoError(t, err)
	raw, status, err := egtest.DoRequest(req)
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, status, "config_dump: %s", raw)

	var dump struct {
		Configs []struct {
			Cluster struct {
				Name        string `json:"name"`
				LbPolicy    string `json:"lb_policy"`
				ClusterType struct {
					Name        string `json:"name"`
					TypedConfig struct {
						ClusterConfig struct {
							Value string `json:"value"`
						} `json:"cluster_config"`
					} `json:"typed_config"`
				} `json:"cluster_type"`
				TransportSocketMatches []struct {
					Name  string            `json:"name"`
					Match map[string]string `json:"match"`
				} `json:"transport_socket_matches"`
			} `json:"cluster"`
		} `json:"configs"`
	}
	require.NoError(t, json.Unmarshal(raw, &dump))

	var found bool
	for _, c := range dump.Configs {
		if c.Cluster.Name != clusterName {
			continue
		}
		found = true
		require.Equal(t, "CLUSTER_PROVIDED", c.Cluster.LbPolicy)
		require.Equal(t, "envoy.clusters.dynamic_modules", c.Cluster.ClusterType.Name)

		matches := map[string]string{}
		for _, m := range c.Cluster.TransportSocketMatches {
			matches[m.Name] = m.Match["sni"]
		}
		require.Equal(t, "host-c.test", matches["host-c"], "transport_socket_matches: %+v", matches)
		require.Equal(t, "host-d.test", matches["host-d"], "transport_socket_matches: %+v", matches)

		var cfg struct {
			Hosts []struct {
				Name string `json:"name"`
				SNI  string `json:"sni"`
			} `json:"hosts"`
		}
		require.NoError(t, json.Unmarshal([]byte(c.Cluster.ClusterType.TypedConfig.ClusterConfig.Value), &cfg))
		hosts := map[string]string{}
		for _, h := range cfg.Hosts {
			hosts[h.Name] = h.SNI
		}
		require.Equal(t, "", hosts["a"], "host a should be plaintext")
		require.Equal(t, "", hosts["b"], "host b should be plaintext")
		require.Equal(t, "host-c.test", hosts["c"])
		require.Equal(t, "host-d.test", hosts["d"])
	}
	require.Truef(t, found, "patched cluster %q not in config_dump", clusterName)
}

func assertTarget(ctx context.Context, t *testing.T, gatewayURL, target, wantUpstream string) {
	t.Helper()
	body, _ := json.Marshal(map[string]string{"target": target})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, gatewayURL+"/", bytes.NewReader(body))
	require.NoError(t, err)
	req.Host = gatewayHost
	req.Header.Set("content-type", "application/json")
	raw, status, err := egtest.DoRequest(req)
	require.NoErrorf(t, err, "POST target=%s", target)
	require.Equalf(t, http.StatusOK, status, "POST target=%s body=%s", target, raw)
	var got demo.UpstreamResponse
	require.NoError(t, json.Unmarshal(raw, &got))
	require.Equalf(t, wantUpstream, got.Upstream, "target %s reached wrong upstream: %+v", target, got)
}

func requireNon2xx(ctx context.Context, t *testing.T, gatewayURL string, body []byte) {
	t.Helper()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, gatewayURL+"/", bytes.NewReader(body))
	require.NoError(t, err)
	req.Host = gatewayHost
	req.Header.Set("content-type", "application/json")
	raw, status, err := egtest.DoRequest(req)
	require.NoError(t, err)
	require.NotEqualf(t, http.StatusOK, status, "expected non-200 for body=%s, got 200 body=%s", body, raw)
}
