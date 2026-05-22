// Package e2e runs the cluster-router example against a real Envoy process.
package e2e

import (
	"bytes"
	_ "embed"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
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
	envoyCmd     *exec.Cmd
	examplesRoot string
	control      *configServer
	upstreamA    *upstreamServer
	upstreamB    *upstreamServer
	upstreamC    *upstreamServer
)

func TestMain(m *testing.M) {
	_, file, _, _ := runtime.Caller(0)
	examplesRoot = filepath.Join(filepath.Dir(file), "../../")

	bin := e2etest.EnvoyBin(examplesRoot)
	if _, err := os.Stat(bin); err != nil {
		fmt.Fprintf(os.Stderr, "SKIP: envoy not found at %s (run: make download-envoy)\n", bin)
		os.Exit(0)
	}

	exampleDir := filepath.Join(examplesRoot, "cluster-router")
	if os.Getenv("TRANSIT_SKIP_BUILD") == "" {
		fmt.Fprintln(os.Stderr, "e2e: building libcluster-router.so ...")
		if err := e2etest.BuildSharedLibrary(examplesRoot, "cluster-router", "libcluster-router.so"); err != nil {
			fmt.Fprintf(os.Stderr, "e2e: build failed: %v\n", err)
			os.Exit(1)
		}
		fmt.Fprintln(os.Stderr, "e2e: build OK")
	} else {
		if err := e2etest.BuildSharedLibrary(examplesRoot, "cluster-router", "libcluster-router.so"); err != nil {
			fmt.Fprintf(os.Stderr, "e2e: %v\n", err)
			os.Exit(1)
		}
		fmt.Fprintln(os.Stderr, "e2e: reusing existing libcluster-router.so (TRANSIT_SKIP_BUILD=1)")
	}

	upstreamA = startUpstream("upstream a")
	upstreamB = startUpstream("upstream b")
	upstreamC = startUpstream("upstream c")
	initial := snapshot{
		Version: "initial",
		Models: map[string]model{
			"gpt-fast": {
				Target:     upstreamA.target(),
				Provider:   "openai",
				AuthHeader: "Bearer openai-token",
			},
			"claude-safe": {
				Target:     upstreamB.target(),
				Provider:   "anthropic",
				AuthHeader: "Bearer anthropic-token",
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

	cfgPath := e2etest.WriteEnvoyConfig("cluster-router-e2e", envoyConfigTmpl, envoyConfigData{
		ProxyPort:         proxyPort,
		AdminPort:         adminPort,
		ClusterConfigJSON: clusterConfigJSON,
	})

	envoyCmd = exec.Command(bin, "-c", cfgPath, "--log-level", "warning",
		"--component-log-level", "dynamic_modules:info")
	envoyCmd.Env = append(os.Environ(),
		"GODEBUG=cgocheck=0",
		"ENVOY_DYNAMIC_MODULES_SEARCH_PATH="+exampleDir,
	)
	envoyCmd.Stdout = os.Stderr
	envoyCmd.Stderr = os.Stderr
	if err := envoyCmd.Start(); err != nil {
		os.Remove(cfgPath)
		fmt.Fprintf(os.Stderr, "e2e: envoy start failed: %v\n", err)
		os.Exit(1)
	}
	fmt.Fprintf(os.Stderr, "e2e: envoy pid=%d\n", envoyCmd.Process.Pid)

	adminURL := fmt.Sprintf("http://127.0.0.1:%d", adminPort)
	if !e2etest.WaitURL(adminURL+"/ready", 15*time.Second) {
		envoyCmd.Process.Kill()
		envoyCmd.Wait()
		os.Remove(cfgPath)
		fmt.Fprintln(os.Stderr, "e2e: envoy not ready in time")
		os.Exit(1)
	}
	fmt.Fprintln(os.Stderr, "e2e: envoy ready")

	code := m.Run()

	envoyCmd.Process.Kill()
	envoyCmd.Wait()
	os.Remove(cfgPath)
	os.Exit(code)
}

func TestClusterRouterEndToEnd(t *testing.T) {
	t.Run("routes initial models and injects upstream headers", func(t *testing.T) {
		requireModel(t, "gpt-fast", "upstream a")
		requireLastRequest(t, upstreamA, observedRequest{
			Auth:     "Bearer openai-token",
			Provider: "openai",
			Version:  "initial",
		})

		requireModel(t, "claude-safe", "upstream b")
		requireLastRequest(t, upstreamB, observedRequest{
			Auth:     "Bearer anthropic-token",
			Provider: "anthropic",
			Version:  "initial",
		})
	})

	t.Run("dumps active config without secrets", func(t *testing.T) {
		body := requireDebugDump(t)
		for _, want := range []string{"gpt-fast", "claude-safe", "initial"} {
			if !strings.Contains(body, want) {
				t.Fatalf("debug dump does not contain %q: %s", want, body)
			}
		}
		if strings.Contains(body, "Bearer") {
			t.Fatalf("debug dump leaked auth header: %s", body)
		}
	})

	t.Run("rejects unknown models", func(t *testing.T) {
		req, err := http.NewRequest(http.MethodGet, proxyURL+"/", nil)
		if err != nil {
			t.Fatalf("new request: %v", err)
		}
		req.Header.Set("x-model", "unknown-model")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("GET unknown model: %v", err)
		}
		defer resp.Body.Close()
		if resp.StatusCode == http.StatusOK {
			t.Fatalf("unknown model unexpectedly returned 200")
		}
	})

	t.Run("refreshes config and adds a new upstream", func(t *testing.T) {
		control.set(snapshot{
			Version: "updated",
			Models: map[string]model{
				"gpt-fast": {
					Target:     upstreamA.target(),
					Provider:   "openai",
					AuthHeader: "Bearer openai-token",
				},
				"claude-safe": {
					Target:     upstreamB.target(),
					Provider:   "anthropic",
					AuthHeader: "Bearer anthropic-token",
				},
				// gpt-slow deliberately reuses upstream A. This proves model
				// additions are not forced to create a fresh host every time.
				"gpt-slow": {
					Target:     upstreamA.target(),
					Provider:   "openai",
					AuthHeader: "Bearer slow-token",
				},
				// kimi-fast points at a host Envoy did not know at bootstrap.
				// The cluster extension resolves and adds it from the refreshed JSON.
				"kimi-fast": {
					Target:     upstreamC.target(),
					Provider:   "moonshot",
					AuthHeader: "Bearer moonshot-token",
				},
			},
		})

		eventually(t, 10*time.Second, func() bool {
			body, status, err := modelRequest("gpt-slow")
			return err == nil && status == http.StatusOK && strings.Contains(body, "upstream a")
		})
		requireLastRequest(t, upstreamA, observedRequest{
			Auth:     "Bearer slow-token",
			Provider: "openai",
			Version:  "updated",
		})

		eventually(t, 10*time.Second, func() bool {
			body, status, err := modelRequest("kimi-fast")
			return err == nil && status == http.StatusOK && strings.Contains(body, "upstream c")
		})
		requireLastRequest(t, upstreamC, observedRequest{
			Auth:     "Bearer moonshot-token",
			Provider: "moonshot",
			Version:  "updated",
		})

		body := requireDebugDump(t)
		for _, want := range []string{"gpt-slow", "kimi-fast", "updated"} {
			if !strings.Contains(body, want) {
				t.Fatalf("debug dump does not contain %q after refresh: %s", want, body)
			}
		}
		if strings.Contains(body, "Bearer") {
			t.Fatalf("debug dump leaked auth header after refresh: %s", body)
		}
	})
}

func requireModel(t *testing.T, model string, want string) {
	t.Helper()
	body, status, err := modelRequest(model)
	if err != nil {
		t.Fatalf("GET model %s: %v", model, err)
	}
	if status != http.StatusOK {
		t.Fatalf("model %s: want 200, got %d with body %q", model, status, body)
	}
	if !strings.Contains(body, want) {
		t.Fatalf("model %s: body %q does not contain %q", model, body, want)
	}
}

func modelRequest(model string) (string, int, error) {
	req, err := http.NewRequest(http.MethodPost, proxyURL+"/v1/chat/completions", bytes.NewBufferString(`{"model":"`+model+`"}`))
	if err != nil {
		return "", 0, err
	}
	req.Header.Set("content-type", "application/json")
	req.Header.Set("x-model", model)
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
	resp, err := http.Get(proxyURL + "/__cluster-router/config") //nolint:noctx
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
	if got.Auth != want.Auth || got.Provider != want.Provider || got.Version != want.Version {
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
	Version string           `json:"version"`
	Models  map[string]model `json:"models"`
}

type model struct {
	Target     string `json:"target"`
	Provider   string `json:"provider"`
	AuthHeader string `json:"auth_header"`
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
	mux.HandleFunc("/routes.json", func(w http.ResponseWriter, _ *http.Request) {
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
	return "http://" + s.listener.Addr().String() + "/routes.json"
}

type upstreamServer struct {
	label    string
	port     int
	mu       sync.Mutex
	requests []observedRequest
}

type observedRequest struct {
	Auth     string
	Provider string
	Version  string
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
			Auth:     r.Header.Get("authorization"),
			Provider: r.Header.Get("x-llm-provider"),
			Version:  r.Header.Get("x-cluster-router-version"),
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
