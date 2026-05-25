// Package e2e runs integration tests for the header-router filter against a real Envoy instance.
//
// TestMain starts two upstream servers (backend-a and backend-b), starts Envoy with the
// Makefile-built filter loaded, runs all tests, then tears everything down.
//
// Prerequisites:
//   - Envoy binary at .bin/envoy in the transit root (run: make download-envoy)
//     or set ENVOY_BIN.
//
// Run:
//
//	make -C examples/header-router e2e
//
// Or directly (from the examples/ directory):
//
//	ENVOY_BIN=../.bin/envoy GOWORK=off go test ./header-router/e2e/... -v -timeout=60s
package e2e

import (
	_ "embed"
	"fmt"
	"io"
	"net"
	"net/http"
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
)

func TestMain(m *testing.M) {
	_, file, _, _ := runtime.Caller(0)
	// header-router/e2e/e2e_test.go → examples/
	examplesRoot = filepath.Join(filepath.Dir(file), "../../")

	bin := e2etest.EnvoyBin(examplesRoot)
	if _, err := os.Stat(bin); err != nil {
		fmt.Fprintf(os.Stderr, "SKIP: envoy not found at %s (run: make download-envoy)\n", bin)
		os.Exit(0)
	}

	if err := e2etest.CheckSharedLibrary(examplesRoot, "header-router", "libheader-router.so"); err != nil {
		fmt.Fprintf(os.Stderr, "e2e: %v\n", err)
		os.Exit(1)
	}

	// Start backend-a.
	backendAPort := e2etest.FreePort()
	backendA := startBackend(backendAPort, "backend-a")
	defer backendA.Close()

	// Start backend-b.
	backendBPort := e2etest.FreePort()
	backendB := startBackend(backendBPort, "backend-b")
	defer backendB.Close()

	proxyPort := e2etest.FreePort()
	adminPort := e2etest.FreePort()
	proxyURL = fmt.Sprintf("http://127.0.0.1:%d", proxyPort)

	type tmplData struct {
		ProxyPort    int
		AdminPort    int
		BackendAPort int
		BackendBPort int
	}
	cfgPath := e2etest.WriteEnvoyConfig("transit-header-router-e2e", envoyConfigTmpl, tmplData{
		ProxyPort:    proxyPort,
		AdminPort:    adminPort,
		BackendAPort: backendAPort,
		BackendBPort: backendBPort,
	})
	defer os.Remove(cfgPath)

	headerRouterDir := filepath.Join(examplesRoot, "header-router")
	envoyCmd = exec.Command(bin, "-c", cfgPath, "--log-level", "warning",
		"--component-log-level", "dynamic_modules:info")
	envoyCmd.Env = append(os.Environ(),
		"GODEBUG=cgocheck=0",
		"ENVOY_DYNAMIC_MODULES_SEARCH_PATH="+headerRouterDir,
		fmt.Sprintf("HEADER_ROUTER_HOST_A=127.0.0.1:%d", backendAPort),
		fmt.Sprintf("HEADER_ROUTER_HOST_B=127.0.0.1:%d", backendBPort),
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

func TestGet_routeToA_respondsBackendA(t *testing.T) {
	req, _ := http.NewRequest(http.MethodGet, proxyURL+"/", nil)
	req.Header.Set("x-route-to", "a")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET /: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if string(body) != "backend-a" {
		t.Fatalf("want body %q, got %q", "backend-a", string(body))
	}
}

func TestGet_routeToB_respondsBackendB(t *testing.T) {
	req, _ := http.NewRequest(http.MethodGet, proxyURL+"/", nil)
	req.Header.Set("x-route-to", "b")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET /: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if string(body) != "backend-b" {
		t.Fatalf("want body %q, got %q", "backend-b", string(body))
	}
}

func TestGet_noHeader_responds200(t *testing.T) {
	resp, err := http.Get(proxyURL + "/")
	if err != nil {
		t.Fatalf("GET /: %v", err)
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
