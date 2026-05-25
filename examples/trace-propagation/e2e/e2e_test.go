// Package e2e runs integration tests for the trace-propagation filter against a
// real Envoy instance.
//
// TestMain starts an in-process backend sink, starts Envoy with the
// Makefile-built filter loaded, runs all tests, then tears everything down.
//
// Prerequisites:
//   - Envoy binary at .bin/envoy in the transit root (run: make download-envoy)
//     or set ENVOY_BIN.
//
// Run:
//
//	make -C examples/trace-propagation e2e
//
// Or directly (from the examples/ directory):
//
//	ENVOY_BIN=../.bin/envoy GOWORK=off go test ./trace-propagation/e2e/... -v -timeout=60s
package e2e

import (
	_ "embed"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"testing"

	"github.com/dio/transit/examples/internal/e2etest"
)

//go:embed testdata/envoy.tmpl.yaml
var envoyConfigTmpl string

var (
	proxyURL     string
	examplesRoot string

	sinkMu          sync.Mutex
	lastSinkHeaders http.Header
)

func TestMain(m *testing.M) {
	_, file, _, _ := runtime.Caller(0)
	// trace-propagation/e2e/e2e_test.go → examples/
	examplesRoot = filepath.Join(filepath.Dir(file), "../../")

	bin := e2etest.EnvoyBin(examplesRoot)
	if _, err := os.Stat(bin); err != nil {
		fmt.Fprintf(os.Stderr, "SKIP: envoy not found at %s (run: make download-envoy)\n", bin)
		os.Exit(0)
	}

	if err := e2etest.CheckSharedLibrary(examplesRoot, "trace-propagation", "libtrace-propagation.so"); err != nil {
		fmt.Fprintf(os.Stderr, "e2e: %v\n", err)
		os.Exit(1)
	}

	backendPort := startSink()
	serverPort := e2etest.FreePort()
	egressPort := e2etest.FreePort()
	proxyPort := e2etest.FreePort()
	adminPort := e2etest.FreePort()
	proxyURL = fmt.Sprintf("http://127.0.0.1:%d", proxyPort)

	cfgPath := e2etest.WriteEnvoyConfig("trace-propagation-e2e", envoyConfigTmpl, map[string]int{
		"ProxyPort":   proxyPort,
		"ServerPort":  serverPort,
		"EgressPort":  egressPort,
		"BackendPort": backendPort,
		"AdminPort":   adminPort,
	})

	exampleDir := filepath.Join(examplesRoot, "trace-propagation")
	stop, ok := e2etest.StartEnvoy(bin, cfgPath, exampleDir, adminPort, []string{
		fmt.Sprintf("TRACE_PROPAGATION_LISTEN_ADDR=127.0.0.1:%d", serverPort),
		fmt.Sprintf("TRACE_PROPAGATION_EGRESS_URL=http://127.0.0.1:%d", egressPort),
	})
	if !ok {
		os.Exit(1)
	}
	fmt.Fprintln(os.Stderr, "e2e: envoy ready")

	code := m.Run()
	stop()
	os.Exit(code)
}

// TestTrace_traceparent_propagated verifies that a known traceparent header
// reaches the backend sink unchanged through the full two-leg path.
func TestTrace_traceparent_propagated(t *testing.T) {
	want := "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01"
	req, err := http.NewRequest(http.MethodGet, proxyURL+"/", nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("traceparent", want)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET /: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("want 200, got %d", resp.StatusCode)
	}
	h := getSinkHeaders()
	if got := h.Get("traceparent"); got != want {
		t.Errorf("sink traceparent: want %q, got %q", want, got)
	}
}

// TestTrace_tracestate_propagated verifies that a tracestate header is forwarded.
func TestTrace_tracestate_propagated(t *testing.T) {
	req, err := http.NewRequest(http.MethodGet, proxyURL+"/", nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("traceparent", "00-aaaabbbbccccdddd-eeeeffff-01")
	req.Header.Set("tracestate", "vendor=opaque")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET /: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("want 200, got %d", resp.StatusCode)
	}
	h := getSinkHeaders()
	if got := h.Get("tracestate"); got != "vendor=opaque" {
		t.Errorf("sink tracestate: want %q, got %q", "vendor=opaque", got)
	}
}

// TestTrace_noTraceparent_reaches_sink verifies that requests without trace
// headers still reach the sink and return 200.
func TestTrace_noTraceparent_reaches_sink(t *testing.T) {
	resp, err := http.Get(proxyURL + "/")
	if err != nil {
		t.Fatalf("GET /: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("want 200, got %d", resp.StatusCode)
	}
}

// TestTrace_xRequestId_propagated verifies that x-request-id is forwarded.
func TestTrace_xRequestId_propagated(t *testing.T) {
	req, err := http.NewRequest(http.MethodGet, proxyURL+"/", nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("x-request-id", "test-req-abc")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET /: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("want 200, got %d", resp.StatusCode)
	}
	h := getSinkHeaders()
	if got := h.Get("x-request-id"); got == "" {
		t.Error("sink missing x-request-id")
	}
}

// ── helpers ───────────────────────────────────────────────────────────────────

// startSink starts a backend HTTP server that records the headers of each
// request it receives. Returns the port it is listening on.
func startSink() int {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		panic("startSink: " + err.Error())
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		sinkMu.Lock()
		lastSinkHeaders = r.Header.Clone()
		sinkMu.Unlock()
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, "sink ok")
	})
	go http.Serve(l, mux) //nolint:errcheck
	return l.Addr().(*net.TCPAddr).Port
}

func getSinkHeaders() http.Header {
	sinkMu.Lock()
	defer sinkMu.Unlock()
	if lastSinkHeaders == nil {
		return http.Header{}
	}
	return lastSinkHeaders.Clone()
}
