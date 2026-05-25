// Package e2e runs integration tests for the trace-propagation filter against a
// real Envoy instance.
//
// TestMain starts an in-process backend sink and an in-memory OTLP gRPC
// receiver, starts Envoy with the Makefile-built filter loaded, runs all
// tests, then tears everything down.
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
	"context"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	otlptrace "go.opentelemetry.io/proto/otlp/trace/v1"

	"github.com/dio/transit/examples/internal/e2etest"
	"github.com/dio/transit/examples/internal/otelsink"
)

//go:embed testdata/envoy.tmpl.yaml
var envoyConfigTmpl string

var (
	proxyURL     string
	examplesRoot string

	otelSink *otelsink.Sink

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

	otelSink = otelsink.New()
	otelPort := otelSink.Start()
	fmt.Fprintf(os.Stderr, "e2e: OTLP sink at port %d\n", otelPort)

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
		"OtelPort":    otelPort,
	})

	exampleDir := filepath.Join(examplesRoot, "trace-propagation")
	stop, ok := e2etest.StartEnvoy(bin, cfgPath, exampleDir, adminPort, []string{
		fmt.Sprintf("TRACE_PROPAGATION_LISTEN_ADDR=127.0.0.1:%d", serverPort),
		fmt.Sprintf("TRACE_PROPAGATION_EGRESS_URL=http://127.0.0.1:%d", egressPort),
		fmt.Sprintf("TRACE_PROPAGATION_OTEL_ENDPOINT=127.0.0.1:%d", otelPort),
	})
	if !ok {
		os.Exit(1)
	}
	fmt.Fprintln(os.Stderr, "e2e: envoy ready")

	code := m.Run()
	stop()
	os.Exit(code)
}

// ── header propagation tests ──────────────────────────────────────────────────

// TestTrace_traceparent_propagated verifies that the trace-id from the client's
// traceparent reaches the backend sink. Envoy correctly rewrites the span-id
// (it becomes a new child span), so we assert only the trace-id is preserved.
func TestTrace_traceparent_propagated(t *testing.T) {
	traceID := "4bf92f3577b34da6a3ce929d0e0e4736"
	req, err := http.NewRequest(http.MethodGet, proxyURL+"/", nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("traceparent", "00-"+traceID+"-00f067aa0ba902b7-01")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET /: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("want 200, got %d", resp.StatusCode)
	}
	h := getSinkHeaders()
	got := h.Get("traceparent")
	if !strings.Contains(got, traceID) {
		t.Errorf("sink traceparent %q does not contain trace-id %q", got, traceID)
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

// ── span / OTLP tests ─────────────────────────────────────────────────────────

// TestSpan_operationName verifies that the dynamic module sets the operation
// name on the active Envoy span and the span reaches the OTLP sink.
func TestSpan_operationName(t *testing.T) {
	resp, err := http.Get(proxyURL + "/")
	if err != nil {
		t.Fatalf("GET /: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("want 200, got %d", resp.StatusCode)
	}
	if !waitForSpan(t, func(sp *otlptrace.Span) bool {
		return sp.Name == "trace-propagation.ingress"
	}) {
		t.Error("timed out waiting for span with name=trace-propagation.ingress")
	}
}

// TestSpan_httpMethodTag verifies that the http.method attribute is set by
// the dynamic module on the exported span.
func TestSpan_httpMethodTag(t *testing.T) {
	resp, err := http.Get(proxyURL + "/")
	if err != nil {
		t.Fatalf("GET /: %v", err)
	}
	resp.Body.Close()
	if !waitForSpan(t, func(sp *otlptrace.Span) bool {
		return spanHasAttr(sp, "http.method", "GET")
	}) {
		t.Error("timed out waiting for span with http.method=GET")
	}
}

// TestSpan_httpPathTag verifies that the http.path attribute is set.
func TestSpan_httpPathTag(t *testing.T) {
	resp, err := http.Get(proxyURL + "/probe")
	if err != nil {
		t.Fatalf("GET /probe: %v", err)
	}
	resp.Body.Close()
	if !waitForSpan(t, func(sp *otlptrace.Span) bool {
		return spanHasAttr(sp, "http.path", "/probe")
	}) {
		t.Error("timed out waiting for span with http.path=/probe")
	}
}

// TestSpan_embeddedServerSpan verifies that the embedded Go HTTP server creates
// its own child span (via Go OTLP SDK) that reaches the sink.
func TestSpan_embeddedServerSpan(t *testing.T) {
	resp, err := http.Get(proxyURL + "/")
	if err != nil {
		t.Fatalf("GET /: %v", err)
	}
	resp.Body.Close()
	if !waitForSpan(t, func(sp *otlptrace.Span) bool {
		return sp.Name == "trace-propagation.embedded"
	}) {
		t.Error("timed out waiting for embedded server span")
	}
}

// TestSpan_egressSpan verifies that the egress Envoy listener creates its own
// span annotated by the trace-propagation-egress dynamic module filter.
func TestSpan_egressSpan(t *testing.T) {
	resp, err := http.Get(proxyURL + "/")
	if err != nil {
		t.Fatalf("GET /: %v", err)
	}
	resp.Body.Close()
	if !waitForSpan(t, func(sp *otlptrace.Span) bool {
		return sp.Name == "trace-propagation.egress"
	}) {
		t.Error("timed out waiting for egress span")
	}
}

// TestUpstream_filterRan verifies that the trace-propagation-upstream filter
// (wired on the backend cluster) stamps x-upstream-filter: ran on the request
// before it reaches the backend sink.
func TestUpstream_filterRan(t *testing.T) {
	resp, err := http.Get(proxyURL + "/")
	if err != nil {
		t.Fatalf("GET /: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("want 200, got %d", resp.StatusCode)
	}
	h := getSinkHeaders()
	if got := h.Get("x-upstream-filter"); got != "ran" {
		t.Errorf("upstream filter did not run: x-upstream-filter=%q", got)
	}
}

// ── helpers ───────────────────────────────────────────────────────────────────

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

func waitForSpan(t *testing.T, predicate func(*otlptrace.Span) bool) bool {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	_, ok := otelSink.WaitForSpan(ctx, predicate)
	return ok
}

func spanHasAttr(sp *otlptrace.Span, key, val string) bool {
	for _, a := range sp.Attributes {
		if a.Key == key && a.Value.GetStringValue() == val {
			return true
		}
	}
	return false
}
