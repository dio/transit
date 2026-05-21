// Package e2e runs integration tests for the sse-tap filter against a real
// Envoy instance.
//
// TestMain builds libsse-tap.so, starts an in-process SSE upstream, starts Envoy
// with the filter loaded, runs all tests, then tears everything down.
//
// Prerequisites:
//   - Envoy binary at .bin/envoy in the transit root (run: make download-envoy)
//     or set ENVOY_BIN.
//
// Run:
//
//	make e2e-sse-tap
//
// Or directly (from the examples/ directory):
//
//	ENVOY_BIN=../.bin/envoy GOWORK=off go test ./sse-tap/e2e/... -v -timeout=60s
//
// Set TRANSIT_SKIP_BUILD=1 to reuse an already-compiled .so.
package e2e

import (
	"bufio"
	"fmt"
	"io"
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
)

var (
	proxyURL     string
	adminURL     string
	envoyCmd     *exec.Cmd
	examplesRoot string
)

func TestMain(m *testing.M) {
	_, file, _, _ := runtime.Caller(0)
	// sse-tap/e2e/e2e_test.go → examples/
	examplesRoot = filepath.Join(filepath.Dir(file), "../../")

	bin := envoyBin()
	if _, err := os.Stat(bin); err != nil {
		fmt.Fprintf(os.Stderr, "SKIP: envoy not found at %s (run: make download-envoy)\n", bin)
		os.Exit(0)
	}

	sseTapDir := filepath.Join(examplesRoot, "sse-tap")
	soPath := filepath.Join(sseTapDir, "libsse-tap.so")

	if os.Getenv("TRANSIT_SKIP_BUILD") == "" {
		fmt.Fprintln(os.Stderr, "e2e: building libsse-tap.so ...")
		cmd := exec.Command("go", "build", "-trimpath", "-buildmode=c-shared",
			"-o", soPath, "./sse-tap/cmd")
		cmd.Dir = examplesRoot
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
		fmt.Fprintln(os.Stderr, "e2e: reusing existing libsse-tap.so (TRANSIT_SKIP_BUILD=1)")
	}

	upstreamPort := startUpstream()
	proxyPort := freePort()
	adminPort := freePort()
	proxyURL = fmt.Sprintf("http://127.0.0.1:%d", proxyPort)
	adminURL = fmt.Sprintf("http://127.0.0.1:%d", adminPort)

	cfgPath := writeEnvoyConfig(filepath.Join(sseTapDir, "e2e"), map[string]int{
		"ProxyPort":    proxyPort,
		"UpstreamPort": upstreamPort,
		"AdminPort":    adminPort,
	})

	envoyCmd = exec.Command(bin, "-c", cfgPath, "--log-level", "warning",
		"--component-log-level", "dynamic_modules:info")
	envoyCmd.Env = append(os.Environ(),
		"GODEBUG=cgocheck=0",
		"ENVOY_DYNAMIC_MODULES_SEARCH_PATH="+sseTapDir,
	)
	envoyCmd.Stdout = os.Stderr
	envoyCmd.Stderr = os.Stderr
	if err := envoyCmd.Start(); err != nil {
		os.Remove(cfgPath)
		fmt.Fprintf(os.Stderr, "e2e: envoy start failed: %v\n", err)
		os.Exit(1)
	}
	fmt.Fprintf(os.Stderr, "e2e: envoy pid=%d\n", envoyCmd.Process.Pid)

	if !waitURL(adminURL+"/ready", 15*time.Second) {
		envoyCmd.Process.Kill()
		envoyCmd.Wait()
		os.Remove(cfgPath)
		fmt.Fprintln(os.Stderr, "e2e: envoy not ready in time")
		os.Exit(1)
	}
	fmt.Fprintln(os.Stderr, "e2e: envoy ready")

	code := m.Run()

	envoyCmd.Process.Kill()
	envoyCmd.Wait()
	os.Remove(cfgPath)
	os.Exit(code)
}

