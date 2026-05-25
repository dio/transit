// Package e2e runs integration tests for the cluster-dfp example against a
// real Envoy instance.
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
	upstreamA    string
	upstreamB    string
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

	exampleDir := filepath.Join(examplesRoot, "cluster-dfp")
	if err := e2etest.CheckSharedLibrary(examplesRoot, "cluster-dfp", "libcluster-dfp.so"); err != nil {
		fmt.Fprintf(os.Stderr, "e2e: %v\n", err)
		os.Exit(1)
	}

	upstreamAPort := startUpstream("upstream a")
	upstreamBPort := startUpstream("upstream b")
	upstreamA = net.JoinHostPort("localhost", fmt.Sprint(upstreamAPort))
	upstreamB = net.JoinHostPort("localhost", fmt.Sprint(upstreamBPort))
	proxyPort := freePort()
	adminPort := freePort()
	proxyURL = fmt.Sprintf("http://127.0.0.1:%d", proxyPort)

	cfgPath := writeEnvoyConfig(envoyConfigData{
		ProxyPort:     proxyPort,
		AdminPort:     adminPort,
		UpstreamAPort: upstreamAPort,
		UpstreamBPort: upstreamBPort,
	})

	stop, ok := e2etest.StartEnvoy(bin, cfgPath, exampleDir, adminPort, nil)
	if !ok {
		os.Exit(1)
	}
	fmt.Fprintln(os.Stderr, "e2e: envoy ready")

	code := m.Run()
	stop()
	os.Exit(code)
}

func TestRoutesToRequestTarget(t *testing.T) {
	requireBody(t, "tiny", "upstream a")
	requireBody(t, "large", "upstream b")
}

func TestReusesDiscoveredHost(t *testing.T) {
	requireBody(t, "tiny", "upstream a")
	requireBody(t, "tiny", "upstream a")
}

func requireBody(t *testing.T, model string, want string) {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, proxyURL+"/", nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("x-model", model)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET / model %s: %v", model, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("model %s: want 200, got %d", model, resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), want) {
		t.Fatalf("model %s: body %q does not contain %q", model, body, want)
	}
}

func envoyBin() string {
	if b := os.Getenv("ENVOY_BIN"); b != "" {
		return b
	}
	return filepath.Join(examplesRoot, "..", ".bin", "envoy")
}

func startUpstream(body string) int {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		panic("startUpstream: " + err.Error())
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(body))
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

type envoyConfigData struct {
	ProxyPort     int
	AdminPort     int
	UpstreamAPort int
	UpstreamBPort int
}

func writeEnvoyConfig(data envoyConfigData) string {
	tmpl := template.Must(template.New("envoy").Parse(envoyConfigTmpl))
	f, err := os.CreateTemp("", "cluster-dfp-e2e-*.yaml")
	if err != nil {
		panic(err)
	}
	if err := tmpl.Execute(f, data); err != nil {
		panic(err)
	}
	f.Close()
	return f.Name()
}

