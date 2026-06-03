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
	"context"
	_ "embed"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	otlplogs "go.opentelemetry.io/proto/otlp/logs/v1"
	otlpmetrics "go.opentelemetry.io/proto/otlp/metrics/v1"
	otlptrace "go.opentelemetry.io/proto/otlp/trace/v1"

	"github.com/dio/transit/examples/internal/e2etest"
	"github.com/dio/transit/examples/internal/otelsink"
)

//go:embed testdata/envoy.tmpl.yaml
var envoyConfigTmpl string

var (
	proxyURL     string
	examplesRoot string
	otelSink     *otelsink.Sink
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

	otelSink = otelsink.New()
	otelPort := otelSink.Start()
	fmt.Fprintf(os.Stderr, "e2e: OTLP sink at port %d\n", otelPort)

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
		OtelPort    int
	}
	cfgPath := e2etest.WriteEnvoyConfig("transit-observability-e2e", envoyConfigTmpl, tmplData{
		ProxyPort:   proxyPort,
		AdminPort:   adminPort,
		BackendPort: backendPort,
		OtelPort:    otelPort,
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
	require.NoError(t, err)
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	require.Equal(t, http.StatusOK, resp.StatusCode, "body: %s", body)
}

// TestGet_withModel_responds200 verifies that x-model header requests pass through.
func TestGet_withModel_responds200(t *testing.T) {
	req, err := http.NewRequest(http.MethodGet, proxyURL+"/", nil)
	require.NoError(t, err)
	req.Header.Set("x-model", "claude-sonnet-4-5")
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)
}

// TestPost_responds200 verifies that POST requests pass through correctly.
func TestPost_responds200(t *testing.T) {
	resp, err := http.Post(proxyURL+"/", "application/json", nil)
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)
}

// ── Trace tests ───────────────────────────────────────────────────────────────

// TestTrace_spanOperation verifies that a span with the correct operation name
// is created for each request.
func TestTrace_spanOperation(t *testing.T) {
	resp, err := http.Get(proxyURL + "/")
	require.NoError(t, err)
	resp.Body.Close()

	require.True(t, waitForSpan(t, func(s *otlptrace.Span) bool {
		return s.Name == "http.request"
	}), "timed out waiting for span with name=http.request")
}

// TestTrace_httpMethodTag verifies that the http.method attribute is set.
func TestTrace_httpMethodTag(t *testing.T) {
	resp, err := http.Get(proxyURL + "/path/to/resource")
	require.NoError(t, err)
	resp.Body.Close()

	require.True(t, waitForSpan(t, func(s *otlptrace.Span) bool {
		return spanHasAttr(s, "http.method", "GET")
	}), "timed out waiting for span with http.method=GET")
}

// TestTrace_httpPathTag verifies that the http.path attribute is set correctly.
func TestTrace_httpPathTag(t *testing.T) {
	resp, err := http.Get(proxyURL + "/test/path")
	require.NoError(t, err)
	resp.Body.Close()

	require.True(t, waitForSpan(t, func(s *otlptrace.Span) bool {
		return spanHasAttr(s, "http.path", "/test/path")
	}), "timed out waiting for span with http.path=/test/path")
}

// TestTrace_llmModelTag_present verifies that llm.model is set when x-model header is sent.
func TestTrace_llmModelTag_present(t *testing.T) {
	req, err := http.NewRequest(http.MethodGet, proxyURL+"/", nil)
	require.NoError(t, err)
	req.Header.Set("x-model", "claude-opus-4-1")
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	resp.Body.Close()

	require.True(t, waitForSpan(t, func(s *otlptrace.Span) bool {
		return spanHasAttr(s, "llm.model", "claude-opus-4-1")
	}), "timed out waiting for span with llm.model=claude-opus-4-1")
}

// TestTrace_llmModelTag_absent verifies that llm.model is not set when x-model header is absent.
func TestTrace_llmModelTag_absent(t *testing.T) {
	resp, err := http.Get(proxyURL + "/")
	require.NoError(t, err)
	resp.Body.Close()

	require.True(t, waitForSpan(t, func(s *otlptrace.Span) bool {
		return s.Name == "http.request" && !spanHasAttrAny(s, "llm.model")
	}), "timed out waiting for span without llm.model attribute")
}

// TestTrace_httpStatusCodeTag verifies that http.status_code is set on the span.
func TestTrace_httpStatusCodeTag(t *testing.T) {
	resp, err := http.Get(proxyURL + "/")
	require.NoError(t, err)
	resp.Body.Close()

	require.True(t, waitForSpan(t, func(s *otlptrace.Span) bool {
		return spanHasAttr(s, "http.status_code", "200")
	}), "timed out waiting for span with http.status_code=200")
}

// ── Metric tests ──────────────────────────────────────────────────────────────

