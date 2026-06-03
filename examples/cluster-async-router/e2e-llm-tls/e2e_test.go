// Package e2elllmtls reproduces the orange auto_host_sni bug minimally inside
// the cluster-async-router framework: two upstreams, distinct hostnames, real
// TLS endpoints. We don't need HTTP success (no auth) — only the TLS
// handshake's SAN verdict. If auto_host_sni truly reads each host's
// hostname() at connect time, both targets should pass TLS validation.
package e2elllmtls

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

//go:embed testdata/envoy.tmpl.openai-first.yaml
var envoyConfigTemplOpenaiFirst string

//go:embed testdata/envoy.tmpl.anthropic-first.yaml
var envoyConfigTemplAnthropicFirst string

var proxyURL string

func getEnvoyConfig() string {
	hostsOrder := os.Getenv("TEST_HOSTS_ORDER")
	if hostsOrder == "openai-first" {
		return envoyConfigTemplOpenaiFirst
	}
	return envoyConfigTemplAnthropicFirst
}

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

	envoyConfigTmpl := getEnvoyConfig()
	cfgPath := e2etest.WriteEnvoyConfig("cluster-async-router-llm-tls", envoyConfigTmpl, map[string]any{
		"ProxyPort":    proxyPort,
		"AdminPort":    adminPort,
		"SystemCAFile": systemCA,
	})

	exampleDir := filepath.Join(examplesRoot, "cluster-async-router")
	stop, ok := e2etest.StartEnvoy(bin, cfgPath, exampleDir, adminPort, nil)
	if !ok {
		os.Exit(1)
	}
	fmt.Fprintln(os.Stderr, "e2e: envoy ready (llm-tls)")

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

const rounds = 3

// TestLLMTLS_Interleaved alternates anthropic and openai per round. We don't
// send credentials, so the LLM endpoints return 401/403 on a successful TLS
// handshake. A 503 means Envoy never completed TLS — almost certainly the
// orange auto_host_sni bug (wrong hostname leaked as SNI, SAN mismatch).
func TestLLMTLS_Interleaved(t *testing.T) {
	targets := []string{"anthropic", "openai"}
	if hosts := os.Getenv("TEST_HOSTS"); hosts != "" {
		targets = strings.Split(hosts, ",")
	}
	for r := 0; r < rounds; r++ {
		for _, target := range targets {
			resp := post(t, `{"target":"`+target+`"}`)
			t.Logf("round=%d target=%s status=%d", r+1, target, resp.StatusCode)
			resp.Body.Close()
			require.Less(t, resp.StatusCode, 500, "round=%d target=%s: TLS handshake failed (likely SAN mismatch)", r+1, target)
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
