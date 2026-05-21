// Package e2e runs integration tests for the request-ui filter against a real
// Envoy instance.
//
// TestMain builds librequest-ui.so, starts a Go test backend (testserver),
// starts Envoy with the filter loaded, waits for the request-ui HTTP server to
// come up, runs all tests, then tears everything down.
//
// Prerequisites:
//   - Envoy binary at .bin/envoy (run: make download-envoy) or set ENVOY_BIN
//
// Run:
//
//	make e2e
//
// Or directly (from the transit module root):
//
//	ENVOY_BIN=.bin/envoy go test ./examples/request-ui/e2e/... -v -timeout=120s
//
// Set TRANSIT_SKIP_BUILD=1 to reuse an already-compiled .so.
package e2e

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"text/template"
	"time"

	"github.com/dio/transit/examples/request-ui/sink"
)

// ── globals set by TestMain ──────────────────────────────────────────────────

var (
	proxyURL string // Envoy proxy (→ testserver)
	uiURL    string // request-ui HTTP server (records API + SSE + UI HTML)
)

var (
	envoyCmd    *exec.Cmd
	testsvr     *exec.Cmd
	projectRoot string
)

// ── TestMain ─────────────────────────────────────────────────────────────────

func TestMain(m *testing.M) {
	_, file, _, _ := runtime.Caller(0)
	projectRoot = filepath.Join(filepath.Dir(file), "../../../")

	bin := envoyBin()
	if _, err := os.Stat(bin); err != nil {
		fmt.Fprintf(os.Stderr, "SKIP: envoy not found at %s (run: make download-envoy)\n", bin)
		os.Exit(0)
	}

	exampleDir := filepath.Join(projectRoot, "examples/request-ui")
	soPath := filepath.Join(exampleDir, "librequest-ui.so")
	e2eDir := filepath.Join(exampleDir, "e2e")

	// Build the .so.
	if os.Getenv("TRANSIT_SKIP_BUILD") == "" {
		fmt.Fprintln(os.Stderr, "e2e: building librequest-ui.so ...")
		cmd := exec.Command("go", "build", "-trimpath", "-buildmode=c-shared",
			"-o", soPath, "./examples/request-ui/cmd")
		cmd.Dir = projectRoot
		cmd.Env = append(os.Environ(), "CGO_ENABLED=1")
		cmd.Stdout = os.Stderr
		cmd.Stderr = os.Stderr
		if err := cmd.Run(); err != nil {
			fmt.Fprintf(os.Stderr, "e2e: build failed: %v\n", err)
			os.Exit(1)
		}
		fmt.Fprintln(os.Stderr, "e2e: build OK")
	} else {
		if _, err := os.Stat(soPath); err != nil {
			fmt.Fprintf(os.Stderr, "e2e: TRANSIT_SKIP_BUILD=1 but %s not found\n", soPath)
			os.Exit(1)
		}
		fmt.Fprintln(os.Stderr, "e2e: reusing existing librequest-ui.so (TRANSIT_SKIP_BUILD=1)")
	}

	// Build testserver.
	testsvBin := filepath.Join(e2eDir, ".bin", "testserver")
	if err := os.MkdirAll(filepath.Dir(testsvBin), 0o755); err != nil {
		fmt.Fprintf(os.Stderr, "e2e: mkdir: %v\n", err)
		os.Exit(1)
	}
	fmt.Fprintln(os.Stderr, "e2e: building testserver ...")
	buildCmd := exec.Command("go", "build", "-o", testsvBin,
		"./examples/request-ui/e2e/testserver")
	buildCmd.Dir = projectRoot
	buildCmd.Stdout = os.Stderr
	buildCmd.Stderr = os.Stderr
	if err := buildCmd.Run(); err != nil {
		fmt.Fprintf(os.Stderr, "e2e: testserver build failed: %v\n", err)
		os.Exit(1)
	}

	// Pick free ports.
	upstreamPort := freePort()
	proxyPort := freePort()
	adminPort := freePort()
	uiPort := freePort()

	proxyURL = fmt.Sprintf("http://127.0.0.1:%d", proxyPort)
	uiURL = fmt.Sprintf("http://127.0.0.1:%d", uiPort)

	// Start testserver.
	testsvr = exec.Command(testsvBin)
	testsvr.Env = append(os.Environ(),
		fmt.Sprintf("TESTSERVER_ADDR=127.0.0.1:%d", upstreamPort))
	testsvr.Stdout = os.Stderr
	testsvr.Stderr = os.Stderr
	if err := testsvr.Start(); err != nil {
		fmt.Fprintf(os.Stderr, "e2e: testserver start failed: %v\n", err)
		os.Exit(1)
	}
	fmt.Fprintf(os.Stderr, "e2e: testserver pid=%d port=%d\n", testsvr.Process.Pid, upstreamPort)

	if !waitURL(fmt.Sprintf("http://127.0.0.1:%d/health", upstreamPort), 10*time.Second) {
		_ = testsvr.Process.Kill()
		_ = testsvr.Wait()
		fmt.Fprintln(os.Stderr, "e2e: testserver not ready in time")
		os.Exit(1)
	}

	// Write templated Envoy config.
	cfgPath := writeEnvoyConfig(e2eDir, map[string]int{
		"ProxyPort":    proxyPort,
		"UpstreamPort": upstreamPort,
		"AdminPort":    adminPort,
	})

	// Start Envoy.
	envoyCmd = exec.Command(bin, "-c", cfgPath, "--log-level", "warning")
	envoyCmd.Env = append(os.Environ(),
		"GODEBUG=cgocheck=0",
		"REQUI_MODE=memory",
		fmt.Sprintf("REQUI_ADDR=127.0.0.1:%d", uiPort),
		"ENVOY_DYNAMIC_MODULES_SEARCH_PATH="+exampleDir,
	)
	envoyCmd.Stdout = os.Stderr
	envoyCmd.Stderr = os.Stderr
	if err := envoyCmd.Start(); err != nil {
		_ = testsvr.Process.Kill()
		_ = testsvr.Wait()
		_ = os.Remove(cfgPath)
		fmt.Fprintf(os.Stderr, "e2e: envoy start failed: %v\n", err)
		os.Exit(1)
	}
	fmt.Fprintf(os.Stderr, "e2e: envoy pid=%d proxy=:%d\n", envoyCmd.Process.Pid, proxyPort)

	adminReady := fmt.Sprintf("http://127.0.0.1:%d/ready", adminPort)
	if !waitURL(adminReady, 15*time.Second) {
		_ = envoyCmd.Process.Kill()
		_ = envoyCmd.Wait()
		_ = testsvr.Process.Kill()
		_ = testsvr.Wait()
		_ = os.Remove(cfgPath)
		fmt.Fprintln(os.Stderr, "e2e: envoy not ready in time")
		os.Exit(1)
	}
	fmt.Fprintln(os.Stderr, "e2e: envoy ready")

	// Trigger the first OnLog so the request-ui HTTP server starts.
	http.Get(proxyURL + "/health") //nolint:errcheck
	if !waitURL(uiURL+"/", 10*time.Second) {
		_ = envoyCmd.Process.Kill()
		_ = envoyCmd.Wait()
		_ = testsvr.Process.Kill()
		_ = testsvr.Wait()
		_ = os.Remove(cfgPath)
		fmt.Fprintln(os.Stderr, "e2e: request-ui server not ready in time")
		os.Exit(1)
	}
	fmt.Fprintf(os.Stderr, "e2e: request-ui ready at %s\n", uiURL)

	code := m.Run()

	_ = envoyCmd.Process.Kill()
	_ = envoyCmd.Wait()
	_ = testsvr.Process.Kill()
	_ = testsvr.Wait()
	_ = os.Remove(cfgPath)
	os.Exit(code)
}

