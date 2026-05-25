// Package e2e runs integration tests for the observability filter against a real Envoy instance.
//
// TestMain starts an upstream server, starts Envoy with the Makefile-built filter loaded,
// runs all tests, then tears everything down.
//
// Prerequisites:
//   - Envoy binary at .bin/envoy in the transit root (run: make download-envoy)
//     or set ENVOY_BIN.
//
// Run:
//
//	make -C examples/observability e2e
//
// Or directly (from the examples/ directory):
//
//	ENVOY_BIN=../.bin/envoy GOWORK=off go test ./observability/e2e/... -v -timeout=60s
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
	// observability/e2e/e2e_test.go → examples/
	examplesRoot = filepath.Join(filepath.Dir(file), "../../")

	bin := e2etest.EnvoyBin(examplesRoot)
	if _, err := os.Stat(bin); err != nil {
		fmt.Fprintf(os.Stderr, "SKIP: envoy not found at %s (run: make download-envoy)\n", bin)
		os.Exit(0)
	}

	if err := e2etest.CheckSharedLibrary(examplesRoot, "observability", "libobservability.so"); err != nil {
		fmt.Fprintf(os.Stderr, "e2e: %v\n", err)
		os.Exit(1)
	}

	// Start upstream backend.
	backendPort := e2etest.FreePort()
	backend := startBackend(backendPort, "upstream-ok")
	defer backend.Close()

	proxyPort := e2etest.FreePort()
	adminPort := e2etest.FreePort()
	proxyURL = fmt.Sprintf("http://127.0.0.1:%d", proxyPort)

	type tmplData struct {
		ProxyPort   int
		AdminPort   int
		BackendPort int
	}
	cfgPath := e2etest.WriteEnvoyConfig("transit-observability-e2e", envoyConfigTmpl, tmplData{
		ProxyPort:   proxyPort,
		AdminPort:   adminPort,
		BackendPort: backendPort,
	})
	defer os.Remove(cfgPath)

	observabilityDir := filepath.Join(examplesRoot, "observability")
	stop, ok := e2etest.StartEnvoy(bin, cfgPath, observabilityDir, adminPort, nil)
	if !ok {
		os.Exit(1)
	}
	fmt.Fprintln(os.Stderr, "e2e: envoy ready")

	code := m.Run()
	stop()
	os.Exit(code)
}

// TestGet_noModel_responds200 verifies that a plain request passes through unmodified.
func TestGet_noModel_responds200(t *testing.T) {
	resp, err := http.Get(proxyURL + "/")
	if err != nil {
		t.Fatalf("GET /: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("want 200, got %d (body: %s)", resp.StatusCode, body)
	}
}

// TestGet_withModel_responds200 verifies that x-model header requests pass through.
func TestGet_withModel_responds200(t *testing.T) {
	req, _ := http.NewRequest(http.MethodGet, proxyURL+"/", nil)
	req.Header.Set("x-model", "claude-sonnet-4-5")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET / with x-model: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("want 200, got %d", resp.StatusCode)
	}
}

// TestPost_responds200 verifies that POST requests pass through correctly.
func TestPost_responds200(t *testing.T) {
	resp, err := http.Post(proxyURL+"/", "application/json", nil)
	if err != nil {
		t.Fatalf("POST /: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("want 200, got %d", resp.StatusCode)
	}
}

// ── helpers ───────────────────────────────────────────────────────────────────

// startBackend starts an HTTP server on the given port that always responds
// with body as the response body (no trailing newline).
func startBackend(port int, body string) *http.Server {
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, body) //nolint:errcheck
	})
	srv := &http.Server{
		Addr:    fmt.Sprintf("127.0.0.1:%d", port),
		Handler: mux,
	}
	ln, err := net.Listen("tcp", srv.Addr)
	if err != nil {
		panic("startBackend: " + err.Error())
	}
	go srv.Serve(ln) //nolint:errcheck
	return srv
}
