// Package e2e runs integration tests for the body-transform filter against a real Envoy instance.
//
// TestMain starts a plain HTTP upstream that echoes the request body, then starts
// Envoy with the Makefile-built filter loaded, runs all tests, then tears everything down.
//
// Prerequisites:
//   - Envoy binary at .bin/envoy in the transit root (run: make download-envoy)
//     or set ENVOY_BIN.
//
// Run:
//
//	make -C examples/body-transform e2e
//
// Or directly (from the examples/ directory):
//
//	ENVOY_BIN=../.bin/envoy GOWORK=off go test ./body-transform/e2e/... -v -timeout=60s
package e2e

import (
	_ "embed"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/dio/transit/examples/internal/e2etest"
)

//go:embed testdata/envoy.tmpl.yaml
var envoyConfigTmpl string

var (
	proxyURL     string
	examplesRoot string
)

func TestMain(m *testing.M) {
	_, file, _, _ := runtime.Caller(0)
	// body-transform/e2e/e2e_test.go → examples/
	examplesRoot = filepath.Join(filepath.Dir(file), "../../")

	bin := e2etest.EnvoyBin(examplesRoot)
	if _, err := os.Stat(bin); err != nil {
		fmt.Fprintf(os.Stderr, "SKIP: envoy not found at %s (run: make download-envoy)\n", bin)
		os.Exit(0)
	}

	btDir := filepath.Join(examplesRoot, "body-transform")
	if err := e2etest.CheckSharedLibrary(examplesRoot, "body-transform", "libbody-transform.so"); err != nil {
		fmt.Fprintf(os.Stderr, "e2e: %v\n", err)
		os.Exit(1)
	}

	// Start a plain HTTP upstream that echoes the request body.
	upstreamPort := e2etest.FreePort()
	upstreamMux := http.NewServeMux()
	upstreamMux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		w.Header().Set("content-type", r.Header.Get("content-type"))
		w.WriteHeader(http.StatusOK)
		w.Write(body) //nolint:errcheck
	})
	upstreamServer := &http.Server{
		Addr:    fmt.Sprintf("127.0.0.1:%d", upstreamPort),
		Handler: upstreamMux,
	}
	upstreamListener, err := net.Listen("tcp", upstreamServer.Addr)
	if err != nil {
		fmt.Fprintf(os.Stderr, "e2e: upstream listen failed: %v\n", err)
		os.Exit(1)
	}
	go upstreamServer.Serve(upstreamListener) //nolint:errcheck

	proxyPort := e2etest.FreePort()
	adminPort := e2etest.FreePort()
	proxyURL = fmt.Sprintf("http://127.0.0.1:%d", proxyPort)

	cfgPath := e2etest.WriteEnvoyConfig("transit-body-transform-e2e", envoyConfigTmpl, map[string]int{
		"ProxyPort":    proxyPort,
		"AdminPort":    adminPort,
		"UpstreamPort": upstreamPort,
	})

	stop, ok := e2etest.StartEnvoy(bin, cfgPath, btDir, adminPort, nil)
	if !ok {
		os.Exit(1)
	}
	fmt.Fprintln(os.Stderr, "e2e: envoy ready")

	code := m.Run()
	upstreamServer.Close()
	stop()
	os.Exit(code)
}

func TestPost_jsonBody_messageRenamedToText(t *testing.T) {
	resp, err := http.Post(proxyURL+"/", "application/json", strings.NewReader(`{"message":"hello"}`))
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)
	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	got := strings.TrimSpace(string(body))
	require.Equal(t, `{"text":"hello"}`, got)
}

func TestPost_jsonBodyMissingMessage_passedThrough(t *testing.T) {
	// JSON without "message" field should pass through unchanged
	resp, err := http.Post(proxyURL+"/", "application/json", strings.NewReader(`{"other":"value"}`))
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)
	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	got := strings.TrimSpace(string(body))
	require.Equal(t, `{"other":"value"}`, got)
}

func TestPost_emptyBody_passedThrough(t *testing.T) {
	// Empty body should pass through unchanged
	resp, err := http.Post(proxyURL+"/", "application/json", strings.NewReader(""))
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)
	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	got := strings.TrimSpace(string(body))
	require.Equal(t, "", got)
}

func TestPost_malformedJSON_passedThrough(t *testing.T) {
	// Malformed JSON should pass through unchanged (filter doesn't validate)
	malformedJSON := `{invalid json`
	resp, err := http.Post(proxyURL+"/", "application/json", strings.NewReader(malformedJSON))
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)
	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	got := strings.TrimSpace(string(body))
	require.Equal(t, malformedJSON, got)
}

func TestPost_nonJSON_passedThrough(t *testing.T) {
	resp, err := http.Post(proxyURL+"/", "text/plain", strings.NewReader("hello"))
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)
	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	got := strings.TrimSpace(string(body))
	require.Equal(t, "hello", got)
}
