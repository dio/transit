// Package e2e runs integration tests for the body-transform filter against a real Envoy instance.
//
// TestMain starts a plain HTTP upstream that echoes the request body, then starts
// Envoy with the Makefile-built filter loaded, runs all tests, then tears everything down.
//
// Prerequisites:
//   - Envoy binary at .bin/envoy in the transit root (run: make download-envoy)
//     or set ENVOY_BIN.
//
// Run:
//
//	make -C examples/body-transform e2e
//
// Or directly (from the examples/ directory):
//
//	ENVOY_BIN=../.bin/envoy GOWORK=off go test ./body-transform/e2e/... -v -timeout=60s
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
	"strings"
	"testing"
	"text/template"
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
	// body-transform/e2e/e2e_test.go → examples/
	examplesRoot = filepath.Join(filepath.Dir(file), "../../")

	bin := envoyBin()
	if _, err := os.Stat(bin); err != nil {
		fmt.Fprintf(os.Stderr, "SKIP: envoy not found at %s (run: make download-envoy)\n", bin)
		os.Exit(0)
	}

	btDir := filepath.Join(examplesRoot, "body-transform")
	if err := e2etest.CheckSharedLibrary(examplesRoot, "body-transform", "libbody-transform.so"); err != nil {
		fmt.Fprintf(os.Stderr, "e2e: %v\n", err)
		os.Exit(1)
	}

	// Start a plain HTTP upstream that echoes the request body.
	upstreamPort := freePort()
	upstreamMux := http.NewServeMux()
	upstreamMux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		w.Header().Set("content-type", r.Header.Get("content-type"))
		w.WriteHeader(http.StatusOK)
		w.Write(body) //nolint:errcheck
	})
	upstreamServer := &http.Server{
		Addr:    fmt.Sprintf("127.0.0.1:%d", upstreamPort),
		Handler: upstreamMux,
	}
	upstreamListener, err := net.Listen("tcp", upstreamServer.Addr)
	if err != nil {
		fmt.Fprintf(os.Stderr, "e2e: upstream listen failed: %v\n", err)
		os.Exit(1)
	}
	go upstreamServer.Serve(upstreamListener) //nolint:errcheck

	proxyPort := freePort()
	adminPort := freePort()
	proxyURL = fmt.Sprintf("http://127.0.0.1:%d", proxyPort)

	cfgPath := writeEnvoyConfig(map[string]int{
		"ProxyPort":    proxyPort,
		"AdminPort":    adminPort,
		"UpstreamPort": upstreamPort,
	})

	envoyCmd = exec.Command(bin, "-c", cfgPath, "--log-level", "warning",
		"--component-log-level", "dynamic_modules:info")
	envoyCmd.Env = append(os.Environ(),
		"GODEBUG=cgocheck=0",
		"ENVOY_DYNAMIC_MODULES_SEARCH_PATH="+btDir,
	)
	envoyCmd.Stdout = os.Stderr
	envoyCmd.Stderr = os.Stderr
	if err := envoyCmd.Start(); err != nil {
		os.Remove(cfgPath)
		fmt.Fprintf(os.Stderr, "e2e: envoy start failed: %v\n", err)
		os.Exit(1)
	}
	fmt.Fprintf(os.Stderr, "e2e: envoy pid=%d\n", envoyCmd.Process.Pid)

	if !waitURL(fmt.Sprintf("http://127.0.0.1:%d/ready", adminPort), 15*time.Second) {
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
	upstreamServer.Close()
	os.Remove(cfgPath)
	os.Exit(code)
}

func TestPost_jsonBody_messageRenamedToText(t *testing.T) {
	resp, err := http.Post(proxyURL+"/", "application/json", strings.NewReader(`{"message":"hello"}`))
	if err != nil {
		t.Fatalf("POST /: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("want 200, got %d", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	got := strings.TrimSpace(string(body))
	if got != `{"text":"hello"}` {
		t.Fatalf("want {\"text\":\"hello\"}, got %q", got)
	}
}

func TestPost_nonJSON_passedThrough(t *testing.T) {
	resp, err := http.Post(proxyURL+"/", "text/plain", strings.NewReader("hello"))
	if err != nil {
		t.Fatalf("POST /: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("want 200, got %d", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	got := strings.TrimSpace(string(body))
	if got != "hello" {
		t.Fatalf("want hello, got %q", got)
	}
}

// ── helpers ───────────────────────────────────────────────────────────────────

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

func writeEnvoyConfig(ports map[string]int) string {
	tmpl := template.Must(template.New("envoy").Parse(envoyConfigTmpl))
	f, err := os.CreateTemp("", "transit-body-transform-e2e-*.yaml")
	if err != nil {
		panic(err)
	}
	if err := tmpl.Execute(f, ports); err != nil {
		panic("template: " + err.Error())
	}
	f.Close()
	return f.Name()
}