// ── Tests ─────────────────────────────────────────────────────────────────────

func TestUIServesHTML(t *testing.T) {
	resp, err := http.Get(uiURL + "/")
	if err != nil {
		t.Fatalf("GET /: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != 200 {
		t.Fatalf("status %d", resp.StatusCode)
	}
	ct := resp.Header.Get("Content-Type")
	if !strings.Contains(ct, "text/html") {
		t.Fatalf("expected text/html, got %q", ct)
	}
}

func TestAPIRequestsArray(t *testing.T) {
	recs := mustFetchRecords(t, nil)
	if recs == nil {
		t.Fatal("expected non-nil slice")
	}
}

func TestProxyGETRecorded(t *testing.T) {
	runID := fmt.Sprintf("test-%d", time.Now().UnixNano())
	path := "/" + runID + "/api/hello"

	resp, err := http.Get(proxyURL + path)
	if err != nil {
		t.Fatalf("proxy GET: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("proxy returned %d, want 200", resp.StatusCode)
	}

	rec := pollRecord(t, runID, func(r *sink.Record) bool {
		return r.Path == path && r.Method == "GET"
	})

	if rec.ResponseCode != 200 {
		t.Errorf("response_code want 200, got %v", rec.ResponseCode)
	}
	if rec.RequestID == "" {
		t.Error("request_id should be populated by Envoy")
	}
	if rec.DurationMs < 0 {
		t.Errorf("duration_ms should be non-negative, got %v", rec.DurationMs)
	}
}

func TestProxyPOSTRecorded(t *testing.T) {
	runID := fmt.Sprintf("test-%d", time.Now().UnixNano())
	path := "/" + runID + "/api/create"

	resp, err := http.Post(proxyURL+path, "application/json",
		strings.NewReader(`{"x":1}`))
	if err != nil {
		t.Fatalf("proxy POST: %v", err)
	}
	_ = resp.Body.Close()

	rec := pollRecord(t, runID, func(r *sink.Record) bool {
		return r.Path == path && r.Method == "POST"
	})

	if rec.ResponseCode != 201 {
		t.Errorf("response_code want 201, got %v", rec.ResponseCode)
	}
}

func TestProxy5xxHasError(t *testing.T) {
	runID := fmt.Sprintf("test-%d", time.Now().UnixNano())
	path := "/" + runID + "/api/error"

	resp, err := http.Get(proxyURL + path)
	if err != nil {
		t.Fatalf("proxy GET: %v", err)
	}
	_ = resp.Body.Close()

	rec := pollRecord(t, runID, func(r *sink.Record) bool {
		return r.Path == path && r.ResponseCode >= 500
	})

	if !rec.HasError {
		t.Errorf("has_error should be true for 5xx response")
	}
}

func TestErrorsFilter(t *testing.T) {
	// Ensure at least one error record exists (from TestProxy5xxHasError or earlier).
	recs := mustFetchRecords(t, map[string]string{"errors": "1"})
	for _, r := range recs {
		if !r.HasError {
			t.Errorf("errors=1 returned non-error record: %+v", r)
		}
	}
}

func TestQSearch(t *testing.T) {
	runID := fmt.Sprintf("test-%d", time.Now().UnixNano())
	path := "/" + runID + "/search-probe"

	resp, err := http.Get(proxyURL + path)
	if err != nil {
		t.Fatalf("proxy GET: %v", err)
	}
	_ = resp.Body.Close()

	// Poll until the record appears.
	pollRecord(t, runID, func(r *sink.Record) bool {
		return strings.Contains(r.Path, runID)
	})

	// Verify the search filter works.
	recs := mustFetchRecords(t, map[string]string{"q": runID})
	if len(recs) == 0 {
		t.Fatal("search returned no records for runID")
	}
	for _, r := range recs {
		if !strings.Contains(r.Path, runID) && !strings.Contains(r.RequestID, runID) {
			t.Errorf("search returned unrelated record: %+v", r)
		}
	}
}

func TestSSEDelivery(t *testing.T) {
	runID := fmt.Sprintf("test-%d", time.Now().UnixNano())
	path := "/" + runID + "/sse-probe"

	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()

	// Start SSE subscription before sending the request.
	req, _ := http.NewRequestWithContext(ctx, "GET", uiURL+"/api/stream", nil)
	sseResp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("SSE connect: %v", err)
	}
	defer func() { _ = sseResp.Body.Close() }()

	// Small delay so the SSE subscription is established.
	time.Sleep(50 * time.Millisecond)

	// Send a request through the proxy.
	go http.Get(proxyURL + path) //nolint:errcheck

	// Read SSE events until we find the one for our path.
	scanner := bufio.NewScanner(sseResp.Body)
	for scanner.Scan() {
		line := scanner.Text()
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		var rec sink.Record
		if err := json.Unmarshal([]byte(line[6:]), &rec); err != nil {
			continue
		}
		if rec.ID > 0 && rec.RequestID != "" {
			return // at least one valid record received via SSE
		}
	}
	if err := scanner.Err(); err != nil && ctx.Err() == nil {
		t.Fatalf("SSE read error: %v", err)
	}
	if ctx.Err() != nil {
		t.Fatal("timed out waiting for SSE event")
	}
}

func TestResponseHeadersRecorded(t *testing.T) {
	runID := fmt.Sprintf("test-%d", time.Now().UnixNano())
	path := "/" + runID + "/headers-check"

	resp, err := http.Get(proxyURL + path)
	if err != nil {
		t.Fatalf("proxy GET: %v", err)
	}
	_ = resp.Body.Close()

	rec := pollRecord(t, runID, func(r *sink.Record) bool {
		return r.Path == path
	})

	if rec.ResponseHeaders == "" {
		t.Fatal("response_headers should be recorded")
	}
	var headers [][2]string
	if err := json.Unmarshal([]byte(rec.ResponseHeaders), &headers); err != nil {
		t.Fatalf("response_headers not valid JSON: %v", err)
	}
	hasStatus := false
	for _, h := range headers {
		if h[0] == ":status" {
			hasStatus = true
			break
		}
	}
	if !hasStatus {
		t.Error("response_headers should include :status pseudo-header")
	}
}

func TestSinceParam(t *testing.T) {
	// Get current max id.
	before := mustFetchRecords(t, map[string]string{"limit": "1"})
	var sinceID int64
	if len(before) > 0 {
		sinceID = before[0].ID
	}

	runID := fmt.Sprintf("test-%d", time.Now().UnixNano())
	path := "/" + runID + "/since-probe"
	resp, err := http.Get(proxyURL + path)
	if err != nil {
		t.Fatalf("proxy GET: %v", err)
	}
	_ = resp.Body.Close()

	pollRecord(t, runID, func(r *sink.Record) bool {
		return r.Path == path
	})

	newRecs := mustFetchRecords(t, map[string]string{
		"since": fmt.Sprintf("%d", sinceID),
	})
	if len(newRecs) == 0 {
		t.Fatal("since= should return new records")
	}
	for _, r := range newRecs {
		if r.ID <= sinceID {
			t.Errorf("record id %d should be > %d", r.ID, sinceID)
		}
	}
}

// ── Helpers ───────────────────────────────────────────────────────────────────

func envoyBin() string {
	if b := os.Getenv("ENVOY_BIN"); b != "" {
		return b
	}
	return filepath.Join(projectRoot, ".bin/envoy")
}

func freePort() int {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		panic("freePort: " + err.Error())
	}
	defer func() { _ = l.Close() }()
	return l.Addr().(*net.TCPAddr).Port
}

