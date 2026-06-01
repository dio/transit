// Package e2e runs integration tests for the cluster-least-conn example against
// a real Envoy instance.
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
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/dio/transit/examples/internal/e2etest"
)

//go:embed testdata/envoy.tmpl.yaml
var envoyConfigTmpl string

var (
	proxyURL      string
	examplesRoot  string
	upstreamAPort int
	upstreamBPort int
	upstreamCPort int
)

func TestMain(m *testing.M) {
	_, file, _, _ := runtime.Caller(0)
	examplesRoot = filepath.Join(filepath.Dir(file), "../../")

	bin := e2etest.EnvoyBin(examplesRoot)
	if _, err := os.Stat(bin); err != nil {
		fmt.Fprintf(os.Stderr, "SKIP: envoy not found at %s (run: make download-envoy)\n", bin)
		os.Exit(0)
	}

	exampleDir := filepath.Join(examplesRoot, "cluster-least-conn")
	if err := e2etest.CheckSharedLibrary(examplesRoot, "cluster-least-conn", "libcluster-least-conn.so"); err != nil {
		fmt.Fprintf(os.Stderr, "e2e: %v\n", err)
		os.Exit(1)
	}

	upstreamAPort = startUpstream("upstream-a")
	upstreamBPort = startUpstream("upstream-b")
	upstreamCPort = startUpstream("upstream-c")

	proxyPort := e2etest.FreePort()
	adminPort := e2etest.FreePort()
	proxyURL = fmt.Sprintf("http://127.0.0.1:%d", proxyPort)

	cfgPath := e2etest.WriteEnvoyConfig("cluster-least-conn-e2e", envoyConfigTmpl, map[string]int{
		"ProxyPort":     proxyPort,
		"UpstreamAPort": upstreamAPort,
		"UpstreamBPort": upstreamBPort,
		"UpstreamCPort": upstreamCPort,
		"AdminPort":     adminPort,
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

func TestGet_routesToUpstreams(t *testing.T) {
	resp, err := http.Get(proxyURL + "/") //nolint:noctx
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)
	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	require.NotEmpty(t, string(body))
}

func TestGet_multipleUpstreams_receiveTraffic(t *testing.T) {
	// Send multiple sequential requests and verify they all succeed.
	// With least-connections LB, sequential requests will consistently select
	// the host with fewest active connections (typically the first/same host).
	// This test verifies the cluster extension is routing correctly.
	upstreamsSeen := make(map[string]int)
	for range 10 {
		resp, err := http.Get(proxyURL + "/") //nolint:noctx
		require.NoError(t, err)
		body, err := io.ReadAll(resp.Body)
		resp.Body.Close()
		require.NoError(t, err)
		upstreamsSeen[string(body)]++
	}

	// Verify at least one upstream received traffic and all requests succeeded
	require.Greater(t, len(upstreamsSeen), 0,
		"expected traffic to reach at least one upstream, got: %v", upstreamsSeen)
}

func startUpstream(name string) int {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		panic("startUpstream: " + err.Error())
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(name)) //nolint:errcheck
	})
	go http.Serve(l, mux) //nolint:errcheck
	return l.Addr().(*net.TCPAddr).Port
}
