// Package e2e runs integration tests for the jwt-callout filter against a real Envoy instance.
//
// TestMain starts Envoy with the Makefile-built filter loaded, a local token-introspection
// stub server, and an upstream echo server, runs all tests, then tears everything down.
//
// Prerequisites:
//   - Envoy binary at .bin/envoy in the transit root (run: make download-envoy)
//     or set ENVOY_BIN.
//
// Run:
//
//	make -C examples/jwt-callout e2e
//
// Or directly (from the examples/ directory):
//
//	ENVOY_BIN=../.bin/envoy GOWORK=off go test ./jwt-callout/e2e/... -v -timeout=60s
package e2e

import (
	_ "embed"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sync"
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

	// introspectionMu guards introspectionBody and introspectionCode.
	introspectionMu   sync.Mutex
	introspectionBody = `{"active":true,"sub":"alice"}`
	introspectionCode = http.StatusOK
)

func TestMain(m *testing.M) {
	_, file, _, _ := runtime.Caller(0)
	// jwt-callout/e2e/e2e_test.go → examples/
	examplesRoot = filepath.Join(filepath.Dir(file), "../../")

	bin := e2etest.EnvoyBin(examplesRoot)
	if _, err := os.Stat(bin); err != nil {
		fmt.Fprintf(os.Stderr, "SKIP: envoy not found at %s (run: make download-envoy)\n", bin)
		os.Exit(0)
	}

	if err := e2etest.CheckSharedLibrary(examplesRoot, "jwt-callout", "libjwt-callout.so"); err != nil {
		fmt.Fprintf(os.Stderr, "e2e: %v\n", err)
		os.Exit(1)
	}

	// Start the stub introspection server on a free port.
	introspectionListener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		fmt.Fprintf(os.Stderr, "e2e: introspection listener: %v\n", err)
		os.Exit(1)
	}
	introspectionPort := introspectionListener.Addr().(*net.TCPAddr).Port
	introspectionServer := &httptest.Server{
		Listener: introspectionListener,
		Config: &http.Server{
			Handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				introspectionMu.Lock()
				body := introspectionBody
				code := introspectionCode
				introspectionMu.Unlock()
				w.WriteHeader(code)
				_, _ = io.WriteString(w, body)
			}),
		},
	}
	introspectionServer.Start()
	defer introspectionServer.Close()

	// Start the upstream echo server on a free port.
	upstreamListener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		fmt.Fprintf(os.Stderr, "e2e: upstream listener: %v\n", err)
		os.Exit(1)
	}
	upstreamPort := upstreamListener.Addr().(*net.TCPAddr).Port
	upstreamServer := &httptest.Server{
		Listener: upstreamListener,
		Config: &http.Server{
			// upstream echoes x-jwt-sub request header as response header
			Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if sub := r.Header.Get("x-jwt-sub"); sub != "" {
					w.Header().Set("x-jwt-sub", sub)
				}
				w.WriteHeader(http.StatusOK)
			}),
		},
	}
	upstreamServer.Start()
	defer upstreamServer.Close()

	proxyPort := e2etest.FreePort()
	adminPort := e2etest.FreePort()
	proxyURL = fmt.Sprintf("http://127.0.0.1:%d", proxyPort)

	type ports struct {
		ProxyPort         int
		AdminPort         int
		IntrospectionPort int
		UpstreamPort      int
	}
	cfgPath := e2etest.WriteEnvoyConfig("transit-jwt-callout-e2e", envoyConfigTmpl, ports{
		ProxyPort:         proxyPort,
		AdminPort:         adminPort,
		IntrospectionPort: introspectionPort,
		UpstreamPort:      upstreamPort,
	})
	defer os.Remove(cfgPath)

	jwtCalloutDir := filepath.Join(examplesRoot, "jwt-callout")
	envoyCmd = exec.Command(bin, "-c", cfgPath, "--log-level", "warning",
		"--component-log-level", "dynamic_modules:info")
	envoyCmd.Env = append(os.Environ(),
		"GODEBUG=cgocheck=0",
		"ENVOY_DYNAMIC_MODULES_SEARCH_PATH="+jwtCalloutDir,
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

// TestValidToken_passes verifies that a valid active token causes the filter to
// forward the request with x-jwt-sub set, which the upstream echoes back.
func TestValidToken_passes(t *testing.T) {
	introspectionMu.Lock()
	introspectionBody = `{"active":true,"sub":"alice"}`
	introspectionCode = http.StatusOK
	introspectionMu.Unlock()

	req, _ := http.NewRequest(http.MethodGet, proxyURL+"/", nil)
	req.Header.Set("Authorization", "Bearer tok")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET /: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("want 200, got %d", resp.StatusCode)
	}
	if got := resp.Header.Get("x-jwt-sub"); got != "alice" {
		t.Fatalf("want x-jwt-sub=alice, got %q", got)
	}
}

// TestInvalidToken_returns401 verifies that an inactive token causes the filter
// to send a 401 local response with "token inactive" in the body.
func TestInvalidToken_returns401(t *testing.T) {
	introspectionMu.Lock()
	introspectionBody = `{"active":false}`
	introspectionCode = http.StatusOK
	introspectionMu.Unlock()

	req, _ := http.NewRequest(http.MethodGet, proxyURL+"/", nil)
	req.Header.Set("Authorization", "Bearer tok")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET /: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("want 401, got %d", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	if string(body) != `{"error":"token inactive"}` {
		t.Fatalf("unexpected body: %s", body)
	}
}
