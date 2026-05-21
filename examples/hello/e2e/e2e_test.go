// Package e2e runs integration tests for the hello filter against a real Envoy instance.
//
// TestMain builds libhello.so, starts Envoy with the filter loaded, runs all
// tests, then tears everything down.
//
// Prerequisites:
//   - Envoy binary at .bin/envoy in the transit root (run: make download-envoy)
//     or set ENVOY_BIN.
//
// Run:
//
//	make e2e-hello
//
// Or directly (from the examples/ directory):
//
//	ENVOY_BIN=../.bin/envoy GOWORK=off go test ./hello/e2e/... -v -timeout=60s
//
// Set TRANSIT_SKIP_BUILD=1 to reuse an already-compiled .so.
package e2e

import (
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
	envoyCmd     *exec.Cmd
	examplesRoot string
)

func TestMain(m *testing.M) {
	_, file, _, _ := runtime.Caller(0)
	// hello/e2e/e2e_test.go → examples/
	examplesRoot = filepath.Join(filepath.Dir(file), "../../")

	bin := envoyBin()
	if _, err := os.Stat(bin); err != nil {
		fmt.Fprintf(os.Stderr, "SKIP: envoy not found at %s (run: make download-envoy)\n", bin)
		os.Exit(0)
	}

	helloDir := filepath.Join(examplesRoot, "hello")
	soPath := filepath.Join(helloDir, "libhello.so")

	if os.Getenv("TRANSIT_SKIP_BUILD") == "" {
		fmt.Fprintln(os.Stderr, "e2e: building libhello.so ...")
		cmd := exec.Command("go", "build", "-trimpath", "-buildmode=c-shared",
			"-o", soPath, "./hello/cmd")
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
		fmt.Fprintln(os.Stderr, "e2e: reusing existing libhello.so (TRANSIT_SKIP_BUILD=1)")
	}

	proxyPort := freePort()
	adminPort := freePort()
	proxyURL = fmt.Sprintf("http://127.0.0.1:%d", proxyPort)

	cfgPath := writeEnvoyConfig(filepath.Join(helloDir, "e2e"), map[string]int{
		"ProxyPort": proxyPort,
		"AdminPort": adminPort,
	})

	envoyCmd = exec.Command(bin, "-c", cfgPath, "--log-level", "warning",
		"--component-log-level", "dynamic_modules:info")
	envoyCmd.Env = append(os.Environ(),
		"GODEBUG=cgocheck=0",
		"ENVOY_DYNAMIC_MODULES_SEARCH_PATH="+helloDir,
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
	os.Remove(cfgPath)
	os.Exit(code)
}

func TestGet_returns200(t *testing.T) {
	resp, err := http.Get(proxyURL + "/")
	if err != nil {
		t.Fatalf("GET /: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("want 200, got %d", resp.StatusCode)
	}
}

func TestGet_bodyContent(t *testing.T) {
	resp, err := http.Get(proxyURL + "/")
	if err != nil {
		t.Fatalf("GET /: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), "hello") {
		t.Fatalf("body %q does not contain 'hello'", body)
	}
}

func TestPost_returns200(t *testing.T) {
	resp, err := http.Post(proxyURL+"/", "application/json", strings.NewReader(`{}`))
	if err != nil {
		t.Fatalf("POST /: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("want 200, got %d", resp.StatusCode)
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

func writeEnvoyConfig(dir string, ports map[string]int) string {
	tmplBytes, err := os.ReadFile(filepath.Join(dir, "testdata/envoy.yaml.tmpl"))
	if err != nil {
		panic("readTemplate: " + err.Error())
	}
	tmpl := template.Must(template.New("envoy").Parse(string(tmplBytes)))
	f, err := os.CreateTemp("", "transit-hello-e2e-*.yaml")
	if err != nil {
		panic(err)
	}
	if err := tmpl.Execute(f, ports); err != nil {
		panic("template: " + err.Error())
	}
	f.Close()
	return f.Name()
}
