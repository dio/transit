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
	_ "embed"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
	"text/template"
	"time"

	"github.com/dio/transit/e2e/sinks/accessloggersink"
	"github.com/dio/transit/e2e/sinks/alssink"
	"github.com/dio/transit/e2e/sinks/otelsink"
)

//go:embed testdata/envoy.yaml.tmpl
var envoyConfigTmpl string

var (
	echoAddr              string
	guardAddr             string
	accessLoggerAddr      string
	correlatorAddr        string
	bodyAddr              string
	mutableBodyAddr       string
	compressAddr          string
	metadataAddr          string
	tracerAddr            string
	alsAddr               string
	upstreamFilterAddr    string
	upstreamAuthAddr      string
	upstreamAuthGroupAddr string
	lbPolicyAddr          string
	adminAddr             string
)

var otelSink *otelsink.Sink
var alsSink *alssink.Sink

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
	compressPort := freePort()
	compressUpstreamPort := startGzipUpstream()
	metadataPort := freePort()
	tracerPort := freePort()
	alsPort := freePort()
	upstreamFilterPort := freePort()
	upstreamAuthPort := freePort()
	upstreamAuthGroupPort := freePort()
	upstreamFilterUpstreamPort := startPlainUpstream()
	lbPolicyPort := freePort()
	adminPort := freePort()

	echoAddr = fmt.Sprintf("http://localhost:%d", echoPort)
	guardAddr = fmt.Sprintf("http://localhost:%d", guardPort)
	accessLoggerAddr = fmt.Sprintf("http://localhost:%d", accessLoggerPort)
	correlatorAddr = fmt.Sprintf("http://localhost:%d", correlatorPort)
	bodyAddr = fmt.Sprintf("http://localhost:%d", bodyPort)
	mutableBodyAddr = fmt.Sprintf("http://localhost:%d", mutableBodyPort)
	compressAddr = fmt.Sprintf("http://localhost:%d", compressPort)
	metadataAddr = fmt.Sprintf("http://localhost:%d", metadataPort)
	tracerAddr = fmt.Sprintf("http://localhost:%d", tracerPort)
	alsAddr = fmt.Sprintf("http://localhost:%d", alsPort)
	upstreamFilterAddr = fmt.Sprintf("http://localhost:%d", upstreamFilterPort)
	upstreamAuthAddr = fmt.Sprintf("http://localhost:%d", upstreamAuthPort)
	upstreamAuthGroupAddr = fmt.Sprintf("http://localhost:%d", upstreamAuthGroupPort)
	lbPolicyAddr = fmt.Sprintf("http://localhost:%d", lbPolicyPort)
	adminAddr = fmt.Sprintf("http://localhost:%d", adminPort)

	otelSink = otelsink.New()
	otelSinkPort := otelSink.Start()
	fmt.Fprintf(os.Stderr, "e2e: OTLP sink at port %d\n", otelSinkPort)

	alsSink = alssink.New()
	alsSinkPort := alsSink.Start()
	fmt.Fprintf(os.Stderr, "e2e: ALS sink at port %d\n", alsSinkPort)

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
		SinkURL:                    sinkURL,
		EchoPort:                   echoPort,
		GuardPort:                  guardPort,
		AccessLoggerPort:           accessLoggerPort,
		CorrelatorPort:             correlatorPort,
		BodyPort:                   bodyPort,
		MutableBodyPort:            mutableBodyPort,
		CompressPort:               compressPort,
		CompressUpstreamPort:       compressUpstreamPort,
		OtelSinkPort:               otelSinkPort,
		MetadataPort:               metadataPort,
		TracerPort:                 tracerPort,
		AlsPort:                    alsPort,
		AlsSinkPort:                alsSinkPort,
		UpstreamFilterPort:         upstreamFilterPort,
		UpstreamAuthPort:           upstreamAuthPort,
		UpstreamAuthGroupPort:      upstreamAuthGroupPort,
		UpstreamFilterUpstreamPort: upstreamFilterUpstreamPort,
		LbPolicyPort:               lbPolicyPort,
		LbPolicyUpstreamPort:       upstreamFilterUpstreamPort,
		AdminPort:                  adminPort,
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

// startPlainUpstream starts a minimal HTTP server that always returns 200 with
// body "upstream ok". Returns the port it is listening on.
func startPlainUpstream() int {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		panic("startPlainUpstream: " + err.Error())
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if auth := r.Header.Get("Authorization"); auth != "" {
			w.Header().Set("x-received-authorization", auth)
		}
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("upstream ok"))
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
	SinkURL                    string
	EchoPort                   int
	GuardPort                  int
	AccessLoggerPort           int
	CorrelatorPort             int
	BodyPort                   int
	MutableBodyPort            int
	CompressPort               int
	CompressUpstreamPort       int
	OtelSinkPort               int
	MetadataPort               int
	TracerPort                 int
	AlsPort                    int
	AlsSinkPort                int
	UpstreamFilterPort         int
	UpstreamAuthPort           int
	UpstreamAuthGroupPort      int
	UpstreamFilterUpstreamPort int
	LbPolicyPort               int
	LbPolicyUpstreamPort       int
	AdminPort                  int
}

func writeEnvoyConfig(p envoyPorts) string {
	tmpl := template.Must(template.New("envoy").Parse(envoyConfigTmpl))
	var buf bytes.Buffer
	if err := tmpl.Execute(&buf, p); err != nil {
		panic("writeEnvoyConfig: " + err.Error())
	}
	f, err := os.CreateTemp("", "transit-e2e-*.yaml")
	if err != nil {
		panic(err)
	}
	buf.WriteTo(f) //nolint:errcheck
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
