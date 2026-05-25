// Package e2e runs integration tests for the lb-policy filter against a real
// Envoy instance.
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
//	make -C examples/lb-policy e2e
//
// Or directly (from the examples/ directory):
//
//	ENVOY_BIN=../.bin/envoy GOWORK=off go test ./lb-policy/e2e/... -v -timeout=60s
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
	"text/template"

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
	// lb-policy/e2e/e2e_test.go → examples/
	examplesRoot = filepath.Join(filepath.Dir(file), "../../")

	bin := envoyBin()
	if _, err := os.Stat(bin); err != nil {
		fmt.Fprintf(os.Stderr, "SKIP: envoy not found at %s (run: make download-envoy)\n", bin)
		os.Exit(0)
	}

	lbPolicyDir := filepath.Join(examplesRoot, "lb-policy")
	if err := e2etest.CheckSharedLibrary(examplesRoot, "lb-policy", "liblb-policy.so"); err != nil {
		fmt.Fprintf(os.Stderr, "e2e: %v\n", err)
		os.Exit(1)
	}

	upstreamPort := startUpstream()
	proxyPort := freePort()
	adminPort := freePort()
	proxyURL = fmt.Sprintf("http://127.0.0.1:%d", proxyPort)

	cfgPath := writeEnvoyConfig(map[string]int{
		"ProxyPort":    proxyPort,
		"UpstreamPort": upstreamPort,
		"AdminPort":    adminPort,
	})

	stop, ok := e2etest.StartEnvoy(bin, cfgPath, lbPolicyDir, adminPort, nil)
	if !ok {
		os.Exit(1)
	}
	fmt.Fprintln(os.Stderr, "e2e: envoy ready")

	code := m.Run()
	stop()
	os.Exit(code)
}

// TestGet_routesToUpstream verifies that the first-host LB policy selects the
// only available host and the request reaches the upstream successfully.
func TestGet_routesToUpstream(t *testing.T) {
	resp, err := http.Get(proxyURL + "/")
	if err != nil {
		t.Fatalf("GET /: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("want 200, got %d", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), "upstream ok") {
		t.Fatalf("body %q does not contain 'upstream ok'", body)
	}
}

// TestGet_multipleRequests verifies that repeated requests all succeed,
// confirming the LB policy is stable across calls.
func TestGet_multipleRequests(t *testing.T) {
	for i := range 5 {
		resp, err := http.Get(proxyURL + "/")
		if err != nil {
			t.Fatalf("request %d: %v", i, err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("request %d: want 200, got %d", i, resp.StatusCode)
		}
	}
}

// ── helpers ───────────────────────────────────────────────────────────────────

// startUpstream starts a minimal HTTP server that always returns "upstream ok".
func startUpstream() int {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		panic("startUpstream: " + err.Error())
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, "upstream ok")
	})
	go http.Serve(l, mux)
	return l.Addr().(*net.TCPAddr).Port
}

func envoyBin() string {
	if b := os.Getenv("ENVOY_BIN"); b != "" {
		return b
	}
	return filepath.Join(examplesRoot, "../.bin/envoy")
}

func freePort() int {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		panic("freePort: " + err.Error())
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}


func writeEnvoyConfig(ports map[string]int) string {
	tmpl := template.Must(template.New("envoy").Parse(envoyConfigTmpl))
	f, err := os.CreateTemp("", "transit-lb-policy-e2e-*.yaml")
	if err != nil {
		panic(err)
	}
	if err := tmpl.Execute(f, ports); err != nil {
		panic("template: " + err.Error())
	}
	f.Close()
	return f.Name()
}
