// Package e2e runs the cluster-shard-router example against a real Envoy
// process. The fake upstreams stand in for L2 shard routers.
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
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/dio/transit/examples/internal/e2etest"
)

//go:embed testdata/envoy.tmpl.yaml
var envoyConfigTmpl string

var (
	proxyURL     string
	examplesRoot string
	control      *configServer
	shardA       *upstreamServer
	shardB       *upstreamServer
	shardC       *upstreamServer
)

func TestMain(m *testing.M) {
	_, file, _, _ := runtime.Caller(0)
	examplesRoot = filepath.Join(filepath.Dir(file), "../../")

	bin := e2etest.EnvoyBin(examplesRoot)
	if _, err := os.Stat(bin); err != nil {
		fmt.Fprintf(os.Stderr, "SKIP: envoy not found at %s (run: make download-envoy)\n", bin)
		os.Exit(0)
	}

	exampleDir := filepath.Join(examplesRoot, "cluster-shard-router")
	if err := e2etest.CheckSharedLibrary(examplesRoot, "cluster-shard-router", "libcluster-shard-router.so"); err != nil {
		fmt.Fprintf(os.Stderr, "e2e: %v\n", err)
		os.Exit(1)
	}

	shardA = startUpstream("l2-a")
	shardB = startUpstream("l2-b")
	shardC = startUpstream("l2-c")
	initial := snapshot{
		Version:      "initial",
		DefaultShard: "a",
		Shards: map[string]shard{
			"a": {
				Target:   shardA.target(),
				Prefixes: []string{"a"},
				Shard:    "a",
				Status:   "active",
			},
			"b": {
				Target:   shardB.target(),
				Prefixes: []string{"b"},
				Shard:    "b",
				Status:   "active",
			},
		},
	}
	control = startConfigServer(initial)

	proxyPort := e2etest.FreePort()
	adminPort := e2etest.FreePort()
	proxyURL = fmt.Sprintf("http://127.0.0.1:%d", proxyPort)
	clusterConfigJSON := marshalJSON(map[string]any{
		"config_url":     control.url(),
		"refresh_millis": 100,
		"timeout_millis": 500,
		"initial":        initial,
	})

	cfgPath := e2etest.WriteEnvoyConfig("cluster-shard-router-e2e", envoyConfigTmpl, envoyConfigData{
		ProxyPort:         proxyPort,
		AdminPort:         adminPort,
		ClusterConfigJSON: clusterConfigJSON,
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

func TestClusterShardRouterEndToEnd(t *testing.T) {
	t.Run("routes explicit tags to L2 shards and injects decision headers", func(t *testing.T) {
		requireTag(t, "a-demo", "l2-a")
		requireLastRequest(t, shardA, observedRequest{
			Tag:     "a-demo",
			Source:  "tag",
			Shard:   "a",
			Target:  shardA.target(),
			Version: "initial",
		})

		requireTag(t, "b-demo", "l2-b")
		requireLastRequest(t, shardB, observedRequest{
			Tag:     "b-demo",
			Source:  "tag",
			Shard:   "b",
			Target:  shardB.target(),
			Version: "initial",
		})
	})

	t.Run("derives tag from tenant and falls back for unknown tags", func(t *testing.T) {
		body, status, err := shardRequest(map[string]string{"x-tenant": "B-Tenant"})
		if err != nil {
			t.Fatalf("GET tenant route: %v", err)
		}
		if status != http.StatusOK || !strings.Contains(body, "l2-b") {
			t.Fatalf("tenant route: status=%d body=%q", status, body)
		}
		requireLastRequest(t, shardB, observedRequest{
			Tag:     "b-tenant",
			Source:  "tenant",
			Shard:   "b",
			Target:  shardB.target(),
			Version: "initial",
		})

		requireTag(t, "z-demo", "l2-a")
		requireLastRequest(t, shardA, observedRequest{
			Tag:     "z-demo",
			Source:  "tag",
			Shard:   "a",
			Target:  shardA.target(),
			Version: "initial",
		})
	})

	t.Run("dumps active shard config", func(t *testing.T) {
		body := requireDebugDump(t)
		for _, want := range []string{`"shard": "a"`, `"shard": "b"`, shardA.target(), shardB.target(), "initial", "default_shard"} {
			if !strings.Contains(body, want) {
				t.Fatalf("debug dump does not contain %q: %s", want, body)
			}
		}
	})

	t.Run("refreshes config and adds a new L2 shard", func(t *testing.T) {
		control.set(snapshot{
			Version:      "updated",
			DefaultShard: "a",
			Shards: map[string]shard{
				"a": {
					Target:   shardA.target(),
					Prefixes: []string{"a"},
					Shard:    "a",
					Status:   "active",
				},
				"b": {
					Target:   shardB.target(),
					Prefixes: []string{"b"},
					Shard:    "b",
					Status:   "active",
				},
				"c": {
					Target:   shardC.target(),
					Prefixes: []string{"c"},
					Shard:    "c",
					Status:   "active",
				},
			},
		})

		eventually(t, 10*time.Second, func() bool {
			body, status, err := shardRequest(map[string]string{"x-transit-tag": "c-demo"})
			return err == nil && status == http.StatusOK && strings.Contains(body, "l2-c")
		})
		requireLastRequest(t, shardC, observedRequest{
			Tag:     "c-demo",
			Source:  "tag",
			Shard:   "c",
			Target:  shardC.target(),
			Version: "updated",
		})

		body := requireDebugDump(t)
		for _, want := range []string{`"shard": "c"`, shardC.target(), "updated", `"c"`} {
			if !strings.Contains(body, want) {
				t.Fatalf("debug dump does not contain %q after refresh: %s", want, body)
			}
		}
	})
}

func requireTag(t *testing.T, tag, want string) {
	t.Helper()
	body, status, err := shardRequest(map[string]string{"x-transit-tag": tag})
	if err != nil {
		t.Fatalf("GET tag %s: %v", tag, err)
	}
	if status != http.StatusOK {
		t.Fatalf("tag %s: want 200, got %d with body %q", tag, status, body)
	}
	if !strings.Contains(body, want) {
		t.Fatalf("tag %s: body %q does not contain %q", tag, body, want)
	}
}

func shardRequest(headers map[string]string) (string, int, error) {
	req, err := http.NewRequest(http.MethodGet, proxyURL+"/", nil)
	if err != nil {
		return "", 0, err
	}
	for name, value := range headers {
		req.Header.Set(name, value)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", 0, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	return string(body), resp.StatusCode, nil
}

func requireDebugDump(t *testing.T) string {
	t.Helper()
	resp, err := http.Get(proxyURL + "/__cluster-shard-router/config") //nolint:noctx
	if err != nil {
		t.Fatalf("GET debug dump: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("debug dump: want 200, got %d with body %q", resp.StatusCode, body)
	}
	return string(body)
}

func requireLastRequest(t *testing.T, upstream *upstreamServer, want observedRequest) {
	t.Helper()
	got, ok := upstream.last()
	if !ok {
		t.Fatalf("%s saw no request", upstream.label)
	}
	if got != want {
		t.Fatalf("%s headers: got %+v, want %+v", upstream.label, got, want)
	}
}

func eventually(t *testing.T, timeout time.Duration, fn func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if fn() {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("condition was not met within %s; active config: %s", timeout, requireDebugDump(t))
}

type snapshot struct {
	Version      string           `json:"version"`
	DefaultShard string           `json:"default_shard"`
	Shards       map[string]shard `json:"shards"`
}

type shard struct {
	Target   string   `json:"target"`
	Prefixes []string `json:"prefixes"`
	Shard    string   `json:"shard"`
	Status   string   `json:"status"`
}

type configServer struct {
	mu       sync.RWMutex
	body     []byte
	listener net.Listener
}

func startConfigServer(initial snapshot) *configServer {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		panic("startConfigServer: " + err.Error())
	}
	s := &configServer{listener: l}
	s.set(initial)

	mux := http.NewServeMux()
	mux.HandleFunc("/shards.json", func(w http.ResponseWriter, _ *http.Request) {
		s.mu.RLock()
		defer s.mu.RUnlock()
		w.Header().Set("content-type", "application/json")
		w.WriteHeader(http.StatusOK)
		w.Write(s.body)
	})
	go http.Serve(l, mux) //nolint:errcheck
	return s
}

func (s *configServer) set(cfg snapshot) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.body = []byte(marshalJSON(cfg))
}

func (s *configServer) url() string {
	return "http://" + s.listener.Addr().String() + "/shards.json"
}

type upstreamServer struct {
	label    string
	port     int
	mu       sync.Mutex
	requests []observedRequest
}

type observedRequest struct {
	Tag     string
	Source  string
	Shard   string
	Target  string
	Version string
}

func startUpstream(label string) *upstreamServer {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		panic("startUpstream: " + err.Error())
	}
	s := &upstreamServer{label: label, port: l.Addr().(*net.TCPAddr).Port}
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		seen := observedRequest{
			Tag:     r.Header.Get("x-transit-tag"),
			Source:  r.Header.Get("x-transit-tag-source"),
			Shard:   r.Header.Get("x-transit-l1-shard"),
			Target:  r.Header.Get("x-transit-l1-target"),
			Version: r.Header.Get("x-cluster-shard-router-version"),
		}
		s.mu.Lock()
		s.requests = append(s.requests, seen)
		s.mu.Unlock()
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(label))
	})
	go http.Serve(l, mux) //nolint:errcheck
	return s
}

func (s *upstreamServer) target() string {
	return net.JoinHostPort("localhost", fmt.Sprint(s.port))
}

func (s *upstreamServer) last() (observedRequest, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.requests) == 0 {
		return observedRequest{}, false
	}
	return s.requests[len(s.requests)-1], true
}

type envoyConfigData struct {
	ProxyPort         int
	AdminPort         int
	ClusterConfigJSON string
}

func marshalJSON(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return string(b)
}
