// Package sni_test is an end-to-end test verifying that runtime-added hosts on
// the orange-pick cluster use HostSpec.Hostname as the TLS SNI (auto_host_sni)
// when dialling each binding.
//
// The test starts two TLS stub servers (one per binding), configures orange
// with a single provider that has two bindings pointing to those stubs, and
// confirms that both TLS handshakes complete successfully — proving that
// auto_host_sni delivers the correct SNI without any xDS-sourced host config.
//
// Both bindings resolve to 127.0.0.1 (different ports) via the IP-literal fast
// path added to lookupWithTTL. The SNI for both is "127.0.0.1", which the stubs
// record and the test asserts.
//
// Prerequisites (test skips gracefully when absent):
//   - Custom Envoy binary at ../.bin/envoy or ENVOY_BIN env var
//   - Built liborange.so (run: make -C examples/orange build)
package sni_test

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"math/big"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"text/template"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/dio/transit/examples/internal/e2etest"
)

// sniEnvoyTmpl is the envoy config template for the SNI test. It mirrors the
// main e2e template but uses a test-supplied CA file and omits MCP.
const sniEnvoyTmpl = `static_resources:
  listeners:
    - name: orange
      address:
        socket_address: { address: 127.0.0.1, port_value: {{.ProxyPort}} }
      filter_chains:
        - filters:
            - name: envoy.filters.network.http_connection_manager
              typed_config:
                "@type": type.googleapis.com/envoy.extensions.filters.network.http_connection_manager.v3.HttpConnectionManager
                stat_prefix: orange
                http_filters:
                  - name: orange-match
                    typed_config:
                      "@type": type.googleapis.com/envoy.extensions.filters.http.dynamic_modules.v3.DynamicModuleFilter
                      dynamic_module_config:
                        name: orange
                      filter_name: orange-match
                      filter_config:
                        "@type": type.googleapis.com/google.protobuf.StringValue
                        value: '{}'
                  - name: envoy.filters.http.router
                    typed_config:
                      "@type": type.googleapis.com/envoy.extensions.filters.http.router.v3.Router
                route_config:
                  name: orange
                  virtual_hosts:
                    - name: local
                      domains: ["*"]
                      routes:
                        - match: { prefix: "/" }
                          route: { cluster: orange_default }

  clusters:
    - name: orange_default
      connect_timeout: 10s
      lb_policy: CLUSTER_PROVIDED
      transport_socket:
        name: envoy.transport_sockets.tls
        typed_config:
          "@type": type.googleapis.com/envoy.extensions.transport_sockets.tls.v3.UpstreamTlsContext
          auto_host_sni: true
          common_tls_context:
            validation_context:
              trusted_ca: { filename: {{.CAFile}} }
      typed_extension_protocol_options:
        envoy.extensions.upstreams.http.v3.HttpProtocolOptions:
          "@type": type.googleapis.com/envoy.extensions.upstreams.http.v3.HttpProtocolOptions
          explicit_http_config:
            http_protocol_options: {}
          http_filters:
            - name: orange-adapt
              typed_config:
                "@type": type.googleapis.com/envoy.extensions.filters.http.dynamic_modules.v3.DynamicModuleFilter
                dynamic_module_config:
                  name: orange
                filter_name: orange-adapt
                filter_config:
                  "@type": type.googleapis.com/google.protobuf.StringValue
                  value: '{}'
            - name: envoy.filters.http.upstream_codec
              typed_config:
                "@type": type.googleapis.com/envoy.extensions.filters.http.upstream_codec.v3.UpstreamCodec
      cluster_type:
        name: envoy.clusters.dynamic_modules
        typed_config:
          "@type": type.googleapis.com/envoy.extensions.clusters.dynamic_modules.v3.ClusterConfig
          dynamic_module_config:
            name: orange
          cluster_name: orange-pick
          cluster_config:
            "@type": type.googleapis.com/google.protobuf.StringValue
            value: '{}'

admin:
  address:
    socket_address: { address: 127.0.0.1, port_value: {{.AdminPort}} }
`

// orangeSNICfgTmpl uses IP-literal endpoints (https://127.0.0.1:PORT) so that
// the orange-pick cluster resolves them via the IP-literal fast path in
// lookupWithTTL (no DNS query needed). Both bindings share the same IP;
// the SNI for both connections will be "127.0.0.1" — the hostname extracted
// from the endpoint URL, delivered via auto_host_sni.
const orangeSNICfgTmpl = `llm:
  providers:
    stub:
      kind: openai
      auth:
        type: bearer
        secret_ref: literal://test-key
      bindings:
        - name: east
          endpoint: https://127.0.0.1:{{.EastPort}}
        - name: west
          endpoint: https://127.0.0.1:{{.WestPort}}
  models:
    east-model:
      provider: stub
      binding: east
      name: east-model
    west-model:
      provider: stub
      binding: west
      name: west-model
`

