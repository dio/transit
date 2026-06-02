// Package e2e tests L1 → L2 routing through a single Envoy process with
// multiple static listeners. Requires RUN_TIERED_SINGLE_ENVOY_E2E=1.
package e2e

import (
	_ "embed"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/dio/transit/examples/internal/e2etest"
)

//go:embed testdata/envoy.tmpl.yaml
var envoyConfigTmpl string

var l1URL string

func TestMain(m *testing.M) {
	if os.Getenv("RUN_TIERED_SINGLE_ENVOY_E2E") != "1" {
		fmt.Fprintln(os.Stderr, "SKIP: set RUN_TIERED_SINGLE_ENVOY_E2E=1 to run")
		os.Exit(0)
	}

	_, file, _, _ := runtime.Caller(0)
	examplesRoot := filepath.Join(filepath.Dir(file), "../../")

	bin := e2etest.EnvoyBin(examplesRoot)
	if _, err := os.Stat(bin); err != nil {
		fmt.Fprintf(os.Stderr, "SKIP: envoy not found at %s (run: make download-envoy)\n", bin)
		os.Exit(0)
	}

	if err := e2etest.CheckSharedLibrary(examplesRoot, "cluster-shard-router", "libcluster-shard-router.so"); err != nil {
		fmt.Fprintf(os.Stderr, "e2e: %v\n", err)
		os.Exit(1)
	}
	if err := e2etest.CheckSharedLibrary(examplesRoot, "cluster-router", "libcluster-router.so"); err != nil {
		fmt.Fprintf(os.Stderr, "e2e: %v\n", err)
		os.Exit(1)
	}

	backendA := httptest.NewServer(backendHandler("a"))
	backendB := httptest.NewServer(backendHandler("b"))

	adminPort := e2etest.FreePort()
	l1Port := e2etest.FreePort()
	l2aPort := e2etest.FreePort()
	l2bPort := e2etest.FreePort()

	l1URL = fmt.Sprintf("http://127.0.0.1:%d", l1Port)

	cfgPath := e2etest.WriteEnvoyConfig("tiered-single-envoy-e2e", envoyConfigTmpl, map[string]any{
		"AdminPort":    adminPort,
		"L1Port":       l1Port,
		"L2APort":      l2aPort,
		"L2BPort":      l2bPort,
		"BackendAPort": serverPort(backendA),
		"BackendBPort": serverPort(backendB),
	})

	searchPath := filepath.Join(examplesRoot, "cluster-shard-router") +
		":" + filepath.Join(examplesRoot, "cluster-router")

	stop, ok := e2etest.StartEnvoy(bin, cfgPath, searchPath, adminPort, nil)
	if !ok {
		os.Exit(1)
	}
	fmt.Fprintln(os.Stderr, "e2e: envoy ready")

	code := m.Run()
	stop()
	os.Exit(code)
}

func backendHandler(id string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Backend-Id", id)
		w.Header().Set("X-Received-Model", r.Header.Get("x-model"))
		w.Header().Set("X-Received-Auth", r.Header.Get("Authorization"))
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, `{"ok":true}`)
	})
}

func serverPort(s *httptest.Server) int {
	u, err := url.Parse(s.URL)
	if err != nil {
		panic("serverPort: " + err.Error())
	}
	port := 0
	fmt.Sscan(u.Port(), &port)
	return port
}

func post(t *testing.T, headers map[string]string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, l1URL+"/v1/chat/completions", nil)
	require.NoError(t, err)
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	return resp
}

func TestTieredSingleEnvoy(t *testing.T) {
	t.Run("gate1-shard-a", func(t *testing.T) {
		resp := post(t, map[string]string{"x-transit-tag": "a", "x-model": "gpt-fast"})
		defer resp.Body.Close()
		io.Copy(io.Discard, resp.Body) //nolint:errcheck
		require.Equal(t, http.StatusOK, resp.StatusCode)
		require.Equal(t, "a", resp.Header.Get("X-Backend-Id"))
		require.Equal(t, "gpt-fast", resp.Header.Get("X-Received-Model"))
	})

	t.Run("gate1-shard-b", func(t *testing.T) {
		resp := post(t, map[string]string{"x-transit-tag": "b", "x-model": "claude-safe"})
		defer resp.Body.Close()
		io.Copy(io.Discard, resp.Body) //nolint:errcheck
		require.Equal(t, http.StatusOK, resp.StatusCode)
		require.Equal(t, "b", resp.Header.Get("X-Backend-Id"))
		require.Equal(t, "claude-safe", resp.Header.Get("X-Received-Model"))
	})

	t.Run("gate2-auth", func(t *testing.T) {
		resp := post(t, map[string]string{"x-transit-tag": "a", "x-model": "gpt-slow"})
		defer resp.Body.Close()
		io.Copy(io.Discard, resp.Body) //nolint:errcheck
		require.Equal(t, http.StatusOK, resp.StatusCode)
		require.Equal(t, "Bearer token-slow", resp.Header.Get("X-Received-Auth"))
	})

	t.Run("gate3-default-shard", func(t *testing.T) {
		resp := post(t, map[string]string{"x-model": "gpt-fast"})
		defer resp.Body.Close()
		io.Copy(io.Discard, resp.Body) //nolint:errcheck
		require.Equal(t, http.StatusOK, resp.StatusCode)
		require.Equal(t, "a", resp.Header.Get("X-Backend-Id"))
	})

	t.Run("gate4-unknown-model", func(t *testing.T) {
		resp := post(t, map[string]string{"x-transit-tag": "a", "x-model": "does-not-exist"})
		defer resp.Body.Close()
		io.Copy(io.Discard, resp.Body) //nolint:errcheck
		require.NotEqual(t, http.StatusOK, resp.StatusCode)
	})
}
