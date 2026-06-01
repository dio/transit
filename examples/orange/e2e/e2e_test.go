// Package e2e runs integration tests for the orange LLM proxy against a real
// Envoy instance, routing through GitHub Models (models.inference.ai.azure.com).
// Requires GITHUB_TOKEN; locally: GITHUB_TOKEN=$(gh auth token) make e2e
package e2e

import (
	"bytes"
	_ "embed"
	"encoding/json"
	"fmt"
	"io"
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

//go:embed testdata/orange.yaml
var orangeConfig []byte

var (
	proxyURL     string
	debugURL     string
	examplesRoot string
)

func TestMain(m *testing.M) {
	_, file, _, _ := runtime.Caller(0)
	examplesRoot = filepath.Join(filepath.Dir(file), "../../")

	bin := e2etest.EnvoyBin(examplesRoot)
	if _, err := os.Stat(bin); err != nil {
		fmt.Fprintf(os.Stderr, "SKIP: envoy not found at %s (run: make download-envoy)\n", bin)
		os.Exit(0)
	}

	if err := e2etest.CheckSharedLibrary(examplesRoot, "orange", "liborange.so"); err != nil {
		fmt.Fprintf(os.Stderr, "e2e: %v\n", err)
		os.Exit(1)
	}

	if os.Getenv("GITHUB_TOKEN") == "" {
		fmt.Fprintln(os.Stderr, "SKIP: GITHUB_TOKEN not set (locally: GITHUB_TOKEN=$(gh auth token) make e2e)")
		os.Exit(0)
	}

	systemCA := findSystemCA()
	if systemCA == "" {
		fmt.Fprintln(os.Stderr, "e2e: no system CA bundle found")
		os.Exit(1)
	}

	orangeCfgFile, err := os.CreateTemp("", "orange-e2e-*.yaml")
	if err != nil {
		fmt.Fprintf(os.Stderr, "e2e: write orange config: %v\n", err)
		os.Exit(1)
	}
	if _, err := orangeCfgFile.Write(orangeConfig); err != nil {
		fmt.Fprintf(os.Stderr, "e2e: write orange config: %v\n", err)
		os.Exit(1)
	}
	_ = orangeCfgFile.Close()
	defer os.Remove(orangeCfgFile.Name())

	proxyPort := e2etest.FreePort()
	adminPort := e2etest.FreePort()
	debugPort := e2etest.FreePort()
	proxyURL = fmt.Sprintf("http://127.0.0.1:%d", proxyPort)
	debugURL = fmt.Sprintf("http://127.0.0.1:%d", debugPort)

	cfgPath := e2etest.WriteEnvoyConfig("orange", envoyConfigTmpl, map[string]any{
		"ProxyPort":    proxyPort,
		"AdminPort":    adminPort,
		"SystemCAFile": systemCA,
	})

	exampleDir := filepath.Join(examplesRoot, "orange")
	stop, ok := e2etest.StartEnvoy(bin, cfgPath, exampleDir, adminPort, []string{
		"ORANGE_CONFIG=" + orangeCfgFile.Name(),
		fmt.Sprintf("ORANGE_DEBUG_ADDR=127.0.0.1:%d", debugPort),
	})
	if !ok {
		os.Exit(1)
	}
	fmt.Fprintln(os.Stderr, "e2e: envoy ready")

	code := m.Run()
	stop()
	os.Exit(code)
}

var testClient = &http.Client{Timeout: 30 * time.Second}

func chatCompletion(t *testing.T, body string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, proxyURL+"/v1/chat/completions",
		bytes.NewBufferString(body))
	require.NoError(t, err)
	req.Header.Set("content-type", "application/json")
	resp, err := testClient.Do(req)
	require.NoError(t, err)
	return resp
}

func TestChatCompletion_gpt4oMini(t *testing.T) {
	resp := chatCompletion(t, `{"model":"gpt-4o-mini","messages":[{"role":"user","content":"Say hello."}],"max_tokens":16}`)
	defer resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)

	raw, err := io.ReadAll(resp.Body)
	require.NoError(t, err)

	var result struct {
		Choices []json.RawMessage `json:"choices"`
	}
	require.NoError(t, json.Unmarshal(raw, &result))
	require.NotEmpty(t, result.Choices)
}

func TestChatCompletion_unknownModel(t *testing.T) {
	resp := chatCompletion(t, `{"model":"nonexistent-model","messages":[{"role":"user","content":"hi"}]}`)
	defer resp.Body.Close()
	require.Equal(t, http.StatusNotFound, resp.StatusCode)
}

func findSystemCA() string {
	for _, p := range []string{
		"/etc/ssl/cert.pem",                  // macOS, FreeBSD
		"/etc/ssl/certs/ca-certificates.crt", // Debian, Ubuntu, Alpine
		"/etc/pki/tls/certs/ca-bundle.crt",   // RHEL, CentOS, Fedora
	} {
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	return ""
}