// TestMetric_requestsCounterIncremented verifies that the requests counter is
// exported via OTLP as "dynamicmodulescustom.observability.requests_total".
// Envoy prepends "dynamicmodulescustom." to every metric defined by a dynamic
// module; the dot-separated name we register ("observability.requests_total")
// becomes the sub-hierarchy within that prefix.
func TestMetric_requestsCounterIncremented(t *testing.T) {
	resp, err := http.Get(proxyURL + "/")
	require.NoError(t, err)
	resp.Body.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	require.True(t, waitForMetric(t, ctx, func(m *otlpmetrics.Metric) bool {
		return metricNamed(m, "dynamicmodulescustom.observability.requests_total")
	}), "timed out waiting for dynamicmodulescustom.observability.requests_total metric")
}

// TestMetric_responsesCounterIncremented verifies that the responses counter is
// exported via OTLP as "dynamicmodulescustom.observability.responses_total".
func TestMetric_responsesCounterIncremented(t *testing.T) {
	resp, err := http.Get(proxyURL + "/")
	require.NoError(t, err)
	resp.Body.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	require.True(t, waitForMetric(t, ctx, func(m *otlpmetrics.Metric) bool {
		return metricNamed(m, "dynamicmodulescustom.observability.responses_total")
	}), "timed out waiting for dynamicmodulescustom.observability.responses_total metric")
}

// ── Log tests ────────────────────────────────────────────────────────────────

// TestLog_statusCodeInMetadata verifies that status_code appears in log records.
func TestLog_statusCodeInMetadata(t *testing.T) {
	resp, err := http.Get(proxyURL + "/")
	require.NoError(t, err)
	resp.Body.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	require.True(t, waitForRecord(t, ctx, func(r *otlplogs.LogRecord) bool {
		return recordHasAttr(r, "status_code", "200")
	}), "timed out waiting for log record with status_code=200")
}

// TestLog_modelPresentWhenHeaderSet verifies that model attribute is set in logs
// when x-model header is sent.
func TestLog_modelPresentWhenHeaderSet(t *testing.T) {
	req, err := http.NewRequest(http.MethodGet, proxyURL+"/", nil)
	require.NoError(t, err)
	req.Header.Set("x-model", "claude-sonnet-4-1")
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	resp.Body.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	require.True(t, waitForRecord(t, ctx, func(r *otlplogs.LogRecord) bool {
		return recordHasAttr(r, "model", "claude-sonnet-4-1")
	}), "timed out waiting for log record with model=claude-sonnet-4-1")
}

// TestLog_modelAbsentWhenNoHeader verifies that log records are created
// when x-model header is not sent. (The model field may be empty or absent.)
func TestLog_modelAbsentWhenNoHeader(t *testing.T) {
	resp, err := http.Get(proxyURL + "/modeltest")
	require.NoError(t, err)
	resp.Body.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	require.True(t, waitForRecord(t, ctx, func(r *otlplogs.LogRecord) bool {
		return recordHasAttr(r, "status_code", "200")
	}), "timed out waiting for log record with status_code=200")
}

// ── helpers ───────────────────────────────────────────────────────────────────

// waitForSpan blocks until a span matching predicate arrives or ctx is
// cancelled. Returns true on match, false on timeout.
func waitForSpan(t *testing.T, predicate func(*otlptrace.Span) bool) bool {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	_, ok := otelSink.WaitForSpan(ctx, predicate)
	return ok
}

// waitForMetric blocks until a metric matching predicate arrives or ctx is
// cancelled. Returns true on match, false on timeout.
func waitForMetric(t *testing.T, ctx context.Context, predicate func(*otlpmetrics.Metric) bool) bool {
	t.Helper()
	_, ok := otelSink.WaitForMetric(ctx, predicate)
	return ok
}

// waitForRecord blocks until a log record matching predicate arrives or ctx is
// cancelled. Returns true on match, false on timeout.
func waitForRecord(t *testing.T, ctx context.Context, predicate func(*otlplogs.LogRecord) bool) bool {
	t.Helper()
	_, ok := otelSink.WaitForRecord(ctx, predicate)
	return ok
}

// spanHasAttr checks if a span has an attribute with the given key and string value.
func spanHasAttr(span *otlptrace.Span, key, value string) bool {
	if span == nil || span.Attributes == nil {
		return false
	}
	for _, kv := range span.Attributes {
		if kv.Key == key {
			if sv := kv.Value.GetStringValue(); sv == value {
				return true
			}
		}
	}
	return false
}

// spanHasAttrAny checks if a span has any attribute with the given key.
func spanHasAttrAny(span *otlptrace.Span, key string) bool {
	if span == nil || span.Attributes == nil {
		return false
	}
	for _, kv := range span.Attributes {
		if kv.Key == key {
			return true
		}
	}
	return false
}

// recordHasAttr checks if a log record has an attribute with the given key and string value.
func recordHasAttr(record *otlplogs.LogRecord, key, value string) bool {
	if record == nil || record.Attributes == nil {
		return false
	}
	for _, kv := range record.Attributes {
		if kv.Key == key {
			if sv := kv.Value.GetStringValue(); sv == value {
				return true
			}
		}
	}
	return false
}

// metricNamed checks if a metric has the given name.
func metricNamed(metric *otlpmetrics.Metric, name string) bool {
	return metric != nil && metric.Name == name
}

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
