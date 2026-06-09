// Package e2e runs integration tests for the grpc-callout example against a real Envoy instance.
//
// Run:
//
//	make -C examples/grpc-callout e2e
package e2e

import (
	"context"
	_ "embed"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"

	rlsv3 "github.com/envoyproxy/go-control-plane/envoy/service/ratelimit/v3"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"

	"github.com/dio/transit/examples/internal/e2etest"
)

//go:embed testdata/envoy.tmpl.yaml
var envoyConfigTmpl string

var proxyURL string

func TestMain(m *testing.M) {
	_, file, _, _ := runtime.Caller(0)
	examplesRoot := filepath.Join(filepath.Dir(file), "../../")

	bin := e2etest.EnvoyBin(examplesRoot)
	if _, err := os.Stat(bin); err != nil {
		fmt.Fprintf(os.Stderr, "SKIP: envoy not found at %s (run: make download-envoy)\n", bin)
		os.Exit(0)
	}

	if err := e2etest.CheckSharedLibrary(examplesRoot, "grpc-callout", "libgrpc-callout.so"); err != nil {
		fmt.Fprintf(os.Stderr, "e2e: %v\n", err)
		os.Exit(1)
	}

	upstreamPort := startUpstream()
	rlsPort, stopRLS := startRLS()
	defer stopRLS()

	proxyPort := e2etest.FreePort()
	adminPort := e2etest.FreePort()
	proxyURL = fmt.Sprintf("http://127.0.0.1:%d", proxyPort)

	type ports struct {
		ProxyPort    int
		AdminPort    int
		UpstreamPort int
		RLSPort      int
	}
	cfgPath := e2etest.WriteEnvoyConfig("transit-grpc-callout-e2e", envoyConfigTmpl, ports{
		ProxyPort:    proxyPort,
		AdminPort:    adminPort,
		UpstreamPort: upstreamPort,
		RLSPort:      rlsPort,
	})
	exampleDir := filepath.Join(examplesRoot, "grpc-callout")

	stopEnvoy, ok := e2etest.StartEnvoy(bin, cfgPath, exampleDir, adminPort, nil)
	if !ok {
		os.Exit(1)
	}
	fmt.Fprintln(os.Stderr, "e2e: envoy ready")

	code := m.Run()
	stopEnvoy()
	os.Exit(code)
}

func TestAllowForwardsRequest(t *testing.T) {
	resp, body := postBody(t, "allow")

	require.Equal(t, http.StatusOK, resp.StatusCode)
	require.Equal(t, "allow", body)
	require.Equal(t, "1", resp.Header.Get("x-upstream-hit"))
}

func TestOverLimitReturns429(t *testing.T) {
	resp, body := postBody(t, "deny")

	require.Equal(t, http.StatusTooManyRequests, resp.StatusCode)
	require.Equal(t, "rate limit exceeded", body)
	require.Equal(t, "over-limit", resp.Header.Get("x-rls-status"))
}

func TestRLSReceivesEnvoyRateLimitRequest(t *testing.T) {
	rlsRecorder.reset()
	resp, body := postBody(t, "inspect")

	require.Equal(t, http.StatusOK, resp.StatusCode)
	require.Equal(t, "inspect", body)

	req := rlsRecorder.last(t)
	require.Equal(t, "transit-grpc-callout", req.GetDomain())
	require.EqualValues(t, 1, req.GetHitsAddend())
	require.Len(t, req.GetDescriptors(), 1)
	require.Len(t, req.GetDescriptors()[0].GetEntries(), 1)
	require.Equal(t, "body-key", req.GetDescriptors()[0].GetEntries()[0].GetKey())
	require.Equal(t, "inspect", req.GetDescriptors()[0].GetEntries()[0].GetValue())
}

func postBody(t *testing.T, body string) (*http.Response, string) {
	t.Helper()

	resp, err := http.Post(proxyURL+"/", "text/plain", strings.NewReader(body))
	require.NoError(t, err)
	t.Cleanup(func() { _ = resp.Body.Close() })
	data, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	return resp, string(data)
}

func startUpstream() int {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		panic("startUpstream: " + err.Error())
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		w.Header().Set("x-upstream-hit", "1")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(body)
	})
	go http.Serve(l, mux) //nolint:errcheck
	return l.Addr().(*net.TCPAddr).Port
}

var rlsRecorder = &recordingRLS{}

type recordingRLS struct {
	rlsv3.UnimplementedRateLimitServiceServer

	mu      sync.Mutex
	lastReq *rlsv3.RateLimitRequest
}

func (s *recordingRLS) ShouldRateLimit(_ context.Context, req *rlsv3.RateLimitRequest) (*rlsv3.RateLimitResponse, error) {
	s.mu.Lock()
	s.lastReq = req
	s.mu.Unlock()

	code := rlsv3.RateLimitResponse_OK
	if descriptorValue(req) == "deny" {
		code = rlsv3.RateLimitResponse_OVER_LIMIT
	}
	return &rlsv3.RateLimitResponse{OverallCode: code}, nil
}

func (s *recordingRLS) reset() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.lastReq = nil
}

func (s *recordingRLS) last(t *testing.T) *rlsv3.RateLimitRequest {
	t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	require.NotNil(t, s.lastReq)
	return s.lastReq
}

func descriptorValue(req *rlsv3.RateLimitRequest) string {
	if len(req.GetDescriptors()) == 0 || len(req.GetDescriptors()[0].GetEntries()) == 0 {
		return ""
	}
	return req.GetDescriptors()[0].GetEntries()[0].GetValue()
}

func startRLS() (int, func()) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		panic("startRLS: " + err.Error())
	}
	srv := grpc.NewServer()
	rlsv3.RegisterRateLimitServiceServer(srv, rlsRecorder)
	go srv.Serve(l) //nolint:errcheck
	return l.Addr().(*net.TCPAddr).Port, srv.Stop
}