// fakeChatResponse is a minimal valid OpenAI chat completion body.
const fakeChatResponse = `{
  "id": "test",
  "object": "chat.completion",
  "model": "test-model",
  "choices": [{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],
  "usage": {"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}
}`

// TestSNI_twoBindings verifies end-to-end that the orange-pick cluster registers
// two runtime-added hosts for a provider with two bindings and that TLS
// handshakes succeed with the correct SNI (HostSpec.Hostname via auto_host_sni).
// The test skips gracefully when the custom Envoy binary is not present.
func TestSNI_twoBindings(t *testing.T) {
	_, thisFile, _, _ := runtime.Caller(0)
	examplesRoot := filepath.Join(filepath.Dir(thisFile), "../../../")

	bin := e2etest.EnvoyBin(examplesRoot)
	if _, err := os.Stat(bin); err != nil {
		t.Skipf("envoy not found at %s (run: make download-envoy)", bin)
	}
	if err := e2etest.CheckSharedLibrary(examplesRoot, "orange", "liborange.so"); err != nil {
		t.Fatalf("e2e: %v", err)
	}

	// --- TLS material -----------------------------------------------------------

	ca, caKey, caPEM := mustGenCA(t)
	serverCert := mustGenServerCert(t, ca, caKey)

	caFile, err := os.CreateTemp("", "orange-sni-ca-*.pem")
	require.NoError(t, err)
	t.Cleanup(func() { os.Remove(caFile.Name()) })
	_, _ = caFile.Write(caPEM)
	_ = caFile.Close()

	tlsCfg := &tls.Config{Certificates: []tls.Certificate{serverCert}} //nolint:gosec — test-only CA

	// --- Stub TLS servers -------------------------------------------------------

	eastPort, eastStop, eastHits, eastSNIs := startStubTLSServer(t, tlsCfg)
	westPort, westStop, westHits, westSNIs := startStubTLSServer(t, tlsCfg)
	t.Cleanup(func() { eastStop(); westStop() })

	// --- Orange config ----------------------------------------------------------

	orangeCfgBytes := mustRenderTemplate(t, orangeSNICfgTmpl, map[string]any{
		"EastPort": eastPort,
		"WestPort": westPort,
	})

	orangeCfgFile, err := os.CreateTemp("", "orange-sni-*.yaml")
	require.NoError(t, err)
	t.Cleanup(func() { os.Remove(orangeCfgFile.Name()) })
	_, _ = orangeCfgFile.Write(orangeCfgBytes)
	_ = orangeCfgFile.Close()

	// --- Envoy ------------------------------------------------------------------

	proxyPort := e2etest.FreePort()
	adminPort := e2etest.FreePort()

	envoyCfgPath := e2etest.WriteEnvoyConfig("orange-sni", sniEnvoyTmpl, map[string]any{
		"ProxyPort": proxyPort,
		"AdminPort": adminPort,
		"CAFile":    caFile.Name(),
	})

	exampleDir := filepath.Join(examplesRoot, "orange")
	stop, ok := e2etest.StartEnvoy(bin, envoyCfgPath, exampleDir, adminPort, []string{
		"ORANGE_CONFIG=" + orangeCfgFile.Name(),
	})
	if !ok {
		t.Fatal("envoy failed to start")
	}
	t.Cleanup(stop)

	proxyURL := fmt.Sprintf("http://127.0.0.1:%d", proxyPort)
	client := &http.Client{Timeout: 15 * time.Second}

	// --- Requests ---------------------------------------------------------------

	// Each model resolves to a different binding; each binding maps to a
	// different stub port. The same IP (127.0.0.1) is used for both, so SNI
	// is "127.0.0.1" on every connection — demonstrating that auto_host_sni
	// uses HostSpec.Hostname rather than a static xDS SNI override.
	sendChatRequest(t, client, proxyURL, "east-model")
	sendChatRequest(t, client, proxyURL, "west-model")

	// Both stubs must have received exactly one request each.
	require.Equal(t, int64(1), eastHits.Load(), "east binding stub must have received exactly one request")
	require.Equal(t, int64(1), westHits.Load(), "west binding stub must have received exactly one request")

	// Both TLS connections must have carried the expected SNI ("127.0.0.1" — the
	// hostname extracted from the binding's endpoint URL via HostSpec.Hostname).
	eastSNIs.mu.Lock()
	gotEastSNIs := append([]string(nil), eastSNIs.vals...)
	eastSNIs.mu.Unlock()
	westSNIs.mu.Lock()
	gotWestSNIs := append([]string(nil), westSNIs.vals...)
	westSNIs.mu.Unlock()

	require.ElementsMatch(t, []string{"127.0.0.1"}, gotEastSNIs,
		"east stub TLS SNI must be the hostname from the east binding endpoint URL (auto_host_sni)")
	require.ElementsMatch(t, []string{"127.0.0.1"}, gotWestSNIs,
		"west stub TLS SNI must be the hostname from the west binding endpoint URL (auto_host_sni)")
}

