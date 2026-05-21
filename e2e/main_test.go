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
	"bytes"
	"compress/gzip"
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

	"github.com/dio/transit/e2e/sinks/accessloggersink"
	"github.com/dio/transit/e2e/sinks/otelsink"
)

var (
	echoAddr         string
	guardAddr        string
	accessLoggerAddr string
	correlatorAddr   string
	bodyAddr         string
	mutableBodyAddr  string
	codecAddr        string
	metadataAddr     string
	tracerAddr       string
	adminAddr        string
)

var otelSink *otelsink.Sink

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
	bodyPort := freePort()
	mutableBodyPort := freePort()
	codecPort := freePort()
	codecUpstreamPort := startGzipUpstream()
	metadataPort := freePort()
	tracerPort := freePort()
	adminPort := freePort()

	echoAddr = fmt.Sprintf("http://localhost:%d", echoPort)
	guardAddr = fmt.Sprintf("http://localhost:%d", guardPort)
	accessLoggerAddr = fmt.Sprintf("http://localhost:%d", accessLoggerPort)
	correlatorAddr = fmt.Sprintf("http://localhost:%d", correlatorPort)
	bodyAddr = fmt.Sprintf("http://localhost:%d", bodyPort)
	mutableBodyAddr = fmt.Sprintf("http://localhost:%d", mutableBodyPort)
	codecAddr = fmt.Sprintf("http://localhost:%d", codecPort)
	metadataAddr = fmt.Sprintf("http://localhost:%d", metadataPort)
	tracerAddr = fmt.Sprintf("http://localhost:%d", tracerPort)
	adminAddr = fmt.Sprintf("http://localhost:%d", adminPort)

	otelSink = otelsink.New()
	otelSinkPort := otelSink.Start()
	fmt.Fprintf(os.Stderr, "e2e: OTLP sink at port %d\n", otelSinkPort)

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

	cfgPath := writeEnvoyConfig(envoyPorts{
		sinkURL:           sinkURL,
		echoPort:          echoPort,
		guardPort:         guardPort,
		accessLoggerPort:  accessLoggerPort,
		correlatorPort:    correlatorPort,
		bodyPort:          bodyPort,
		mutableBodyPort:   mutableBodyPort,
		codecPort:         codecPort,
		codecUpstreamPort: codecUpstreamPort,
		otelSinkPort:      otelSinkPort,
		metadataPort:      metadataPort,
		tracerPort:        tracerPort,
		adminPort:         adminPort,
	})

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

// startGzipUpstream starts a minimal HTTP server that always returns the text
// "hello codec" compressed with gzip, regardless of Accept-Encoding. Returns
// the port it is listening on.
func startGzipUpstream() int {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		panic("startGzipUpstream: " + err.Error())
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, _ *http.Request) {
		var buf bytes.Buffer
		gz := gzip.NewWriter(&buf)
		gz.Write([]byte("hello codec"))
		gz.Close()
		w.Header().Set("Content-Encoding", "gzip")
		w.Header().Set("Content-Type", "text/plain")
		w.WriteHeader(http.StatusOK)
		w.Write(buf.Bytes())
	})
	go http.Serve(l, mux) //nolint:errcheck
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

type envoyPorts struct {
	sinkURL           string
	echoPort          int
	guardPort         int
	accessLoggerPort  int
	correlatorPort    int
	bodyPort          int
	mutableBodyPort   int
	codecPort         int
	codecUpstreamPort int
	otelSinkPort      int
	metadataPort      int
	tracerPort        int
	adminPort         int
}

