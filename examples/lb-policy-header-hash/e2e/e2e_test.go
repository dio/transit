// Package e2e runs integration tests for the lb-policy-header-hash filter against a real
// Envoy instance.
//
// TestMain starts an in-process upstream, starts Envoy with the Makefile-built
// custom LB policy loaded, runs all tests, then tears everything down.
//
// Prerequisites:
//   - Envoy binary at .bin/envoy in the transit root (run: make download-envoy)
//     or set ENVOY_BIN.
//
// Run:
//
//	make -C examples/lb-policy-header-hash e2e
//
// Or directly (from the examples/ directory):
//
//	ENVOY_BIN=../.bin/envoy GOWORK=off go test ./lb-policy-header-hash/e2e/... -v -timeout=60s
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
	// lb-policy-header-hash/e2e/e2e_test.go → examples/
	examplesRoot = filepath.Join(filepath.Dir(file), "../../")

	bin := e2etest.EnvoyBin(examplesRoot)
	if _, err := os.Stat(bin); err != nil {
		fmt.Fprintf(os.Stderr, "SKIP: envoy not found at %s (run: make download-envoy)\n", bin)
		os.Exit(0)
	}

	lbPolicyHeaderHashDir := filepath.Join(examplesRoot, "lb-policy-header-hash")
	if err := e2etest.CheckSharedLibrary(examplesRoot, "lb-policy-header-hash", "liblb-policy-header-hash.so"); err != nil {
		fmt.Fprintf(os.Stderr, "e2e: %v\n", err)
		os.Exit(1)
	}

	upstreamAPort = startUpstream("upstream-a")
	upstreamBPort = startUpstream("upstream-b")
	upstreamCPort = startUpstream("upstream-c")

	proxyPort := e2etest.FreePort()
	adminPort := e2etest.FreePort()
	proxyURL = fmt.Sprintf("http://127.0.0.1:%d", proxyPort)

	cfgPath := e2etest.WriteEnvoyConfig("lb-policy-header-hash-e2e", envoyConfigTmpl, map[string]int{
		"ProxyPort":     proxyPort,
		"UpstreamAPort": upstreamAPort,
		"UpstreamBPort": upstreamBPort,
		"UpstreamCPort": upstreamCPort,
		"AdminPort":     adminPort,
	})

	stop, ok := e2etest.StartEnvoy(bin, cfgPath, lbPolicyHeaderHashDir, adminPort, nil)
	if !ok {
		os.Exit(1)
	}
	fmt.Fprintln(os.Stderr, "e2e: envoy ready")

	code := m.Run()
	stop()
	os.Exit(code)
}

// TestGet_stickyRouting_sameSessionID verifies that the same x-session-id header
// always routes to the same upstream host (sticky session property).
// Sends multiple requests with the same session ID and verifies responses
// all come from the same upstream.
func TestGet_stickyRouting_sameSessionID(t *testing.T) {
	sessionID := "sticky-user-42"
	var firstUpstream string

	for i := range 5 {
		req, err := http.NewRequest(http.MethodGet, proxyURL+"/", nil)
		require.NoError(t, err)
		req.Header.Set("x-session-id", sessionID)

		resp, err := http.DefaultClient.Do(req)
		require.NoError(t, err)
		body, err := io.ReadAll(resp.Body)
		resp.Body.Close()
		require.NoError(t, err)

		require.Equal(t, http.StatusOK, resp.StatusCode)

		upstream := string(body)
		if i == 0 {
			firstUpstream = upstream
		} else {
			// Verify all requests go to the same upstream
			require.Equal(t, firstUpstream, upstream,
				"session %q should stick to same upstream, request %d routed to different upstream", sessionID, i)
		}
	}
}

// TestGet_differentSessions_differentUpstreams verifies that different x-session-id
// values can route to different upstreams. This proves the hash function
// distributes different sessions across hosts.
func TestGet_differentSessions_differentUpstreams(t *testing.T) {
	sessions := []string{"user-alice", "user-bob", "user-charlie"}
	upstreams := make(map[string]string)

	for _, sessionID := range sessions {
		req, err := http.NewRequest(http.MethodGet, proxyURL+"/", nil)
		require.NoError(t, err)
		req.Header.Set("x-session-id", sessionID)

		resp, err := http.DefaultClient.Do(req)
		require.NoError(t, err)
		body, err := io.ReadAll(resp.Body)
		resp.Body.Close()
		require.NoError(t, err)

		require.Equal(t, http.StatusOK, resp.StatusCode)
		upstreams[sessionID] = string(body)
	}

	// With 3 sessions and 3 upstreams, at least 2 different upstreams should be hit
	// (statistically very likely unless hash is broken)
	uniqueUpstreams := make(map[string]struct{})
	for _, upstream := range upstreams {
		uniqueUpstreams[upstream] = struct{}{}
	}
	require.Greater(t, len(uniqueUpstreams), 1,
		"different sessions should route to different upstreams, but all routed to: %v", upstreams)
}

// TestGet_noHeader_succeeds verifies that a request without x-session-id still succeeds
// (falls back to default index 0).
func TestGet_noHeader_succeeds(t *testing.T) {
	resp, err := http.Get(proxyURL + "/")
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)
}

// ── helpers ───────────────────────────────────────────────────────────────────

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
