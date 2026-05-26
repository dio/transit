// Package e2e runs integration tests for the cluster example against a real
// Envoy instance.
package e2e

import (
	_ "embed"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"text/template"

	"github.com/dio/transit/examples/internal/e2etest"
)

//go:embed testdata/envoy.tmpl.yaml
var envoyConfigTmpl string

var (
	proxyURL     string
	examplesRoot string
)

func TestMain(m *testing.M) {
	_, file, _, _ := runtime.Caller(0)
	examplesRoot = filepath.Join(filepath.Dir(file), "../../")

	bin := envoyBin()
	if _, err := os.Stat(bin); err != nil {
		fmt.Fprintf(os.Stderr, "SKIP: envoy not found at %s (run: make download-envoy)\n", bin)
		os.Exit(0)
	}

	clusterDir := filepath.Join(examplesRoot, "cluster")
	if err := e2etest.CheckSharedLibrary(examplesRoot, "cluster", "libcluster.so"); err != nil {
		fmt.Fprintf(os.Stderr, "e2e: %v\n", err)
		os.Exit(1)
	}

	upstreamPort := startUpstream()
	proxyPort := freePort()
	adminPort := freePort()
	proxyURL = fmt.Sprintf("http://127.0.0.1:%d", proxyPort)

	cfgPath := writeEnvoyConfig(map[string]int{
		"ProxyPort":    proxyPort,
		"UpstreamPort": upstreamPort,
		"AdminPort":    adminPort,
	})

	stop, ok := e2etest.StartEnvoy(bin, cfgPath, clusterDir, adminPort, nil)
	if !ok {
		os.Exit(1)
	}
	fmt.Fprintln(os.Stderr, "e2e: envoy ready")

	code := m.Run()
	stop()
	os.Exit(code)
}

func TestGetRoutesThroughCluster(t *testing.T) {
	resp, err := http.Get(proxyURL + "/") //nolint:noctx
	if err != nil {
		t.Fatalf("GET /: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("want 200, got %d", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), "upstream ok") {
		t.Fatalf("body %q does not contain 'upstream ok'", body)
	}
}

func envoyBin() string {
	if b := os.Getenv("ENVOY_BIN"); b != "" {
		return b
	}
	return filepath.Join(examplesRoot, "..", ".bin", "envoy")
}

func startUpstream() int {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		panic("startUpstream: " + err.Error())
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("upstream ok"))
	})
	go http.Serve(l, mux)
	return l.Addr().(*net.TCPAddr).Port
}

func freePort() int {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		panic("freePort: " + err.Error())
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}

func writeEnvoyConfig(data map[string]int) string {
	tmpl := template.Must(template.New("envoy").Parse(envoyConfigTmpl))
	f, err := os.CreateTemp("", "cluster-e2e-*.yaml")
	if err != nil {
		panic(err)
	}
	if err := tmpl.Execute(f, data); err != nil {
		panic(err)
	}
	f.Close()
	return f.Name()
}
