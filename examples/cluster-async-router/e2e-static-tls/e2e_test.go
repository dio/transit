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
func TestStaticTLS_Httpbin(t *testing.T) {
	resp := post(t, `{"target":"httpbin"}`)
	defer resp.Body.Close()
	t.Logf("status=%d", resp.StatusCode)
	require.Less(t, resp.StatusCode, 500, "TLS handshake to httpbin.org failed; auto_host_sni produced no SNI (dynamic-module host hostname is empty)")
}

func TestStaticTLS_Example(t *testing.T) {
	resp := post(t, `{"target":"example"}`)
	defer resp.Body.Close()
	t.Logf("status=%d", resp.StatusCode)
	require.Less(t, resp.StatusCode, 500, "TLS handshake to example.com failed; auto_host_sni produced no SNI (dynamic-module host hostname is empty)")
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
