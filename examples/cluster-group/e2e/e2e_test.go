// Package e2e runs integration tests for the cluster-group example against a
// real Envoy instance.
//
// The test starts a discovery server and an upstream server, then boots Envoy
// with the discovery-cluster module. It verifies that the cold-start fetch
// populates hosts before the first request, and that background refresh picks
// up a host change while Envoy is running.
package e2e

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

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

	bin := e2etest.EnvoyBin(examplesRoot)
	if _, err := os.Stat(bin); err != nil {
		fmt.Fprintf(os.Stderr, "SKIP: envoy not found at %s (run: make download-envoy)\n", bin)
		os.Exit(0)
	}

	if err := e2etest.CheckSharedLibrary(examplesRoot, "cluster-group", "libcluster-group.so"); err != nil {
		fmt.Fprintf(os.Stderr, "e2e: %v\n", err)
		os.Exit(1)
	}

	upstreamPort, _ := startUpstream("upstream ok")
	discoveryPort := startDiscovery(upstreamPort)

	proxyPort := e2etest.FreePort()
	adminPort := e2etest.FreePort()
	proxyURL = fmt.Sprintf("http://127.0.0.1:%d", proxyPort)

	cfgPath := e2etest.WriteEnvoyConfig("cluster-group", envoyConfigTmpl, map[string]int{
		"ProxyPort":     proxyPort,
		"DiscoveryPort": discoveryPort,
		"AdminPort":     adminPort,
	})

	clusterGroupDir := filepath.Join(examplesRoot, "cluster-group")
	stop, ok := e2etest.StartEnvoy(bin, cfgPath, clusterGroupDir, adminPort, nil)
	if !ok {
		os.Exit(1)
	}
	fmt.Fprintln(os.Stderr, "e2e: envoy ready")

	code := m.Run()
	stop()
	os.Exit(code)
}

// TestColdStartFetch verifies that the synchronous ServerInitialized fetch
// populates hosts before the first request arrives.
func TestColdStartFetch(t *testing.T) {
	resp, err := http.Get(proxyURL + "/") //nolint:noctx
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)
	body, _ := io.ReadAll(resp.Body)
	require.Contains(t, string(body), "upstream ok")
}

// TestBackgroundRefresh verifies that when the discovery server starts
// returning a new upstream, the cluster picks it up within the refresh
// interval (200 ms) without restarting Envoy.
func TestBackgroundRefresh(t *testing.T) {
	// Start a second upstream and tell the discovery server to advertise it.
	newPort, newBody := startUpstream("upstream v2")
	switchDiscovery(newPort)

	// Wait up to 2 s for the cluster to refresh and start routing to the new host.
	require.Eventually(t, func() bool {
		resp, err := http.Get(proxyURL + "/") //nolint:noctx
		if err != nil {
			return false
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return string(b) == newBody
	}, 2*time.Second, 100*time.Millisecond, "cluster did not refresh to new upstream within 2s")
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

// currentPort holds the upstream port the discovery server advertises. Swapped
// atomically by switchDiscovery to simulate a discovery update.
var currentPort atomic.Int32

// startDiscovery starts an HTTP server that returns the current upstream port
// as JSON, and returns the port it bound to.
func startDiscovery(initialUpstreamPort int) int {
	currentPort.Store(int32(initialUpstreamPort))
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		panic("startDiscovery: " + err.Error())
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/hosts", func(w http.ResponseWriter, _ *http.Request) {
		port := currentPort.Load()
		body, _ := json.Marshal(map[string]any{
			"hosts": []string{fmt.Sprintf("127.0.0.1:%d", port)},
		})
		w.Header().Set("Content-Type", "application/json")
		w.Write(body) //nolint:errcheck
	})
	go http.Serve(l, mux) //nolint:errcheck
	return l.Addr().(*net.TCPAddr).Port
}

// switchDiscovery atomically updates the port the discovery server advertises.
func switchDiscovery(port int) {
	currentPort.Store(int32(port))
}

// startUpstream starts an HTTP server that returns responseBody for all
// requests. Returns (port, body).
func startUpstream(responseBody string) (int, string) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		panic("startUpstream: " + err.Error())
	}
	mux := http.NewServeMux()
	body := responseBody
	mux.HandleFunc("/", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		io.WriteString(w, body) //nolint:errcheck
	})
	go http.Serve(l, mux) //nolint:errcheck
	return l.Addr().(*net.TCPAddr).Port, body
}
