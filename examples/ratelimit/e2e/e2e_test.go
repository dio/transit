package e2e

import (
	"context"
	_ "embed"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	rlsv3 "github.com/envoyproxy/go-control-plane/envoy/service/ratelimit/v3"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/dio/transit/examples/internal/e2etest"
	"github.com/dio/transit/examples/ratelimit"
)

//go:embed testdata/envoy.tmpl.yaml
var envoyTemplate string

func TestAdmission(t *testing.T) {
	for _, mode := range []string{"closed", "open"} {
		t.Run(mode, func(t *testing.T) {
			h := start(t, mode, "rls-service")
			for _, tc := range []struct {
				key  string
				want int
			}{
				{"alice", 200}, {"deny", 429}, {"", 400},
				{"error", failureStatus(mode)}, {"unknown", failureStatus(mode)},
				{"slow", failureStatus(mode)},
			} {
				t.Run(tc.key, func(t *testing.T) {
					before := h.upstreamHits.Load()
					resp, body := h.request(t, tc.key, "original body")
					require.Equal(t, tc.want, resp.StatusCode, body)
					if tc.want == http.StatusOK {
						require.Equal(t, "original body", body)
						require.Equal(t, before+1, h.upstreamHits.Load())
					} else {
						require.Equal(t, before, h.upstreamHits.Load(), "rejected request reached upstream")
					}
				})
			}
			resp, _ := h.request(t, "inspect", "")
			require.Equal(t, 200, resp.StatusCode)
			h.rls.mu.Lock()
			req := h.rls.last
			h.rls.mu.Unlock()
			require.NotNil(t, req)
			require.Equal(t, "orange", req.Domain)
			require.EqualValues(t, 1, req.HitsAddend)
			require.Len(t, req.Descriptors, 1)
			require.Len(t, req.Descriptors[0].Entries, 2)
			require.Equal(t, "key_id", req.Descriptors[0].Entries[0].Key)
			require.Equal(t, "inspect", req.Descriptors[0].Entries[0].Value)
			require.Equal(t, "dim", req.Descriptors[0].Entries[1].Key)
			require.Equal(t, "rpm", req.Descriptors[0].Entries[1].Value)
			t.Run("chunked with trailers", func(t *testing.T) {
				before := h.upstreamHits.Load()
				req, err := http.NewRequest(http.MethodPost, h.url, strings.NewReader("chunked body"))
				require.NoError(t, err)
				req.ContentLength = -1
				req.Trailer = http.Header{"X-End": {"done"}}
				req.Header.Set("x-tars-id", "slow")
				client := &http.Client{Timeout: 3 * time.Second}
				resp, err := client.Do(req)
				require.NoError(t, err)
				defer resp.Body.Close()
				body, err := io.ReadAll(resp.Body)
				require.NoError(t, err)
				require.Equal(t, failureStatus(mode), resp.StatusCode)
				if mode == "closed" {
					require.Equal(t, before, h.upstreamHits.Load())
				} else {
					require.Equal(t, "chunked body", string(body))
				}
			})
		})
	}
}

func TestMissingCluster(t *testing.T) {
	for _, mode := range []string{"closed", "open"} {
		t.Run(mode, func(t *testing.T) {
			h := start(t, mode, "missing-cluster")
			resp, _ := h.request(t, "alice", "payload")
			require.Equal(t, failureStatus(mode), resp.StatusCode)
			h.rls.mu.Lock()
			defer h.rls.mu.Unlock()
			require.Nil(t, h.rls.last)
			if mode == "closed" {
				require.Zero(t, h.upstreamHits.Load())
			}
		})
	}
}

func failureStatus(mode string) int {
	if mode == "open" {
		return http.StatusOK
	}
	return http.StatusServiceUnavailable
}

type harness struct {
	url          string
	upstreamHits atomic.Int64
	rls          *recordingRLS
}

func start(t *testing.T, mode, cluster string) *harness {
	t.Helper()
	_, file, _, _ := runtime.Caller(0)
	examplesRoot := filepath.Join(filepath.Dir(file), "../..")
	bin := e2etest.EnvoyBin(examplesRoot)
	if _, err := os.Stat(bin); err != nil {
		t.Skipf("Envoy not found: %v", err)
	}
	require.NoError(t, e2etest.CheckSharedLibrary(examplesRoot, "ratelimit", "libratelimit.so"))
	h := &harness{rls: &recordingRLS{}}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h.upstreamHits.Add(1)
		_, _ = io.Copy(w, r.Body)
	}))
	t.Cleanup(upstream.Close)
	l, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	srv := grpc.NewServer()
	rlsv3.RegisterRateLimitServiceServer(srv, h.rls)
	go func() { _ = srv.Serve(l) }()
	t.Cleanup(srv.Stop)

	config := ratelimit.Config{
		Cluster: cluster, Domain: "orange", TimeoutMillis: 100, FailureMode: mode,
		Descriptors: [][]ratelimit.Entry{{{Key: "key_id", Header: "x-tars-id"}, {Key: "dim", Value: "rpm"}}},
	}
	data, err := json.Marshal(config)
	require.NoError(t, err)
	configPath := filepath.Join(t.TempDir(), "config.json")
	require.NoError(t, os.WriteFile(configPath, data, 0600))
	ports := struct{ ProxyPort, AdminPort, UpstreamPort, RLSPort int }{
		e2etest.FreePort(), e2etest.FreePort(),
		upstream.Listener.Addr().(*net.TCPAddr).Port, l.Addr().(*net.TCPAddr).Port,
	}
	cfgPath := e2etest.WriteEnvoyConfig("transit-ratelimit-e2e", envoyTemplate, ports)
	stop, ok := e2etest.StartEnvoy(bin, cfgPath, filepath.Join(examplesRoot, "ratelimit"), ports.AdminPort,
		[]string{"RATELIMIT_CONFIG=" + configPath})
	require.True(t, ok, "Envoy failed to start")
	t.Cleanup(stop)
	h.url = fmt.Sprintf("http://127.0.0.1:%d/", ports.ProxyPort)
	return h
}

func (h *harness) request(t *testing.T, key, body string) (*http.Response, string) {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, h.url, strings.NewReader(body))
	require.NoError(t, err)
	if key != "" {
		req.Header.Set("x-tars-id", key)
	}
	client := &http.Client{Timeout: 3 * time.Second}
	resp, err := client.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	return resp, string(data)
}

type recordingRLS struct {
	rlsv3.UnimplementedRateLimitServiceServer
	mu   sync.Mutex
	last *rlsv3.RateLimitRequest
}

func (s *recordingRLS) ShouldRateLimit(ctx context.Context, req *rlsv3.RateLimitRequest) (*rlsv3.RateLimitResponse, error) {
	s.mu.Lock()
	s.last = req
	s.mu.Unlock()
	switch req.GetDescriptors()[0].GetEntries()[0].GetValue() {
	case "deny":
		return &rlsv3.RateLimitResponse{OverallCode: rlsv3.RateLimitResponse_OVER_LIMIT}, nil
	case "error":
		return nil, status.Error(codes.Unavailable, "test failure")
	case "unknown":
		return &rlsv3.RateLimitResponse{}, nil
	case "slow":
		<-ctx.Done()
		return nil, status.Error(codes.Canceled, "callout canceled")
	default:
		return &rlsv3.RateLimitResponse{OverallCode: rlsv3.RateLimitResponse_OK}, nil
	}
}
