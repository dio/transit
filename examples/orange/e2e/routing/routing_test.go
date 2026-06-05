// Package routing_test verifies the three routing compositions described in the
// README — flat split, split-of-chains (split-then-fallback), chain-of-splits —
// and confirms that chain-of-chain is caught at runtime with a 404.
//
// Tests run against a real Envoy + liborange.so instance using local TLS stub
// servers. No external API credentials are required.
//
// Prerequisites (test skips gracefully when absent):
//   - Custom Envoy binary at ../../.bin/envoy or ENVOY_BIN env var
//   - Built liborange.so (run: make -C examples/orange build)
package routing_test

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
	"sync/atomic"
	"testing"
	"text/template"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/dio/transit/examples/internal/e2etest"
)

// routingEnvoyTmpl is the Envoy bootstrap for routing tests.
// It omits MCP/responsesws plumbing and adds a retry floor on the LLM route so
// that fallback chains (injected x-envoy-retry-on / x-envoy-max-retries) fire.
const routingEnvoyTmpl = `static_resources:
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
                          route:
                            cluster: orange_default
                            timeout: 30s
                            retry_policy:
                              retry_on: "5xx,connect-failure,reset,gateway-error,retriable-status-codes"
                              retriable_status_codes: [503]
                              num_retries: 7
                              per_try_timeout: 5s
                              retry_back_off:
                                base_interval: 0.1s
                                max_interval: 1s

  clusters:
    - name: orange_default
      connect_timeout: 5s
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
            - name: orange-meter
              typed_config:
                "@type": type.googleapis.com/envoy.extensions.filters.http.dynamic_modules.v3.DynamicModuleFilter
                dynamic_module_config:
                  name: orange
                filter_name: orange-meter
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

// routingOrangeTmpl is the orange.yaml for routing tests.
// Four providers backed by local TLS stubs:
//   - live_a, live_b, live_c — always return 200.
//   - dead                   — always returns 503 (triggers retry/fallback).
//
// Four keys exercise the documented compositions:
//   - sk-flat-split    : flat 50/50 split between live_a and live_b.
//   - sk-split-chain   : split where both arms are chains (dead → live fallback).
//   - sk-chain-split   : chain whose first position is a split (live_a or live_b).
//   - sk-chain-chain   : chain-of-chain — passes config load, fails at runtime.
const routingOrangeTmpl = `llm:
  providers:
    live_a:
      kind: openai
      endpoint: https://127.0.0.1:{{.LiveAPort}}
      auth:
        type: bearer
        secret_ref: literal://test-key
    live_b:
      kind: openai
      endpoint: https://127.0.0.1:{{.LiveBPort}}
      auth:
        type: bearer
        secret_ref: literal://test-key
    live_c:
      kind: openai
      endpoint: https://127.0.0.1:{{.LiveCPort}}
      auth:
        type: bearer
        secret_ref: literal://test-key
    dead:
      kind: openai
      endpoint: https://127.0.0.1:{{.DeadPort}}
      auth:
        type: bearer
        secret_ref: literal://test-key
  models: {}

keys:
  # Flat split: 50/50 between live_a and live_b. No chain, no retry needed.
  test/user/sk-flat-split:
    workspace: test
    user: user
    llm:
      models:
        my-model:
          routing:
            split:
              children:
                - weight: 50
                  target: { provider: live_a }
                - weight: 50
                  target: { provider: live_b }

  # Split-of-chains: both arms chain from dead (503) to a live fallback.
  # Every request must retry to the fallback to succeed.
  test/user/sk-split-chain:
    workspace: test
    user: user
    llm:
      models:
        my-model:
          routing:
            split:
              children:
                - weight: 50
                  chain:
                    retry:
                      retry_on: "5xx,connect-failure,reset"
                      per_try_timeout_ms: 4000
                    children:
                      - target: { provider: dead }
                      - target: { provider: live_a }
                - weight: 50
                  chain:
                    retry:
                      retry_on: "5xx,connect-failure,reset"
                      per_try_timeout_ms: 4000
                    children:
                      - target: { provider: dead }
                      - target: { provider: live_b }

  # Chain-of-splits: position 0 is a split (live_a or live_b), position 1 is live_c.
  # All requests succeed on the first attempt; the chain fallback is never needed.
  test/user/sk-chain-split:
    workspace: test
    user: user
    llm:
      models:
        my-model:
          routing:
            chain:
              children:
                - split:
                    children:
                      - weight: 50
                        target: { provider: live_a }
                      - weight: 50
                        target: { provider: live_b }
                - target: { provider: live_c }

  # Chain-of-chain: passes config.Load but resolveRouting returns an error at
  # runtime because chain.children[0] is itself a chain. Expected: HTTP 404
  # with code orange.model_not_found.
  test/user/sk-chain-chain:
    workspace: test
    user: user
    llm:
      models:
        my-model:
          routing:
            chain:
              children:
                - chain:
                    children:
                      - target: { provider: live_a }
                      - target: { provider: live_b }
                - target: { provider: live_c }