func writeEnvoyConfig(p envoyPorts) string {
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

    - name: body-e2e
      address:
        socket_address: { address: 0.0.0.0, port_value: %d }
      filter_chains:
        - filters:
            - name: envoy.filters.network.http_connection_manager
              typed_config:
                "@type": type.googleapis.com/envoy.extensions.filters.network.http_connection_manager.v3.HttpConnectionManager
                stat_prefix: body_e2e
                http_filters:
                  - name: e2e-body
                    typed_config:
                      "@type": type.googleapis.com/envoy.extensions.filters.http.dynamic_modules.v3.DynamicModuleFilter
                      dynamic_module_config:
                        name: e2e
                      filter_name: e2e-body
                  - name: envoy.filters.http.router
                    typed_config:
                      "@type": type.googleapis.com/envoy.extensions.filters.http.router.v3.Router
                route_config:
                  name: body_e2e
                  virtual_hosts:
                    - name: local
                      domains: ["*"]
                      routes:
                        - match: { prefix: "/" }
                          direct_response:
                            status: 200
                            body: { inline_string: "body-ok" }

    - name: mutable-body-e2e
      address:
        socket_address: { address: 0.0.0.0, port_value: %d }
      filter_chains:
        - filters:
            - name: envoy.filters.network.http_connection_manager
              typed_config:
                "@type": type.googleapis.com/envoy.extensions.filters.network.http_connection_manager.v3.HttpConnectionManager
                stat_prefix: mutable_body_e2e
                http_filters:
                  - name: e2e-mutable-body
                    typed_config:
                      "@type": type.googleapis.com/envoy.extensions.filters.http.dynamic_modules.v3.DynamicModuleFilter
                      dynamic_module_config:
                        name: e2e
                      filter_name: e2e-mutable-body
                  - name: envoy.filters.http.router
                    typed_config:
                      "@type": type.googleapis.com/envoy.extensions.filters.http.router.v3.Router
                route_config:
                  name: mutable_body_e2e
                  virtual_hosts:
                    - name: local
                      domains: ["*"]
                      routes:
                        - match: { prefix: "/" }
                          direct_response:
                            status: 200
                            body: { inline_string: "body-mutable-ok" }

    - name: codec-e2e
      address:
        socket_address: { address: 0.0.0.0, port_value: %d }
      filter_chains:
        - filters:
            - name: envoy.filters.network.http_connection_manager
              typed_config:
                "@type": type.googleapis.com/envoy.extensions.filters.network.http_connection_manager.v3.HttpConnectionManager
                stat_prefix: codec_e2e
                http_filters:
                  - name: e2e-codec
                    typed_config:
                      "@type": type.googleapis.com/envoy.extensions.filters.http.dynamic_modules.v3.DynamicModuleFilter
                      dynamic_module_config:
                        name: e2e
                      filter_name: e2e-codec
                  - name: envoy.filters.http.router
                    typed_config:
                      "@type": type.googleapis.com/envoy.extensions.filters.http.router.v3.Router
                route_config:
                  name: codec_e2e
                  virtual_hosts:
                    - name: local
                      domains: ["*"]
                      routes:
                        - match: { prefix: "/" }
                          route: { cluster: codec-upstream }

    - name: metadata-e2e
      address:
        socket_address: { address: 0.0.0.0, port_value: %d }
      filter_chains:
        - filters:
            - name: envoy.filters.network.http_connection_manager
              typed_config:
                "@type": type.googleapis.com/envoy.extensions.filters.network.http_connection_manager.v3.HttpConnectionManager
                stat_prefix: metadata_e2e
                access_log:
                  - name: envoy.access_loggers.open_telemetry
                    typed_config:
                      "@type": type.googleapis.com/envoy.extensions.access_loggers.open_telemetry.v3.OpenTelemetryAccessLogConfig
                      grpc_service:
                        envoy_grpc:
                          cluster_name: otel-collector
                      log_name: e2e-otel
                      body:
                        string_value: "%%DYNAMIC_METADATA(e2e:custom_field)%%"
                      attributes:
                        values:
                          - key: method
                            value:
                              string_value: "%%DYNAMIC_METADATA(e2e:method)%%"
                http_filters:
                  - name: e2e-metadata
                    typed_config:
                      "@type": type.googleapis.com/envoy.extensions.filters.http.dynamic_modules.v3.DynamicModuleFilter
                      dynamic_module_config:
                        name: e2e
                      filter_name: e2e-metadata
                  - name: envoy.filters.http.router
                    typed_config:
                      "@type": type.googleapis.com/envoy.extensions.filters.http.router.v3.Router
                route_config:
                  name: metadata_e2e
                  virtual_hosts:
                    - name: local
                      domains: ["*"]
                      routes:
                        - match: { prefix: "/" }
                          direct_response:
                            status: 200
                            body: { inline_string: "metadata-ok" }

    - name: tracer-e2e
      address:
        socket_address: { address: 0.0.0.0, port_value: %d }
      filter_chains:
        - filters:
            - name: envoy.filters.network.http_connection_manager
              typed_config:
                "@type": type.googleapis.com/envoy.extensions.filters.network.http_connection_manager.v3.HttpConnectionManager
                stat_prefix: tracer_e2e
                generate_request_id: true
                tracing:
                  client_sampling: { value: 100 }
                  random_sampling: { value: 100 }
                  overall_sampling: { value: 100 }
                http_filters:
                  - name: e2e-tracer
                    typed_config:
                      "@type": type.googleapis.com/envoy.extensions.filters.http.dynamic_modules.v3.DynamicModuleFilter
                      dynamic_module_config:
                        name: e2e
                      filter_name: e2e-tracer
                  - name: envoy.filters.http.router
                    typed_config:
                      "@type": type.googleapis.com/envoy.extensions.filters.http.router.v3.Router
                route_config:
                  name: tracer_e2e
                  virtual_hosts:
                    - name: local
                      domains: ["*"]
                      routes:
                        - match: { prefix: "/" }
                          direct_response:
                            status: 200
                            body: { inline_string: "tracer-ok" }

  clusters:
    - name: codec-upstream
      connect_timeout: 5s
      type: STATIC
      load_assignment:
        cluster_name: codec-upstream
        endpoints:
          - lb_endpoints:
              - endpoint:
                  address:
                    socket_address: { address: 127.0.0.1, port_value: %d }

    - name: otel-collector
      connect_timeout: 5s
      type: STATIC
      typed_extension_protocol_options:
        envoy.extensions.upstreams.http.v3.HttpProtocolOptions:
          "@type": type.googleapis.com/envoy.extensions.upstreams.http.v3.HttpProtocolOptions
          explicit_http_config:
            http2_protocol_options: {}
      load_assignment:
        cluster_name: otel-collector
        endpoints:
          - lb_endpoints:
              - endpoint:
                  address:
                    socket_address: { address: 127.0.0.1, port_value: %d }

admin:
  address:
    socket_address: { address: 127.0.0.1, port_value: %d }

stats_flush_interval: 1s
stats_sinks:
  - name: envoy.stat_sinks.open_telemetry
    typed_config:
      "@type": type.googleapis.com/envoy.extensions.stat_sinks.open_telemetry.v3.SinkConfig
      grpc_service:
        envoy_grpc:
          cluster_name: otel-collector
      report_counters_as_deltas: false

tracing:
  http:
    name: envoy.tracers.opentelemetry
    typed_config:
      "@type": type.googleapis.com/envoy.config.trace.v3.OpenTelemetryConfig
      grpc_service:
        envoy_grpc:
          cluster_name: otel-collector
      service_name: "transit-e2e"
`, p.echoPort, p.guardPort, p.accessLoggerPort, p.sinkURL, p.correlatorPort, p.sinkURL, p.bodyPort, p.mutableBodyPort, p.codecPort, p.metadataPort, p.tracerPort, p.codecUpstreamPort, p.otelSinkPort, p.adminPort)

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
