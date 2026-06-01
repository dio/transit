// Package e2e runs integration tests for the cluster-async-router example
// against a real Envoy instance.
package e2e

import (
	"bytes"
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

	if err := e2etest.CheckSharedLibrary(examplesRoot, "cluster-async-router", "libcluster-async-router.so"); err != nil {
		fmt.Fprintf(os.Stderr, "e2e: %v\n", err)
		os.Exit(1)
	}

	upstreamAPort = startUpstream("upstream-a")
	upstreamBPort = startUpstream("upstream-b")

	proxyPort := e2etest.FreePort()
	adminPort := e2etest.FreePort()
	proxyURL = fmt.Sprintf("http://127.0.0.1:%d", proxyPort)

	cfgPath := e2etest.WriteEnvoyConfig("cluster-async-router", envoyConfigTmpl, map[string]int{
		"ProxyPort":     proxyPort,
		"UpstreamAPort": upstreamAPort,
		"UpstreamBPort": upstreamBPort,
		"AdminPort":     adminPort,
	})

	exampleDir := filepath.Join(examplesRoot, "cluster-async-router")
	stop, ok := e2etest.StartEnvoy(bin, cfgPath, exampleDir, adminPort, nil)
	if !ok {
		os.Exit(1)
	}
	fmt.Fprintln(os.Stderr, "e2e: envoy ready")

	code := m.Run()
	stop()
	os.Exit(code)
}

func post(t *testing.T, body string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, proxyURL+"/", bytes.NewBufferString(body))
	require.NoError(t, err)
	req.Header.Set("content-type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	return resp
}

func TestPost_bodyTargetsA(t *testing.T) {
	resp := post(t, `{"target":"a"}`)
	defer resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)
	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	require.Contains(t, string(body), "upstream-a")
}

func TestPost_bodyTargetsB(t *testing.T) {
	resp := post(t, `{"target":"b"}`)
	defer resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)
	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	require.Contains(t, string(body), "upstream-b")
}

func TestPost_unknownTarget_fails(t *testing.T) {
	resp := post(t, `{"target":"nope"}`)
	defer resp.Body.Close()
	require.NotEqual(t, http.StatusOK, resp.StatusCode)
}

func TestPost_missingTarget_fails(t *testing.T) {
	resp := post(t, `{}`)
	defer resp.Body.Close()
	require.NotEqual(t, http.StatusOK, resp.StatusCode)
}

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