// TestGet_tapHeaderInjected verifies that the filter injects x-sse-tap: 1 on
// every upstream request. The upstream echoes it back as x-upstream-sse-tap.
func TestGet_tapHeaderInjected(t *testing.T) {
	resp, err := http.Get(proxyURL + "/echo")
	if err != nil {
		t.Fatalf("GET /echo: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("want 200, got %d", resp.StatusCode)
	}
	got := resp.Header.Get("x-upstream-sse-tap")
	if got != "1" {
		t.Fatalf("x-upstream-sse-tap: want %q, got %q", "1", got)
	}
}

// TestSSE_anthropicPassthrough verifies that an Anthropic SSE stream passes
// through the filter without modification.
func TestSSE_anthropicPassthrough(t *testing.T) {
	resp, err := http.Get(proxyURL + "/sse/anthropic")
	if err != nil {
		t.Fatalf("GET /sse/anthropic: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("want 200, got %d", resp.StatusCode)
	}

	body, _ := io.ReadAll(resp.Body)
	s := string(body)
	if !strings.Contains(s, "message_start") {
		t.Fatalf("response body does not contain 'message_start': %q", s)
	}
	if !strings.Contains(s, "message_delta") {
		t.Fatalf("response body does not contain 'message_delta': %q", s)
	}
}

// TestSSE_openaiPassthrough verifies that an OpenAI SSE stream passes through
// the filter without modification.
func TestSSE_openaiPassthrough(t *testing.T) {
	resp, err := http.Get(proxyURL + "/sse/openai")
	if err != nil {
		t.Fatalf("GET /sse/openai: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("want 200, got %d", resp.StatusCode)
	}

	body, _ := io.ReadAll(resp.Body)
	s := string(body)
	if !strings.Contains(s, "prompt_tokens") {
		t.Fatalf("response body does not contain 'prompt_tokens': %q", s)
	}
	if !strings.Contains(s, "[DONE]") {
		t.Fatalf("response body does not contain '[DONE]': %q", s)
	}
}

// TestSSE_countersRecorded checks that Envoy records non-zero token counters
// for SSE responses via the admin stats endpoint.
func TestSSE_countersRecorded(t *testing.T) {
	// Drive a request so counters are incremented.
	resp, err := http.Get(proxyURL + "/sse/anthropic")
	if err != nil {
		t.Fatalf("GET /sse/anthropic: %v", err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()

	// Poll until the counter appears in admin stats.
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if v := readStat(t, "sse_tap_input_tokens"); v > 0 {
			t.Logf("sse_tap_input_tokens: %d", v)
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	// Dump all stats containing "sse_tap" to help diagnose key format.
	if resp2, err2 := http.Get(adminURL + "/stats?filter=sse_tap"); err2 == nil { //nolint:noctx
		body2, _ := io.ReadAll(resp2.Body)
		resp2.Body.Close()
		t.Logf("all sse_tap stats:\n%s", body2)
	}
	t.Fatal("timed out waiting for sse_tap_input_tokens > 0 in admin stats")
}

// ── helpers ───────────────────────────────────────────────────────────────────

// startUpstream starts a minimal in-process HTTP server and returns its port.
// Routes:
//
//	GET /echo          → echoes x-sse-tap header back as x-upstream-sse-tap
//	GET /sse/anthropic → Anthropic-format SSE stream
//	GET /sse/openai    → OpenAI-format SSE stream
func startUpstream() int {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		panic("startUpstream: " + err.Error())
	}
	mux := http.NewServeMux()

	mux.HandleFunc("/echo", func(w http.ResponseWriter, r *http.Request) {
		if v := r.Header.Get("x-sse-tap"); v != "" {
			w.Header().Set("x-upstream-sse-tap", v)
		}
		w.WriteHeader(http.StatusOK)
	})

	mux.HandleFunc("/sse/anthropic", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.WriteHeader(http.StatusOK)
		lines := []string{
			"event: message_start",
			`data: {"message":{"usage":{"input_tokens":42}}}`,
			"",
			"event: content_block_delta",
			`data: {"delta":{"text":"Hello"}}`,
			"",
			"event: message_delta",
			`data: {"usage":{"output_tokens":15}}`,
			"",
			"event: message_stop",
			"data: {}",
			"",
		}
		for _, line := range lines {
			fmt.Fprintln(w, line)
		}
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
	})

	mux.HandleFunc("/sse/openai", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.WriteHeader(http.StatusOK)
		lines := []string{
			`data: {"choices":[{"delta":{"content":"Hi"}}]}`,
			"",
			`data: {"choices":[],"usage":{"prompt_tokens":10,"completion_tokens":5}}`,
			"",
			"data: [DONE]",
			"",
		}
		for _, line := range lines {
			fmt.Fprintln(w, line)
		}
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
	})

	go http.Serve(l, mux)
	return l.Addr().(*net.TCPAddr).Port
}

// readStat queries the Envoy admin stats endpoint and returns the value of the
// named counter, or 0 if not found. The name is matched as a suffix of the stat
// key to handle Envoy scoping (e.g. "listener.X.sse_tap_input_tokens: 42").
func readStat(t *testing.T, name string) int64 {
	t.Helper()
	resp, err := http.Get(adminURL + "/stats?filter=" + name) //nolint:noctx
	if err != nil {
		return 0
	}
	defer resp.Body.Close()
	sc := bufio.NewScanner(resp.Body)
	for sc.Scan() {
		line := sc.Text()
		idx := strings.Index(line, ": ")
		if idx < 0 {
			continue
		}
		key := line[:idx]
		if key != name && !strings.HasSuffix(key, "."+name) {
			continue
		}
		t.Logf("admin stat: %s", line)
		var v int64
		fmt.Sscanf(strings.TrimSpace(line[idx+2:]), "%d", &v)
		return v
	}
	return 0
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

func waitURL(url string, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		resp, err := http.Get(url) //nolint:noctx
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return true
			}
		}
		time.Sleep(200 * time.Millisecond)
	}
	return false
}

func writeEnvoyConfig(dir string, ports map[string]int) string {
	tmplBytes, err := os.ReadFile(filepath.Join(dir, "testdata/envoy.yaml.tmpl"))
	if err != nil {
		panic("readTemplate: " + err.Error())
	}
	tmpl := template.Must(template.New("envoy").Parse(string(tmplBytes)))
	f, err := os.CreateTemp("", "transit-sse-tap-e2e-*.yaml")
	if err != nil {
		panic(err)
	}
	if err := tmpl.Execute(f, ports); err != nil {
		panic("template: " + err.Error())
	}
	f.Close()
	return f.Name()
}
