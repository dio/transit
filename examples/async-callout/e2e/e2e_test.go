// Package e2e runs integration tests for the async-callout filter against a real Envoy instance.
//
// TestMain starts Envoy with the Makefile-built filter loaded and a local auth
// stub server, runs all tests, then tears everything down.
//
// Prerequisites:
//   - Envoy binary at .bin/envoy in the transit root (run: make download-envoy)
//     or set ENVOY_BIN.
//
// Run:
//
//	make -C examples/async-callout e2e
//
// Or directly (from the examples/ directory):
//
//	ENVOY_BIN=../.bin/envoy GOWORK=off go test ./async-callout/e2e/... -v -timeout=60s
package e2e

import (
	_ "embed"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
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
	proxyURL     string
	examplesRoot string

	// authResponse is the body the stub auth server returns.
	// Tests swap this between subtests.
	authResponse     = "ok"
	authResponseCode = http.StatusOK
)

func TestMain(m *testing.M) {
	_, file, _, _ := runtime.Caller(0)
	// async-callout/e2e/e2e_test.go → examples/
	examplesRoot = filepath.Join(filepath.Dir(file), "../../")

	bin := e2etest.EnvoyBin(examplesRoot)
	if _, err := os.Stat(bin); err != nil {
		fmt.Fprintf(os.Stderr, "SKIP: envoy not found at %s (run: make download-envoy)\n", bin)
		os.Exit(0)
	}

	if err := e2etest.CheckSharedLibrary(examplesRoot, "async-callout", "libasync-callout.so"); err != nil {
		fmt.Fprintf(os.Stderr, "e2e: %v\n", err)
		os.Exit(1)
	}

	// Start the stub auth server on a free port.
	authListener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		fmt.Fprintf(os.Stderr, "e2e: auth listener: %v\n", err)
		os.Exit(1)
	}
	authPort := authListener.Addr().(*net.TCPAddr).Port
	authServer := &httptest.Server{
		Listener: authListener,
		Config: &http.Server{
			Handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(authResponseCode)
				_, _ = io.WriteString(w, authResponse)
			}),
		},
	}
	authServer.Start()
	defer authServer.Close()

	// Start an upstream that echoes back request headers.
	upstreamPort := startUpstream()

	proxyPort := e2etest.FreePort()
	adminPort := e2etest.FreePort()
	proxyURL = fmt.Sprintf("http://127.0.0.1:%d", proxyPort)

	type ports struct {
		ProxyPort    int
		AdminPort    int
		AuthPort     int
		UpstreamPort int
	}
	cfgPath := e2etest.WriteEnvoyConfig("transit-async-callout-e2e", envoyConfigTmpl, ports{
		ProxyPort:    proxyPort,
		AdminPort:    adminPort,
		AuthPort:     authPort,
		UpstreamPort: upstreamPort,
	})
	asyncCalloutDir := filepath.Join(examplesRoot, "async-callout")

	stop, ok := e2etest.StartEnvoy(bin, cfgPath, asyncCalloutDir, adminPort, nil)
	if !ok {
		os.Exit(1)
	}
	fmt.Fprintln(os.Stderr, "e2e: envoy ready")

	code := m.Run()
	stop()
	os.Exit(code)
}

// TestAuthOK_passes verifies that when the auth stub returns "ok" the filter
// forwards the request, injects x-auth-checked: true, and upstream receives it.
func TestAuthOK_passes(t *testing.T) {
	authResponse = "ok"
	authResponseCode = http.StatusOK

	resp, err := http.Get(proxyURL + "/")
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)
	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	// Upstream echoes back the x-auth-checked header value
	require.Equal(t, "true", string(body))
}

// TestAuthDenied_returns403 verifies that a non-"ok" body from the auth stub
// causes the filter to send a 403 local response without forwarding.
func TestAuthDenied_returns403(t *testing.T) {
	authResponse = "denied"
	authResponseCode = http.StatusOK

	resp, err := http.Get(proxyURL + "/")
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusForbidden, resp.StatusCode)
	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	require.Equal(t, `{"error":"denied"}`, string(body))
}

// TestAuthErrorResponse_returns403 verifies that when the auth stub returns a
// non-"ok" body (even with 2xx status), the filter rejects with 403.
func TestAuthErrorResponse_returns403(t *testing.T) {
	authResponse = "server-error"
	authResponseCode = http.StatusOK

	resp, err := http.Get(proxyURL + "/")
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusForbidden, resp.StatusCode)
	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	require.Equal(t, `{"error":"denied"}`, string(body))
}

// startUpstream starts a minimal HTTP server that echoes back the x-auth-checked header.
func startUpstream() int {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		panic("startUpstream: " + err.Error())
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		// Echo back the x-auth-checked header value
		authChecked := r.Header.Get("x-auth-checked")
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, authChecked)
	})
	go http.Serve(l, mux) //nolint:errcheck
	return l.Addr().(*net.TCPAddr).Port
}
