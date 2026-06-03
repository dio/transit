// Package e2estatictls is a falsification experiment for the proposal in
// docs/auto-host-sni-verdict.md: replace transport_socket_matches with a single
// static UpstreamTlsContext using auto_host_sni + auto_sni_san_validation.
//
// The hypothesis is that Envoy will derive the SNI from the selected host's
// hostname() at connect time. For dynamic-modules clusters, the host pointer is
// minted by envoy_dynamic_module_callback_cluster_add_hosts, whose ABI takes
// only ip:port addresses (and weights, locality, and metadata) — no hostname.
// If the host's hostname() is empty, auto_host_sni cannot produce an SNI and
// the upstream TLS handshake to a real public endpoint must fail.
//
// This suite limits itself to TLS upstreams (httpbin.org, example.com) and
// uses the system CA bundle. Plain HTTP targets are intentionally excluded:
// the static UpstreamTlsContext applies to the whole cluster, so plain traffic
// cannot share it.
package e2estatictls

import (
	"bytes"
	_ "embed"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/dio/transit/examples/internal/e2etest"
)

//go:embed testdata/envoy.tmpl.yaml
var envoyConfigTmpl string

var proxyURL string

func TestMain(m *testing.M) {
	_, file, _, _ := runtime.Caller(0)
	examplesRoot := filepath.Join(filepath.Dir(file), "../../")

	bin := e2etest.EnvoyBin(examplesRoot)
	if _, err := os.Stat(bin); err != nil {
		fmt.Fprintf(os.Stderr, "SKIP: envoy not found at %s (run: make download-envoy)\n", bin)
		os.Exit(0)
	}
	if err := e2etest.CheckSharedLibrary(examplesRoot, "cluster-async-router", "libcluster-async-router.so"); err != nil {
		fmt.Fprintf(os.Stderr, "e2e: %v\n", err)
		os.Exit(1)
	}

	systemCA := findSystemCA()
	if systemCA == "" {
		fmt.Fprintln(os.Stderr, "SKIP: no system CA bundle found")
		os.Exit(0)
	}

	proxyPort := e2etest.FreePort()
	adminPort := e2etest.FreePort()
	proxyURL = fmt.Sprintf("http://127.0.0.1:%d", proxyPort)

	cfgPath := e2etest.WriteEnvoyConfig("cluster-async-router-static-tls", envoyConfigTmpl, map[string]any{
		"ProxyPort":    proxyPort,
		"AdminPort":    adminPort,
		"SystemCAFile": systemCA,
	})

	exampleDir := filepath.Join(examplesRoot, "cluster-async-router")
	stop, ok := e2etest.StartEnvoy(bin, cfgPath, exampleDir, adminPort, nil)
	if !ok {
		os.Exit(1)
	}
	fmt.Fprintln(os.Stderr, "e2e: envoy ready (static-tls)")

	code := m.Run()
	stop()
	os.Exit(code)
}

var testClient = &http.Client{Timeout: 10 * time.Second}

func post(t *testing.T, body string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, proxyURL+"/", bytes.NewBufferString(body))
	require.NoError(t, err)
	req.Header.Set("content-type", "application/json")
	resp, err := testClient.Do(req)
	require.NoError(t, err)
	return resp
}

// A non-503 (specifically < 500) response means Envoy completed the TLS
// handshake to the public endpoint. 503 with NR/upstream_connection_failure is
// what we expect if auto_host_sni cannot read a hostname off the dynamic-module
// host and ends up sending an empty SNI (which will fail SAN validation).
// rounds is how many full passes through the upstream set the interleaved
// test performs. Interleaving across targets stresses connection-pool reuse
// and surfaces SNI caching bugs that a single-target loop would miss.
const rounds = 3

// TestStaticTLS_Interleaved hits httpbin → example → cloudflare repeatedly,
// rotating the target each iteration. If auto_host_sni leaks one host's
// hostname as SNI to other hosts in the cluster, at least one target will
// fail with a SAN matcher mismatch on at least one attempt.
func TestStaticTLS_Interleaved(t *testing.T) {
	targets := []string{"httpbin", "example", "cloudflare"}
	if hosts := os.Getenv("TEST_HOSTS"); hosts != "" {
		targets = strings.Split(hosts, ",")
	}
	for r := 0; r < rounds; r++ {
		for _, target := range targets {
			resp := post(t, `{"target":"`+target+`"}`)
			t.Logf("round=%d target=%s status=%d", r+1, target, resp.StatusCode)
			resp.Body.Close()
			require.Less(t, resp.StatusCode, 500, "round=%d target=%s TLS handshake failed", r+1, target)
		}
	}
}

func findSystemCA() string {
	for _, p := range []string{
		"/etc/ssl/cert.pem",
		"/etc/ssl/certs/ca-certificates.crt",
		"/etc/pki/tls/certs/ca-bundle.crt",
	} {
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	return ""
}