func writeEnvoyConfig(e2eDir string, ports map[string]int) string {
	tmplPath := filepath.Join(e2eDir, "testdata/envoy.yaml.tmpl")
	tmplBytes, err := os.ReadFile(tmplPath)
	if err != nil {
		panic("readTemplate: " + err.Error())
	}
	tmpl := template.Must(template.New("envoy").Parse(string(tmplBytes)))

	f, err := os.CreateTemp("", "requi-envoy-*.yaml")
	if err != nil {
		panic("createTemp: " + err.Error())
	}
	if err := tmpl.Execute(f, ports); err != nil {
		_ = f.Close()
		_ = os.Remove(f.Name())
		panic("template: " + err.Error())
	}
	_ = f.Close()
	return f.Name()
}

// waitURL polls url every 200ms until it returns 200 or the deadline expires.
func waitURL(url string, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		resp, err := http.Get(url)
		if err == nil {
			_ = resp.Body.Close()
			if resp.StatusCode == 200 {
				return true
			}
		}
		time.Sleep(200 * time.Millisecond)
	}
	return false
}

func mustFetchRecords(t *testing.T, params map[string]string) []*sink.Record {
	t.Helper()
	u := uiURL + "/api/requests"
	if len(params) > 0 {
		u += "?"
		sep := ""
		for k, v := range params {
			u += sep + k + "=" + v
			sep = "&"
		}
	}
	resp, err := http.Get(u)
	if err != nil {
		t.Fatalf("GET %s: %v", u, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != 200 {
		t.Fatalf("GET %s: status %d", u, resp.StatusCode)
	}
	var recs []*sink.Record
	if err := json.NewDecoder(resp.Body).Decode(&recs); err != nil {
		t.Fatalf("decode: %v", err)
	}
	return recs
}

// pollRecord polls /api/requests?q=runID every 200ms until match returns true
// or timeout expires.
func pollRecord(t *testing.T, runID string, match func(*sink.Record) bool) *sink.Record {
	t.Helper()
	deadline := time.Now().Add(8 * time.Second)
	for time.Now().Before(deadline) {
		recs := mustFetchRecords(t, map[string]string{"q": runID})
		for _, r := range recs {
			if match(r) {
				return r
			}
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Fatalf("pollRecord: no matching record for runID=%s after 8s", runID)
	return nil
}
