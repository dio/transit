// Package e2estatictlsmatches proves that metadata-driven static
// transport_socket_matches lets a single dynamic-modules cluster serve both
// plaintext and TLS upstreams without any runtime mutation of transport sockets.
//
// Three hosts are registered in the cluster config:
//
//   - httpbin.org:443  — bucket=tls-system-ca, Hostname=httpbin.org
//   - example.com:443  — bucket=tls-system-ca, Hostname=example.com
//   - 127.0.0.1:<port> — bucket=plaintext
//
// The envoy.yaml has two transport_socket_matches entries that key on the
// "bucket" field in the envoy.transport_socket_match endpoint metadata
// namespace. Envoy selects the TLS socket for the first two hosts and the
// raw_buffer socket for the third — all at connect time, with no mutation.
package e2estatictlsmatches

import (
	"bytes"
	_ "embed"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
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

	// Spin up a local plaintext server so we can prove the raw_buffer socket works.
	plainServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, "plaintext ok")
	}))
	defer plainServer.Close()

	// httptest.NewServer always binds to 127.0.0.1, so extract port directly.
	plainAddr := plainServer.Listener.Addr().String() // "127.0.0.1:<port>"

	proxyPort := e2etest.FreePort()
	adminPort := e2etest.FreePort()
	proxyURL = fmt.Sprintf("http://127.0.0.1:%d", proxyPort)

	// Build the cluster config JSON embedding the three hosts.
	type hostEntry struct {
		Name    string `json:"name"`
		Address string `json:"address"`
		SNI     string `json:"sni,omitempty"`
		Bucket  string `json:"bucket,omitempty"`
	}
	type clusterCfg struct {
		Hosts []hostEntry `json:"hosts"`
	}
	cfg := clusterCfg{
		Hosts: []hostEntry{
			{Name: "httpbin", Address: "httpbin.org:443", SNI: "httpbin.org", Bucket: "tls-system-ca"},
			{Name: "example", Address: "example.com:443", SNI: "example.com", Bucket: "tls-system-ca"},
			{Name: "plain", Address: plainAddr, Bucket: "plaintext"},
		},
	}
	cfgBytes, err := json.Marshal(cfg)
	if err != nil {
		fmt.Fprintf(os.Stderr, "e2e: marshal cluster config: %v\n", err)
		os.Exit(1)
	}

	cfgPath := e2etest.WriteEnvoyConfig("cluster-async-router-static-tls-matches", envoyConfigTmpl, map[string]any{
		"ProxyPort":     proxyPort,
		"AdminPort":     adminPort,
		"SystemCAFile":  systemCA,
		"ClusterConfig": string(cfgBytes),
	})

	exampleDir := filepath.Join(examplesRoot, "cluster-async-router")
	stop, ok := e2etest.StartEnvoy(bin, cfgPath, exampleDir, adminPort, nil)
	if !ok {
		os.Exit(1)
	}
	fmt.Fprintln(os.Stderr, "e2e: envoy ready (static-tls-matches)")

	code := m.Run()
	stop()
	plainServer.Close()
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

// TestTLSMatches_Httpbin routes to httpbin.org:443 via the tls-system-ca match.
// A non-5xx response proves TLS connected; httpbin returns 405 for POST /get
// and 200 for GET — either confirms a successful handshake.
func TestTLSMatches_Httpbin(t *testing.T) {
	resp := post(t, `{"target":"httpbin"}`)
	defer resp.Body.Close()
	t.Logf("status=%d", resp.StatusCode)
	require.Less(t, resp.StatusCode, 500,
		"TLS handshake to httpbin.org failed; transport_socket_matches did not select tls-system-ca socket")
}

// TestTLSMatches_Example routes to example.com:443 via the tls-system-ca match.
func TestTLSMatches_Example(t *testing.T) {
	resp := post(t, `{"target":"example"}`)
	defer resp.Body.Close()
	t.Logf("status=%d", resp.StatusCode)
	require.Less(t, resp.StatusCode, 500,
		"TLS handshake to example.com failed; transport_socket_matches did not select tls-system-ca socket")
}

// TestTLSMatches_Plaintext routes to the local httptest server via the raw_buffer match.
// 200 proves Envoy used the plaintext socket for the bucket=plaintext host.
func TestTLSMatches_Plaintext(t *testing.T) {
	resp := post(t, `{"target":"plain"}`)
	defer resp.Body.Close()
	t.Logf("status=%d", resp.StatusCode)
	require.Equal(t, http.StatusOK, resp.StatusCode,
		"plaintext host did not return 200; transport_socket_matches may have applied TLS to a raw HTTP server")
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