`

// fakeChatResponse is a minimal valid OpenAI chat completion JSON.
const fakeChatResponse = `{
  "id": "test",
  "object": "chat.completion",
  "model": "my-model",
  "choices": [{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],
  "usage": {"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}
}`

var (
	proxyURL    string
	liveAHits   atomic.Int64
	liveBHits   atomic.Int64
	liveCHits   atomic.Int64
	deadHits    atomic.Int64
)

func TestMain(m *testing.M) {
	_, file, _, _ := runtime.Caller(0)
	examplesRoot := filepath.Join(filepath.Dir(file), "../../../")

	bin := e2etest.EnvoyBin(examplesRoot)
	if _, err := os.Stat(bin); err != nil {
		fmt.Fprintf(os.Stderr, "SKIP: envoy not found at %s (run: make download-envoy)\n", bin)
		os.Exit(0)
	}
	if err := e2etest.CheckSharedLibrary(examplesRoot, "orange", "liborange.so"); err != nil {
		fmt.Fprintf(os.Stderr, "e2e/routing: %v\n", err)
		os.Exit(1)
	}

	// --- TLS material -----------------------------------------------------------

	ca, caKey, caPEM := genCA()

	caFile, err := os.CreateTemp("", "orange-routing-ca-*.pem")
	if err != nil {
		fmt.Fprintf(os.Stderr, "e2e/routing: write CA file: %v\n", err)
		os.Exit(1)
	}
	if _, err := caFile.Write(caPEM); err != nil {
		fmt.Fprintf(os.Stderr, "e2e/routing: write CA pem: %v\n", err)
		os.Exit(1)
	}
	_ = caFile.Close()
	defer os.Remove(caFile.Name())

	serverCert := genServerCert(ca, caKey)
	tlsCfg := &tls.Config{Certificates: []tls.Certificate{serverCert}} //nolint:gosec — test-only CA

	// --- Stub servers -----------------------------------------------------------

	liveAPort, stopLiveA := startStub(tlsCfg, http.StatusOK, &liveAHits)
	liveBPort, stopLiveB := startStub(tlsCfg, http.StatusOK, &liveBHits)
	liveCPort, stopLiveC := startStub(tlsCfg, http.StatusOK, &liveCHits)
	deadPort, stopDead := startStub(tlsCfg, http.StatusServiceUnavailable, &deadHits)
	defer func() { stopLiveA(); stopLiveB(); stopLiveC(); stopDead() }()

	// --- Orange config ----------------------------------------------------------

	orangeBytes := mustRender(routingOrangeTmpl, map[string]any{
		"LiveAPort": liveAPort,
		"LiveBPort": liveBPort,
		"LiveCPort": liveCPort,
		"DeadPort":  deadPort,
	})
	orangeFile, err := os.CreateTemp("", "orange-routing-*.yaml")
	if err != nil {
		fmt.Fprintf(os.Stderr, "e2e/routing: write orange config: %v\n", err)
		os.Exit(1)
	}
	if _, err := orangeFile.Write(orangeBytes); err != nil {
		fmt.Fprintf(os.Stderr, "e2e/routing: write orange config: %v\n", err)
		os.Exit(1)
	}
	_ = orangeFile.Close()
	defer os.Remove(orangeFile.Name())

	// --- Envoy ------------------------------------------------------------------

	proxyPort := e2etest.FreePort()
	adminPort := e2etest.FreePort()
	proxyURL = fmt.Sprintf("http://127.0.0.1:%d", proxyPort)

	cfgPath := e2etest.WriteEnvoyConfig("orange-routing", routingEnvoyTmpl, map[string]any{
		"ProxyPort": proxyPort,
		"AdminPort": adminPort,
		"CAFile":    caFile.Name(),
	})

	exampleDir := filepath.Join(examplesRoot, "orange")
	stop, ok := e2etest.StartEnvoy(bin, cfgPath, exampleDir, adminPort, []string{
		"ORANGE_CONFIG=" + orangeFile.Name(),
	})
	if !ok {
		os.Exit(1)
	}
	fmt.Fprintln(os.Stderr, "e2e/routing: envoy ready")

	code := m.Run()
	stop()
	os.Exit(code)
}

var testClient = &http.Client{Timeout: 30 * time.Second}

// chatRequest sends a POST /v1/chat/completions with the given API key.
func chatRequest(t *testing.T, key string) *http.Response {
	t.Helper()
	body, _ := json.Marshal(map[string]any{
		"model":      "my-model",
		"messages":   []map[string]string{{"role": "user", "content": "hi"}},
		"max_tokens": 1,
	})
	req, err := http.NewRequest(http.MethodPost, proxyURL+"/v1/chat/completions", bytes.NewReader(body))
	require.NoError(t, err)
	req.Header.Set("content-type", "application/json")
	req.Header.Set("authorization", "Bearer "+key)
	resp, err := testClient.Do(req)
	require.NoError(t, err)
	return resp
}

// errorCode decodes the orange error code from a non-2xx response body.
func errorCode(t *testing.T, resp *http.Response) string {
	t.Helper()
	var body struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&body))
	return body.Error.Code
}

// TestRouting_flatSplit verifies that a flat 50/50 split routes all requests
// to live providers and that both arms receive traffic over multiple requests.
func TestRouting_flatSplit(t *testing.T) {
	before := liveAHits.Load() + liveBHits.Load()
	const n = 10
	for i := range n {
		resp := chatRequest(t, "test/user/sk-flat-split")
		defer resp.Body.Close()
		assert.Equal(t, http.StatusOK, resp.StatusCode, "request %d", i)
	}
	after := liveAHits.Load() + liveBHits.Load()
	assert.Equal(t, int64(n), after-before, "live_a+live_b must have served all %d requests", n)
	// dead and live_c must not have been contacted.
	assert.Equal(t, int64(0), deadHits.Load(), "dead stub must not be contacted for flat split")
}

// TestRouting_splitOfChains_fallback verifies that when every split arm is a
// chain whose primary is a dead stub (503), Orange retries through to the live
// fallback and all requests succeed.
func TestRouting_splitOfChains_fallback(t *testing.T) {
	deadBefore := deadHits.Load()
	liveABefore := liveAHits.Load()
	liveBBefore := liveBHits.Load()

	const n = 6
	for i := range n {
		resp := chatRequest(t, "test/user/sk-split-chain")
		defer resp.Body.Close()
		assert.Equal(t, http.StatusOK, resp.StatusCode, "request %d should succeed via fallback", i)
	}

	// The dead stub must have been attempted for every request (primary in both arms).
	deadAfter := deadHits.Load()
	assert.Equal(t, int64(n), deadAfter-deadBefore,
		"dead stub must be the primary attempt for every request (%d)", n)

	// The live fallbacks must have served all n successful responses.
	liveAfter := liveAHits.Load() + liveBHits.Load()
	liveBefore := liveABefore + liveBBefore
	assert.Equal(t, int64(n), liveAfter-liveBefore,
		"live_a+live_b must serve exactly %d fallback responses", n)
}

// TestRouting_chainOfSplits verifies that a chain whose first position is a
// split routes all requests to a live provider on the first attempt (no retry
// needed) and that live_c (the chain fallback) is never contacted.
func TestRouting_chainOfSplits(t *testing.T) {
	liveCBefore := liveCHits.Load()
	liveABefore := liveAHits.Load()
	liveBBefore := liveBHits.Load()

	const n = 8
	for i := range n {
		resp := chatRequest(t, "test/user/sk-chain-split")
		defer resp.Body.Close()
		assert.Equal(t, http.StatusOK, resp.StatusCode, "request %d", i)
	}

	// Split position 0 always picks live_a or live_b — all n requests succeed
	// without falling through to live_c.
	liveABafter := liveAHits.Load() + liveBHits.Load()
	assert.Equal(t, int64(n), liveABafter-(liveABefore+liveBBefore),
		"live_a+live_b must serve all %d requests from chain-of-splits", n)
	assert.Equal(t, liveCBefore, liveCHits.Load(),
		"live_c (chain fallback at position 1) must not be contacted")
}

// TestRouting_chainOfChain_runtimeError verifies that a chain whose first child
// is itself a chain passes config.Load but returns HTTP 404 with code
// orange.model_not_found when resolveRouting detects the chain-of-chain
// structure at request time.
func TestRouting_chainOfChain_runtimeError(t *testing.T) {
	resp := chatRequest(t, "test/user/sk-chain-chain")
	defer resp.Body.Close()
	require.Equal(t, http.StatusNotFound, resp.StatusCode)
	assert.Equal(t, "orange.model_not_found", errorCode(t, resp))
}

// --- helpers ------------------------------------------------------------------

// startStub starts a TLS HTTP/1.1 stub that returns statusCode for every POST.
// Returns the port, a stop func, and a request counter.
func startStub(cfg *tls.Config, statusCode int, hits *atomic.Int64) (port int, stop func()) {
	ln, err := tls.Listen("tcp", "127.0.0.1:0", cfg)
	if err != nil {
		panic("startStub: " + err.Error())
	}
	port = ln.Addr().(*net.TCPAddr).Port

	var body []byte
	if statusCode == http.StatusOK {
		body = []byte(fakeChatResponse)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		w.Header().Set("content-type", "application/json")
		w.WriteHeader(statusCode)
		if len(body) > 0 {
			_, _ = w.Write(body)
		}
	})
	srv := &http.Server{Handler: mux} //nolint:gosec — test-only server
	go func() { _ = srv.Serve(ln) }()
	stop = func() { _ = srv.Close() }
	return port, stop
}

// genCA generates an ephemeral ECDSA CA for test use only.
func genCA() (ca *x509.Certificate, key *ecdsa.PrivateKey, pemBytes []byte) {
	k, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		panic(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "orange-routing-test-ca"},
		NotBefore:             time.Now().Add(-time.Minute),
		NotAfter:              time.Now().Add(time.Hour),
		IsCA:                  true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
		BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &k.PublicKey, k)
	if err != nil {
		panic(err)
	}
	ca, err = x509.ParseCertificate(der)
	if err != nil {
		panic(err)
	}
	var buf bytes.Buffer
	_ = pem.Encode(&buf, &pem.Block{Type: "CERTIFICATE", Bytes: der})
	return ca, k, buf.Bytes()
}

// genServerCert signs a server cert valid for 127.0.0.1. All stubs share it.
func genServerCert(ca *x509.Certificate, caKey *ecdsa.PrivateKey) tls.Certificate {
	srvKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		panic(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(2),
		Subject:      pkix.Name{CommonName: "orange-routing-stub"},
		IPAddresses:  []net.IP{net.ParseIP("127.0.0.1")},
		NotBefore:    time.Now().Add(-time.Minute),
		NotAfter:     time.Now().Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, ca, &srvKey.PublicKey, caKey)
	if err != nil {
		panic(err)
	}
	keyDER, err := x509.MarshalECPrivateKey(srvKey)
	if err != nil {
		panic(err)
	}
	cert, err := tls.X509KeyPair(
		pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}),
		pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}),
	)
	if err != nil {
		panic(err)
	}
	return cert
}

// mustRender executes a text/template against data and returns the bytes.
func mustRender(tmpl string, data any) []byte {
	parsed := template.Must(template.New("t").Parse(tmpl))
	var buf bytes.Buffer
	if err := parsed.Execute(&buf, data); err != nil {
		panic(err)
	}
	return buf.Bytes()
}
