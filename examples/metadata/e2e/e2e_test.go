// Package e2e runs integration tests for the metadata example against a real
// Envoy instance.
//
// TestMain starts two upstream servers (standard and premium), starts Envoy
// with the metadata module loaded, runs all tests, then tears everything down.
//
// Prerequisites:
//   - Envoy binary at .bin/envoy in the transit root (run: make download-envoy)
//     or set ENVOY_BIN.
//
// Run:
//
//	make -C examples/metadata e2e
//
// Or directly (from the examples/ directory):
//
//	ENVOY_BIN=../.bin/envoy GOWORK=off go test ./metadata/e2e/... -v -timeout=60s
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
	// metadata/e2e/e2e_test.go → examples/
	examplesRoot = filepath.Join(filepath.Dir(file), "../../")

	bin := e2etest.EnvoyBin(examplesRoot)
	if _, err := os.Stat(bin); err != nil {
		fmt.Fprintf(os.Stderr, "SKIP: envoy not found at %s (run: make download-envoy)\n", bin)
		os.Exit(0)
	}

	if err := e2etest.CheckSharedLibrary(examplesRoot, "metadata", "libmetadata.so"); err != nil {
		fmt.Fprintf(os.Stderr, "e2e: %v\n", err)
		os.Exit(1)
	}

	// Start standard backend — responds "standard".
	standardPort := e2etest.FreePort()
	standardBackend := startBackend(standardPort, "standard")
	defer standardBackend.Close()

	// Start premium backend — responds "premium".
	premiumPort := e2etest.FreePort()
	premiumBackend := startBackend(premiumPort, "premium")
	defer premiumBackend.Close()

	proxyPort := e2etest.FreePort()
	adminPort := e2etest.FreePort()
	proxyURL = fmt.Sprintf("http://127.0.0.1:%d", proxyPort)

	type tmplData struct {
		ProxyPort    int
		AdminPort    int
		StandardPort int
		PremiumPort  int
	}
	cfgPath := e2etest.WriteEnvoyConfig("transit-metadata-e2e", envoyConfigTmpl, tmplData{
		ProxyPort:    proxyPort,
		AdminPort:    adminPort,
		StandardPort: standardPort,
		PremiumPort:  premiumPort,
	})
	defer os.Remove(cfgPath)

	metadataDir := filepath.Join(examplesRoot, "metadata")
	stop, ok := e2etest.StartEnvoy(bin, cfgPath, metadataDir, adminPort, nil)
	if !ok {
		os.Exit(1)
	}
	fmt.Fprintln(os.Stderr, "e2e: envoy ready")

	code := m.Run()
	stop()
	os.Exit(code)
}

func TestPremiumRoute_selectsPremiumHost(t *testing.T) {
	resp, err := http.Get(proxyURL + "/premium/") //nolint:noctx
	if err != nil {
		t.Fatalf("GET /premium/: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if string(body) != "premium" {
		t.Fatalf("want body %q, got %q", "premium", string(body))
	}
}

func TestStandardRoute_selectsStandardHost(t *testing.T) {
	resp, err := http.Get(proxyURL + "/standard/") //nolint:noctx
	if err != nil {
		t.Fatalf("GET /standard/: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if string(body) != "standard" {
		t.Fatalf("want body %q, got %q", "standard", string(body))
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
