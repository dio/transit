// Package e2e runs integration tests for the filter-chain filter against a real Envoy instance.
//
// TestMain starts a plain HTTP upstream that echoes x-filtered back as a response header,
// then starts Envoy with the Makefile-built filter loaded, runs all tests, then tears everything down.
//
// Prerequisites:
//   - Envoy binary at .bin/envoy in the transit root (run: make download-envoy)
//     or set ENVOY_BIN.
//
// Run:
//
//	make -C examples/filter-chain e2e
//
// Or directly (from the examples/ directory):
//
//	ENVOY_BIN=../.bin/envoy GOWORK=off go test ./filter-chain/e2e/... -v -timeout=60s
package e2e

import (
	_ "embed"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/dio/transit/examples/internal/e2etest"
)

//go:embed testdata/envoy.tmpl.yaml
var envoyConfigTmpl string

type ports struct {
	ProxyPort    int
	AdminPort    int
	UpstreamPort int
}

var (
	proxyURL     string
	examplesRoot string
)

func TestMain(m *testing.M) {
	_, file, _, _ := runtime.Caller(0)
	// filter-chain/e2e/e2e_test.go → examples/
	examplesRoot = filepath.Join(filepath.Dir(file), "../../")

	bin := e2etest.EnvoyBin(examplesRoot)
	if _, err := os.Stat(bin); err != nil {
		fmt.Fprintf(os.Stderr, "SKIP: envoy not found at %s (run: make download-envoy)\n", bin)
		os.Exit(0)
	}

	fcDir := filepath.Join(examplesRoot, "filter-chain")
	if err := e2etest.CheckSharedLibrary(examplesRoot, "filter-chain", "libfilter-chain.so"); err != nil {
		fmt.Fprintf(os.Stderr, "e2e: %v\n", err)
		os.Exit(1)
	}

	// Start a plain HTTP upstream that echoes x-filtered as a response header.
	upstreamPort := e2etest.FreePort()
	upstreamMux := http.NewServeMux()
	upstreamMux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("x-filtered", r.Header.Get("x-filtered"))
		w.WriteHeader(http.StatusOK)
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

	cfgPath := e2etest.WriteEnvoyConfig("transit-filter-chain-e2e", envoyConfigTmpl, ports{
		ProxyPort:    proxyPort,
		AdminPort:    adminPort,
		UpstreamPort: upstreamPort,
	})

	stop, ok := e2etest.StartEnvoy(bin, cfgPath, fcDir, adminPort, nil)
	if !ok {
		os.Exit(1)
	}
	fmt.Fprintln(os.Stderr, "e2e: envoy ready")

	code := m.Run()
	upstreamServer.Close()
	stop()
	os.Exit(code)
}

func TestRequest_withValidAPIKey_passes(t *testing.T) {
	req, err := http.NewRequest(http.MethodGet, proxyURL+"/", nil)
	require.NoError(t, err)
	req.Header.Set("x-api-key", "secret")

	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()

	require.Equal(t, http.StatusOK, resp.StatusCode)
	require.Equal(t, "true", resp.Header.Get("x-filtered"))
}

func TestRequest_missingAPIKey_returns401(t *testing.T) {
	resp, err := http.Get(proxyURL + "/") //nolint:noctx
	require.NoError(t, err)
	defer resp.Body.Close()

	require.Equal(t, http.StatusUnauthorized, resp.StatusCode)
}

func TestRequest_withEmptyAPIKey_returns401(t *testing.T) {
	req, err := http.NewRequest(http.MethodGet, proxyURL+"/", nil)
	require.NoError(t, err)
	req.Header.Set("x-api-key", "")

	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()

	require.Equal(t, http.StatusUnauthorized, resp.StatusCode)
}

func TestRequest_withAnyNonEmptyAPIKey_passes(t *testing.T) {
	// Filter only checks for presence, not the value
	req, err := http.NewRequest(http.MethodGet, proxyURL+"/", nil)
	require.NoError(t, err)
	req.Header.Set("x-api-key", "any-value")

	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()

	require.Equal(t, http.StatusOK, resp.StatusCode)
	require.Equal(t, "true", resp.Header.Get("x-filtered"))
}