// sniTracker records TLS ServerName values seen across connections.
type sniTracker struct {
	mu   sync.Mutex
	vals []string
}

func (s *sniTracker) record(sni string) {
	s.mu.Lock()
	s.vals = append(s.vals, sni)
	s.mu.Unlock()
}

// startStubTLSServer starts a TLS HTTP/1.1 server that returns a fake chat
// completion for any POST request. It returns the port, a stop func, a hit
// counter, and an SNI tracker that records the ServerName of each connection.
func startStubTLSServer(t *testing.T, cfg *tls.Config) (port int, stop func(), hits *atomic.Int64, tracker *sniTracker) {
	t.Helper()
	tracker = &sniTracker{}

	// Wrap GetCertificate to record the SNI from each ClientHello.
	baseCerts := cfg.Certificates
	wrappedCfg := &tls.Config{ //nolint:gosec — test-only CA
		Certificates: baseCerts,
		GetCertificate: func(chi *tls.ClientHelloInfo) (*tls.Certificate, error) {
			tracker.record(chi.ServerName)
			if len(baseCerts) > 0 {
				return &baseCerts[0], nil
			}
			return nil, nil
		},
	}

	ln, err := tls.Listen("tcp", "127.0.0.1:0", wrappedCfg)
	require.NoError(t, err)

	port = ln.Addr().(*net.TCPAddr).Port
	hits = new(atomic.Int64)

	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		w.Header().Set("content-type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(fakeChatResponse))
	})

	srv := &http.Server{Handler: mux} //nolint:gosec — test-only server
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		_ = srv.Serve(ln)
	}()
	stop = func() {
		_ = srv.Close()
		wg.Wait()
	}
	return port, stop, hits, tracker
}

// sendChatRequest sends a minimal chat completion request to the proxy and
// asserts a 200 OK response.
func sendChatRequest(t *testing.T, c *http.Client, proxyURL, model string) {
	t.Helper()
	body, _ := json.Marshal(map[string]any{
		"model":      model,
		"messages":   []map[string]string{{"role": "user", "content": "hi"}},
		"max_tokens": 1,
	})
	req, err := http.NewRequest(http.MethodPost, proxyURL+"/v1/chat/completions", bytes.NewReader(body))
	require.NoError(t, err)
	req.Header.Set("content-type", "application/json")
	resp, err := c.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode, "model=%s", model)
}

// mustGenCA generates an ephemeral ECDSA CA cert + key for test use only.
func mustGenCA(t *testing.T) (*x509.Certificate, *ecdsa.PrivateKey, []byte) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)

	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "orange-sni-test-ca"},
		NotBefore:             time.Now().Add(-time.Minute),
		NotAfter:              time.Now().Add(time.Hour),
		IsCA:                  true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
		BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	require.NoError(t, err)

	cert, err := x509.ParseCertificate(der)
	require.NoError(t, err)

	var pemBuf bytes.Buffer
	require.NoError(t, pem.Encode(&pemBuf, &pem.Block{Type: "CERTIFICATE", Bytes: der}))
	return cert, key, pemBuf.Bytes()
}

// mustGenServerCert signs a server cert valid for 127.0.0.1 (IP SAN).
// Both binding stubs share this cert since both resolve to 127.0.0.1.
func mustGenServerCert(t *testing.T, ca *x509.Certificate, caKey *ecdsa.PrivateKey) tls.Certificate {
	t.Helper()
	srvKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)

	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(2),
		Subject:      pkix.Name{CommonName: "orange-sni-stub"},
		IPAddresses:  []net.IP{net.ParseIP("127.0.0.1")},
		NotBefore:    time.Now().Add(-time.Minute),
		NotAfter:     time.Now().Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, ca, &srvKey.PublicKey, caKey)
	require.NoError(t, err)

	keyDER, err := x509.MarshalECPrivateKey(srvKey)
	require.NoError(t, err)

	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})

	cert, err := tls.X509KeyPair(certPEM, keyPEM)
	require.NoError(t, err)
	return cert
}

// mustRenderTemplate renders a Go text/template string with the given data map.
func mustRenderTemplate(t *testing.T, tmpl string, data any) []byte {
	t.Helper()
	parsed := template.Must(template.New("t").Parse(tmpl))
	var buf bytes.Buffer
	require.NoError(t, parsed.Execute(&buf, data))
	return buf.Bytes()
}
