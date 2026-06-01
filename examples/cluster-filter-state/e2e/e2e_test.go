// Package e2e runs integration tests for the cluster-filter-state example
// against a real Envoy instance.
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
)

func TestMain(m *testing.M) {
	_, file, _, _ := runtime.Caller(0)
	examplesRoot = filepath.Join(filepath.Dir(file), "../../")

	bin := e2etest.EnvoyBin(examplesRoot)
	if _, err := os.Stat(bin); err != nil {
		fmt.Fprintf(os.Stderr, "SKIP: envoy not found at %s (run: make download-envoy)\n", bin)
		os.Exit(0)
	}

	err := e2etest.CheckSharedLibrary(examplesRoot, "cluster-filter-state", "libcluster-filter-state.so")
	if err != nil {
		fmt.Fprintf(os.Stderr, "e2e: %v\n", err)
		os.Exit(1)
	}

	upstreamAPort = startUpstream("upstream-a")
	upstreamBPort = startUpstream("upstream-b")

	proxyPort := e2etest.FreePort()
	adminPort := e2etest.FreePort()
	proxyURL = fmt.Sprintf("http://127.0.0.1:%d", proxyPort)

	cfgPath := e2etest.WriteEnvoyConfig("cluster-filter-state", envoyConfigTmpl, map[string]int{
		"ProxyPort":     proxyPort,
		"UpstreamAPort": upstreamAPort,
		"UpstreamBPort": upstreamBPort,
		"AdminPort":     adminPort,
	})

	exampleDir := filepath.Join(examplesRoot, "cluster-filter-state")
	stop, ok := e2etest.StartEnvoy(bin, cfgPath, exampleDir, adminPort, nil)
	if !ok {
		os.Exit(1)
	}
	fmt.Fprintln(os.Stderr, "e2e: envoy ready")

	code := m.Run()
	stop()
	os.Exit(code)
}

func TestGet_filterStateRoutingToUpstreamA(t *testing.T) {
	// Filter state routing: x-target-host header writes filter state,
	// cluster extension reads it and selects matching host.
	req, err := http.NewRequest(http.MethodGet, proxyURL+"/", nil)
	require.NoError(t, err)
	req.Header.Set("x-target-host", fmt.Sprintf("127.0.0.1:%d", upstreamAPort))

	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()

	require.Equal(t, http.StatusOK, resp.StatusCode)
	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	require.Contains(t, string(body), "upstream-a")
}

func TestGet_filterStateRoutingToUpstreamB(t *testing.T) {
	// Filter state routing: x-target-host pointing to upstream B.
	// Proves filter state is read and matched correctly.
	req, err := http.NewRequest(http.MethodGet, proxyURL+"/", nil)
	require.NoError(t, err)
	req.Header.Set("x-target-host", fmt.Sprintf("127.0.0.1:%d", upstreamBPort))

	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()

	require.Equal(t, http.StatusOK, resp.StatusCode)
	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	require.Contains(t, string(body), "upstream-b")
}

func TestGet_emptyHeader_routesToDefault(t *testing.T) {
	// Empty x-target-host header: filter state is set but empty,
	// cluster extension falls back to first host (upstream A).
	req, err := http.NewRequest(http.MethodGet, proxyURL+"/", nil)
	require.NoError(t, err)
	req.Header.Set("x-target-host", "")

	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()

	require.Equal(t, http.StatusOK, resp.StatusCode)
	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	require.Contains(t, string(body), "upstream-a")
}

func TestGet_noHeader_routesToDefault(t *testing.T) {
	// No x-target-host header: filter state not set,
	// cluster extension falls back to first host (upstream A).
	resp, err := http.Get(proxyURL + "/") //nolint:noctx
	require.NoError(t, err)
	defer resp.Body.Close()

	require.Equal(t, http.StatusOK, resp.StatusCode)
	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	require.Contains(t, string(body), "upstream-a")
}

func TestGet_unmappedTarget_routesToDefault(t *testing.T) {
	// x-target-host points to unmapped address: no match in cluster config,
	// cluster extension falls back to first host (upstream A).
	req, err := http.NewRequest(http.MethodGet, proxyURL+"/", nil)
	require.NoError(t, err)
	req.Header.Set("x-target-host", "192.168.1.99:9999")

	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()

	require.Equal(t, http.StatusOK, resp.StatusCode)
	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	require.Contains(t, string(body), "upstream-a")
}

// startUpstream starts a minimal HTTP server that returns name in the body.
func startUpstream(name string) int {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		panic("startUpstream: " + err.Error())
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, name)
	})
	go http.Serve(l, mux) //nolint:errcheck
	return l.Addr().(*net.TCPAddr).Port
}
