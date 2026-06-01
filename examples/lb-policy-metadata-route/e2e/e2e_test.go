// Package e2e runs integration tests for the lb-policy-metadata-route filter
// against a real Envoy instance.
//
// TestMain starts an in-process upstream, starts Envoy with the Makefile-built
// custom LB policy loaded, runs all tests, then tears everything down.
//
// Prerequisites:
//   - Envoy binary at .bin/envoy in the transit root (run: make download-envoy)
//     or set ENVOY_BIN.
//
// Run:
//
//	make -C examples/lb-policy-metadata-route e2e
//
// Or directly (from the examples/ directory):
//
//	ENVOY_BIN=../.bin/envoy GOWORK=off go test ./lb-policy-metadata-route/e2e/... -v -timeout=60s
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
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/dio/transit/examples/internal/e2etest"
)

//go:embed testdata/envoy.tmpl.yaml
var envoyConfigTmpl string

var (
	proxyURL            string
	examplesRoot        string
	upstreamPrimaryPort int
	upstreamPremiumPort int
)

func TestMain(m *testing.M) {
	_, file, _, _ := runtime.Caller(0)
	// lb-policy-metadata-route/e2e/e2e_test.go → examples/
	examplesRoot = filepath.Join(filepath.Dir(file), "../../")

	bin := e2etest.EnvoyBin(examplesRoot)
	if _, err := os.Stat(bin); err != nil {
		fmt.Fprintf(os.Stderr, "SKIP: envoy not found at %s (run: make download-envoy)\n", bin)
		os.Exit(0)
	}

	exampleDir := filepath.Join(examplesRoot, "lb-policy-metadata-route")
	if err := e2etest.CheckSharedLibrary(examplesRoot, "lb-policy-metadata-route", "liblb-policy-metadata-route.so"); err != nil {
		fmt.Fprintf(os.Stderr, "e2e: %v\n", err)
		os.Exit(1)
	}

	upstreamPrimaryPort = startUpstream("primary")
	upstreamPremiumPort = startUpstream("premium")

	proxyPort := e2etest.FreePort()
	adminPort := e2etest.FreePort()
	proxyURL = fmt.Sprintf("http://127.0.0.1:%d", proxyPort)

	cfgPath := e2etest.WriteEnvoyConfig("lb-policy-metadata-route-e2e", envoyConfigTmpl, map[string]int{
		"ProxyPort":           proxyPort,
		"UpstreamPrimaryPort": upstreamPrimaryPort,
		"UpstreamPremiumPort": upstreamPremiumPort,
		"AdminPort":           adminPort,
	})

	stop, ok := e2etest.StartEnvoy(bin, cfgPath, exampleDir, adminPort, nil)
	if !ok {
		os.Exit(1)
	}
	fmt.Fprintln(os.Stderr, "e2e: envoy ready")

	code := m.Run()
	stop()
	os.Exit(code)
}

// TestGet_requestPrimaryCapability verifies that a request with
// x-required-capability: primary routes to the primary host.
func TestGet_requestPrimaryCapability(t *testing.T) {
	req, err := http.NewRequest(http.MethodGet, proxyURL+"/", nil)
	require.NoError(t, err)
	req.Header.Set("x-required-capability", "primary")
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)

	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	require.Equal(t, "primary", string(body))
}

// TestGet_requestPremiumCapability verifies that a request with
// x-required-capability: premium routes to the premium host.
func TestGet_requestPremiumCapability(t *testing.T) {
	req, err := http.NewRequest(http.MethodGet, proxyURL+"/", nil)
	require.NoError(t, err)
	req.Header.Set("x-required-capability", "premium")
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)

	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	require.Equal(t, "premium", string(body))
}

// TestGet_unmatchedCapability_fallbackToFirst verifies that a request with
// an unmapped capability falls back to the first host.
func TestGet_unmatchedCapability_fallbackToFirst(t *testing.T) {
	req, err := http.NewRequest(http.MethodGet, proxyURL+"/", nil)
	require.NoError(t, err)
	req.Header.Set("x-required-capability", "experimental")
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)

	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	// Fallback is first host in config (primary)
	require.Equal(t, "primary", string(body))
}

// TestGet_noCapabilityHeader_fallbackToFirst verifies that a request without
// the capability header falls back to the first host.
func TestGet_noCapabilityHeader_fallbackToFirst(t *testing.T) {
	resp, err := http.Get(proxyURL + "/")
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)

	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	require.Equal(t, "primary", string(body))
}

// ── helpers ───────────────────────────────────────────────────────────────────

func startUpstream(name string) int {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		panic("startUpstream: " + err.Error())
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, name)
	})
	go http.Serve(l, mux) //nolint:errcheck
	return l.Addr().(*net.TCPAddr).Port
}
