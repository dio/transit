// Package e2e runs integration tests against a real Envoy binary.
//
// TestMain builds a combined .so from all e2e filters (echo, guard, e2e-logger),
// starts the access log sink, starts Envoy, and tears everything down when done.
//
// Prerequisites:
//   - Envoy binary at .bin/envoy (run: make download-envoy) or set ENVOY_BIN
//
// Run:
//
//	make e2e
//
// Or manually:
//
//	ENVOY_BIN=.bin/envoy go test ./e2e/... -v -timeout=90s
//
// Tests skip automatically when ENVOY_BIN is not present.
// Set TRANSIT_SKIP_BUILD=1 to reuse a previously built .so (faster iteration).
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
	"testing"
	"time"

	"github.com/dio/transit/e2e/accessloggersink"
)

var (
	echoAddr         string
	guardAddr        string
	accessLoggerAddr string
	correlatorAddr   string
	adminAddr        string
)

var (
	envoyCmd    *exec.Cmd
	projectRoot string
)

func TestMain(m *testing.M) {
	_, file, _, _ := runtime.Caller(0)
	projectRoot = filepath.Dir(file)

	bin := envoyBin()
	if _, err := os.Stat(bin); err != nil {
		fmt.Fprintf(os.Stderr, "SKIP: envoy not found at %s (run: make download-envoy)\n", bin)
		os.Exit(0)
	}

	echoPort := freePort()
	guardPort := freePort()
	accessLoggerPort := freePort()
	correlatorPort := freePort()
	adminPort := freePort()

	echoAddr = fmt.Sprintf("http://localhost:%d", echoPort)
	guardAddr = fmt.Sprintf("http://localhost:%d", guardPort)
	accessLoggerAddr = fmt.Sprintf("http://localhost:%d", accessLoggerPort)
	correlatorAddr = fmt.Sprintf("http://localhost:%d", correlatorPort)
	adminAddr = fmt.Sprintf("http://localhost:%d", adminPort)

	sinkURL := accessloggersink.StartSink()
	fmt.Fprintf(os.Stderr, "e2e: access logger sink at %s\n", sinkURL)

	soPath := filepath.Join(projectRoot, "libe2e.so")

	if os.Getenv("TRANSIT_SKIP_BUILD") == "" {
		fmt.Fprintln(os.Stderr, "e2e: building libe2e.so ...")
		cmd := exec.Command("go", "build", "-trimpath", "-buildmode=c-shared", "-o", soPath, "./cmd")
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
			fmt.Fprintf(os.Stderr, "e2e: TRANSIT_SKIP_BUILD=1 but libe2e.so not found at %s\n", soPath)
			os.Exit(1)
		}
		fmt.Fprintln(os.Stderr, "e2e: reusing existing libe2e.so (TRANSIT_SKIP_BUILD=1)")
	}

	cfgPath := writeEnvoyConfig(sinkURL, echoPort, guardPort, accessLoggerPort, correlatorPort, adminPort)

	envoyCmd = exec.Command(bin,
		"-c", cfgPath,
		"--log-level", "warning",
		"--component-log-level", "dynamic_modules:info",
	)
	envoyCmd.Env = append(os.Environ(),
		"GODEBUG=cgocheck=0",
		"ENVOY_DYNAMIC_MODULES_SEARCH_PATH="+projectRoot,
	)
	envoyCmd.Stdout = os.Stderr
	envoyCmd.Stderr = os.Stderr

	if err := envoyCmd.Start(); err != nil {
		os.Remove(cfgPath)
		fmt.Fprintf(os.Stderr, "e2e: envoy start failed: %v\n", err)
		os.Exit(1)
	}
	fmt.Fprintf(os.Stderr, "e2e: envoy pid=%d\n", envoyCmd.Process.Pid)

	if !waitReady(15 * time.Second) {
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

func envoyBin() string {
	if b := os.Getenv("ENVOY_BIN"); b != "" {
		return b
	}
	return filepath.Join(projectRoot, "..", ".bin", "envoy")
}

// freePort asks the OS for an unused TCP port and returns its number.
// There is an inherent TOCTOU gap between closing the listener and Envoy
// binding the port, but in practice this is reliable in isolated test
// environments.
func freePort() int {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		panic("freePort: " + err.Error())
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}

func waitReady(timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		resp, err := http.Get(adminAddr + "/ready")
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

func writeEnvoyConfig(sinkURL string, echoPort, guardPort, accessLoggerPort, correlatorPort, adminPort int) string {
	cfg := fmt.Sprintf(`
static_resources:
  listeners:
    - name: echo
      address:
        socket_address: { address: 0.0.0.0, port_value: %d }
      filter_chains:
        - filters:
            - name: envoy.filters.network.http_connection_manager
              typed_config:
                "@type": type.googleapis.com/envoy.extensions.filters.network.http_connection_manager.v3.HttpConnectionManager
                stat_prefix: echo
                http_filters:
                  - name: echo
                    typed_config:
                      "@type": type.googleapis.com/envoy.extensions.filters.http.dynamic_modules.v3.DynamicModuleFilter
                      dynamic_module_config:
                        name: e2e
                      filter_name: echo
                  - name: envoy.filters.http.router
                    typed_config:
                      "@type": type.googleapis.com/envoy.extensions.filters.http.router.v3.Router
                route_config:
                  name: echo
                  virtual_hosts:
                    - name: local
                      domains: ["*"]
                      routes:
                        - match: { prefix: "/" }
                          direct_response:
                            status: 200
                            body: { inline_string: "echo ok" }

    - name: guard
      address:
        socket_address: { address: 0.0.0.0, port_value: %d }
      filter_chains:
        - filters:
            - name: envoy.filters.network.http_connection_manager
              typed_config:
                "@type": type.googleapis.com/envoy.extensions.filters.network.http_connection_manager.v3.HttpConnectionManager
                stat_prefix: guard
                http_filters:
                  - name: guard
                    typed_config:
                      "@type": type.googleapis.com/envoy.extensions.filters.http.dynamic_modules.v3.DynamicModuleFilter
                      dynamic_module_config:
                        name: e2e
                      filter_name: guard
                  - name: envoy.filters.http.router
                    typed_config:
                      "@type": type.googleapis.com/envoy.extensions.filters.http.router.v3.Router
                route_config:
                  name: guard
                  virtual_hosts:
                    - name: local
                      domains: ["*"]
                      routes:
                        - match: { prefix: "/" }
                          direct_response:
                            status: 200
                            body: { inline_string: "guard ok" }

    - name: access-logger-e2e
      address:
        socket_address: { address: 0.0.0.0, port_value: %d }
      filter_chains:
        - filters:
            - name: envoy.filters.network.http_connection_manager
              typed_config:
                "@type": type.googleapis.com/envoy.extensions.filters.network.http_connection_manager.v3.HttpConnectionManager
                stat_prefix: access_logger_e2e
                access_log:
                  - name: envoy.access_loggers.dynamic_modules
                    typed_config:
                      "@type": type.googleapis.com/envoy.extensions.access_loggers.dynamic_modules.v3.DynamicModuleAccessLog
                      dynamic_module_config:
                        name: e2e
                      logger_name: e2e-logger
                      logger_config:
                        "@type": type.googleapis.com/google.protobuf.StringValue
                        value: '{"sink_url":"%s"}'
                http_filters:
                  - name: envoy.filters.http.router
                    typed_config:
                      "@type": type.googleapis.com/envoy.extensions.filters.http.router.v3.Router
                route_config:
                  name: access_logger_e2e
                  virtual_hosts:
                    - name: local
                      domains: ["*"]
                      routes:
                        - match: { prefix: "/" }
                          direct_response:
                            status: 200
                            body: { inline_string: "access-logger-ok" }

    - name: correlator-e2e
      address:
        socket_address: { address: 0.0.0.0, port_value: %d }
      filter_chains:
        - filters:
            - name: envoy.filters.network.http_connection_manager
              typed_config:
                "@type": type.googleapis.com/envoy.extensions.filters.network.http_connection_manager.v3.HttpConnectionManager
                stat_prefix: correlator_e2e
                access_log:
                  - name: envoy.access_loggers.dynamic_modules
                    typed_config:
                      "@type": type.googleapis.com/envoy.extensions.access_loggers.dynamic_modules.v3.DynamicModuleAccessLog
                      dynamic_module_config:
                        name: e2e
                      logger_name: e2e-correlator-logger
                      logger_config:
                        "@type": type.googleapis.com/google.protobuf.StringValue
                        value: '{"sink_url":"%s"}'
                http_filters:
                  - name: e2e-correlator
                    typed_config:
                      "@type": type.googleapis.com/envoy.extensions.filters.http.dynamic_modules.v3.DynamicModuleFilter
                      dynamic_module_config:
                        name: e2e
                      filter_name: e2e-correlator
                  - name: envoy.filters.http.router
                    typed_config:
                      "@type": type.googleapis.com/envoy.extensions.filters.http.router.v3.Router
                route_config:
                  name: correlator_e2e
                  virtual_hosts:
                    - name: local
                      domains: ["*"]
                      routes:
                        - match: { prefix: "/" }
                          direct_response:
                            status: 200
                            body: { inline_string: "correlator-ok" }

admin:
  address:
    socket_address: { address: 127.0.0.1, port_value: %d }
`, echoPort, guardPort, accessLoggerPort, sinkURL, correlatorPort, sinkURL, adminPort)

	f, err := os.CreateTemp("", "transit-e2e-*.yaml")
	if err != nil {
		panic(err)
	}
	f.WriteString(cfg)
	f.Close()
	return f.Name()
}

// helpers

func mustDo(t *testing.T, req *http.Request) *http.Response {
	t.Helper()
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	return resp
}

func readBody(t *testing.T, resp *http.Response) string {
	t.Helper()
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	return string(b)
}
