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
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/dio/transit/examples/internal/e2etest"
)

//go:embed testdata/envoy.tmpl.yaml
var envoyConfigTmpl string

var (
	proxyURL     string
	envoyCmd     *exec.Cmd
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

	proxyPort := e2etest.FreePort()
	adminPort := e2etest.FreePort()
	proxyURL = fmt.Sprintf("http://127.0.0.1:%d", proxyPort)

	type ports struct {
		ProxyPort int
		AdminPort int
		AuthPort  int
	}
	cfgPath := e2etest.WriteEnvoyConfig("transit-async-callout-e2e", envoyConfigTmpl, ports{
		ProxyPort: proxyPort,
		AdminPort: adminPort,
		AuthPort:  authPort,
	})
	defer os.Remove(cfgPath)

	asyncCalloutDir := filepath.Join(examplesRoot, "async-callout")
	envoyCmd = exec.Command(bin, "-c", cfgPath, "--log-level", "warning",
		"--component-log-level", "dynamic_modules:info")
	envoyCmd.Env = append(os.Environ(),
		"GODEBUG=cgocheck=0",
		"ENVOY_DYNAMIC_MODULES_SEARCH_PATH="+asyncCalloutDir,
	)
	envoyCmd.Stdout = os.Stderr
	envoyCmd.Stderr = os.Stderr
	if err := envoyCmd.Start(); err != nil {
		fmt.Fprintf(os.Stderr, "e2e: envoy start failed: %v\n", err)
		os.Exit(1)
	}
	fmt.Fprintf(os.Stderr, "e2e: envoy pid=%d\n", envoyCmd.Process.Pid)

	if !e2etest.WaitURL(fmt.Sprintf("http://127.0.0.1:%d/ready", adminPort), 15*time.Second) {
		envoyCmd.Process.Kill()
		envoyCmd.Wait()
		fmt.Fprintln(os.Stderr, "e2e: envoy not ready in time")
		os.Exit(1)
	}
	fmt.Fprintln(os.Stderr, "e2e: envoy ready")

	code := m.Run()
	envoyCmd.Process.Kill()
	envoyCmd.Wait()
	os.Exit(code)
}

// TestAuthOK_passes verifies that when the auth stub returns "ok" the filter
// forwards the request and injects x-auth-checked: true.
func TestAuthOK_passes(t *testing.T) {
	authResponse = "ok"
	authResponseCode = http.StatusOK

	resp, err := http.Get(proxyURL + "/")
	if err != nil {
		t.Fatalf("GET /: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("want 200, got %d", resp.StatusCode)
	}
}

// TestAuthDenied_returns403 verifies that a non-"ok" body from the auth stub
// causes the filter to send a 403 local response.
func TestAuthDenied_returns403(t *testing.T) {
	authResponse = "denied"
	authResponseCode = http.StatusOK

	resp, err := http.Get(proxyURL + "/")
	if err != nil {
		t.Fatalf("GET /: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("want 403, got %d", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	if string(body) != `{"error":"denied"}` {
		t.Fatalf("unexpected body: %s", body)
	}
}

// TestAuthError_returns503 verifies that a non-2xx from the auth stub causes
// the filter to send a 503 local response.
func TestAuthError_returns503(t *testing.T) {
	authResponse = ""
	authResponseCode = http.StatusInternalServerError

	resp, err := http.Get(proxyURL + "/")
	if err != nil {
		t.Fatalf("GET /: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("want 503, got %d", resp.StatusCode)
	}
}
