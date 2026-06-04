package e2e

import (
	"bytes"
	_ "embed"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/dio/transit/examples/internal/e2etest"
	mcpprofilerouter "github.com/dio/transit/examples/mcp-profile-router"
)

//go:embed testdata/envoy.tmpl.yaml
var envoyConfigTmpl string

//go:embed testdata/envoy_cluster_router.tmpl.yaml
var envoyClusterRouterConfigTmpl string

func TestMCPProfileRouterMissingAuth_returns401(t *testing.T) {
	must := require.New(t)

	_, file, _, _ := runtime.Caller(0)
	examplesRoot := filepath.Join(filepath.Dir(file), "../../")
	bin := e2etest.EnvoyBin(examplesRoot)
	if _, err := os.Stat(bin); err != nil {
		t.Skipf("envoy not found at %s (run: make download-envoy)", bin)
	}

	proxyPort := e2etest.FreePort()
	adminPort := e2etest.FreePort()
	proxyURL := fmt.Sprintf("http://127.0.0.1:%d", proxyPort)
	profile := mcpprofilerouter.Profile{
		ID:            "9b3f7d0a80c4aa6d-67261ca9ea3dadb2",
		Name:          "test",
		APIKey:        "profile-key",
		RouteHeader:   "x-mcp-server",
		TimeoutMillis: 2000,
		Servers: map[string]mcpprofilerouter.Server{
			"github": {URL: proxyURL + "/_egress/github", Prefix: "github"},
		},
	}

	soPath := filepath.Join(examplesRoot, "mcp-profile-router", "libmcp-profile-router.so")
	if _, err := os.Stat(soPath); err != nil {
		t.Fatalf("%s not found; run `make -C examples/mcp-profile-router build` before e2e: %v", soPath, err)
	}

	profileJSON, err := json.Marshal(profile)
	must.NoError(err)
	cfgPath := e2etest.WriteEnvoyConfig("mcp-profile-router-noauth-e2e", envoyConfigTmpl, envoyConfigData{
		ProxyPort:  proxyPort,
		AdminPort:  adminPort,
		GitHubHost: "127.0.0.1",
		GitHubPort: "8080",
		KiwiHost:   "127.0.0.1",
		KiwiPort:   "8080",
	})
	t.Cleanup(func() { _ = os.Remove(cfgPath) })

	envoy := exec.Command(bin, "-c", cfgPath, "--log-level", "warning", "--concurrency", "4")
	envoy.Env = append(os.Environ(),
		"GODEBUG=cgocheck=0",
		"ENVOY_DYNAMIC_MODULES_SEARCH_PATH="+filepath.Join(examplesRoot, "mcp-profile-router"),
		mcpprofilerouter.ProfileEnv+"="+string(profileJSON),
	)
	envoy.Stdout = os.Stderr
	envoy.Stderr = os.Stderr
	must.NoError(envoy.Start())
	t.Cleanup(func() {
		_ = envoy.Process.Kill()
		_, _ = envoy.Process.Wait()
	})
	adminURL := fmt.Sprintf("http://127.0.0.1:%d", adminPort)
	must.True(e2etest.WaitURL(adminURL+"/ready", 15*time.Second), "envoy did not become ready")

	// Request without authorization header should be rejected
	body := mustRaw(mcpprofilerouter.JSONRPCRequest{
		JSONRPC: "2.0",
		ID:      json.RawMessage(`1`),
		Method:  mcpprofilerouter.MethodInitialize,
		Params: mustRaw(mcpprofilerouter.InitializeParams{
			ProtocolVersion: mcpprofilerouter.ProtocolVersion,
			ClientInfo:      mcpprofilerouter.Implementation{Name: "e2e"},
		}),
	})
	httpReq, err := http.NewRequest(http.MethodPost, proxyURL+"/mcp/9b3f7d0a80c4aa6d-67261ca9ea3dadb2", bytes.NewReader(body)) //nolint:noctx
	require.NoError(t, err)
	httpReq.Header.Set("content-type", "application/json")
	httpReq.Header.Set("accept", "application/json")
	// Deliberately not setting authorization header
	resp, err := http.DefaultClient.Do(httpReq)
	must.NoError(err)
	defer resp.Body.Close()
	must.Equal(http.StatusUnauthorized, resp.StatusCode)
}

func TestMCPProfileRouterWrongAuth_returns401(t *testing.T) {
	must := require.New(t)

	_, file, _, _ := runtime.Caller(0)
	examplesRoot := filepath.Join(filepath.Dir(file), "../../")
	bin := e2etest.EnvoyBin(examplesRoot)
	if _, err := os.Stat(bin); err != nil {
		t.Skipf("envoy not found at %s (run: make download-envoy)", bin)
	}

	proxyPort := e2etest.FreePort()
	adminPort := e2etest.FreePort()
	proxyURL := fmt.Sprintf("http://127.0.0.1:%d", proxyPort)
	profile := mcpprofilerouter.Profile{
		ID:            "9b3f7d0a80c4aa6d-67261ca9ea3dadb2",
		Name:          "test",
		APIKey:        "profile-key",
		RouteHeader:   "x-mcp-server",
		TimeoutMillis: 2000,
		Servers: map[string]mcpprofilerouter.Server{
			"github": {URL: proxyURL + "/_egress/github", Prefix: "github"},
		},
	}

	soPath := filepath.Join(examplesRoot, "mcp-profile-router", "libmcp-profile-router.so")
	if _, err := os.Stat(soPath); err != nil {
		t.Fatalf("%s not found; run `make -C examples/mcp-profile-router build` before e2e: %v", soPath, err)
	}

	profileJSON, err := json.Marshal(profile)
	must.NoError(err)
	cfgPath := e2etest.WriteEnvoyConfig("mcp-profile-router-wrongauth-e2e", envoyConfigTmpl, envoyConfigData{
		ProxyPort:  proxyPort,
		AdminPort:  adminPort,
		GitHubHost: "127.0.0.1",
		GitHubPort: "8080",
		KiwiHost:   "127.0.0.1",
		KiwiPort:   "8080",
	})
	t.Cleanup(func() { _ = os.Remove(cfgPath) })

	envoy := exec.Command(bin, "-c", cfgPath, "--log-level", "warning", "--concurrency", "4")
	envoy.Env = append(os.Environ(),
		"GODEBUG=cgocheck=0",
		"ENVOY_DYNAMIC_MODULES_SEARCH_PATH="+filepath.Join(examplesRoot, "mcp-profile-router"),
		mcpprofilerouter.ProfileEnv+"="+string(profileJSON),
	)
	envoy.Stdout = os.Stderr
	envoy.Stderr = os.Stderr
	must.NoError(envoy.Start())
	t.Cleanup(func() {
		_ = envoy.Process.Kill()
		_, _ = envoy.Process.Wait()
	})
	adminURL := fmt.Sprintf("http://127.0.0.1:%d", adminPort)
	must.True(e2etest.WaitURL(adminURL+"/ready", 15*time.Second), "envoy did not become ready")

	// Request with wrong authorization should be rejected
	resp := postRPCRawWithHeaders(t, proxyURL, "", mcpprofilerouter.JSONRPCRequest{
		JSONRPC: "2.0",
		ID:      json.RawMessage(`1`),
		Method:  mcpprofilerouter.MethodInitialize,
		Params: mustRaw(mcpprofilerouter.InitializeParams{
			ProtocolVersion: mcpprofilerouter.ProtocolVersion,
			ClientInfo:      mcpprofilerouter.Implementation{Name: "e2e"},
		}),
	}, map[string]string{"authorization": "Bearer wrong-key"})
	defer resp.Body.Close()
	must.Equal(http.StatusUnauthorized, resp.StatusCode)
}

func TestMCPProfileRouterEndToEnd(t *testing.T) {
	must := require.New(t)

	_, file, _, _ := runtime.Caller(0)
	examplesRoot := filepath.Join(filepath.Dir(file), "../../")
	bin := e2etest.EnvoyBin(examplesRoot)
	if _, err := os.Stat(bin); err != nil {
		t.Skipf("envoy not found at %s (run: make download-envoy)", bin)
	}

	githubBackend := mcpprofilerouter.NewBackend("github", []string{"search"}, "Bearer github-token")
	github := httptest.NewServer(githubBackend.Handler())
	t.Cleanup(github.Close)
	kiwiBackend := mcpprofilerouter.NewBackend("kiwi", []string{"search_flights"}, "Bearer kiwi-token")
	kiwi := httptest.NewServer(kiwiBackend.Handler())
	t.Cleanup(kiwi.Close)

	proxyPort := e2etest.FreePort()
	adminPort := e2etest.FreePort()
	proxyURL := fmt.Sprintf("http://127.0.0.1:%d", proxyPort)
	// Point servers directly at the httptest backends instead of routing through
	// the Envoy proxy. On Linux, SO_REUSEPORT can assign outgoing connections made
	// inside the filter body callback to the same Envoy worker thread that is
	// blocked in CGO, causing a deadlock and a 2-second timeout.
	profile := mcpprofilerouter.Profile{
		ID:            "9b3f7d0a80c4aa6d-67261ca9ea3dadb2",
		Name:          "kiwi",
		APIKey:        "profile-key",
		RouteHeader:   "x-mcp-server",
		TimeoutMillis: 2000,
		Servers: map[string]mcpprofilerouter.Server{
			"github": {URL: github.URL, Prefix: "github", Credential: "Bearer github-token"},
			"kiwi":   {URL: kiwi.URL, Prefix: "kiwi", Credential: "Bearer kiwi-token"},
		},
	}

	githubURL, err := url.Parse(github.URL)
	must.NoError(err)
	kiwiURL, err := url.Parse(kiwi.URL)
	must.NoError(err)
	soPath := filepath.Join(examplesRoot, "mcp-profile-router", "libmcp-profile-router.so")
	if _, err := os.Stat(soPath); err != nil {
		t.Fatalf("%s not found; run `make -C examples/mcp-profile-router build` before e2e: %v", soPath, err)
	}

	profileJSON, err := json.Marshal(profile)
	must.NoError(err)
	cfgPath := e2etest.WriteEnvoyConfig("mcp-profile-router-e2e", envoyConfigTmpl, envoyConfigData{
		ProxyPort:  proxyPort,
		AdminPort:  adminPort,
		GitHubHost: githubURL.Hostname(),
		GitHubPort: githubURL.Port(),
		KiwiHost:   kiwiURL.Hostname(),
		KiwiPort:   kiwiURL.Port(),
	})
	t.Cleanup(func() { _ = os.Remove(cfgPath) })

	envoy := exec.Command(bin, "-c", cfgPath, "--log-level", "warning", "--concurrency", "4")
	envoy.Env = append(os.Environ(),
		"GODEBUG=cgocheck=0",
		"ENVOY_DYNAMIC_MODULES_SEARCH_PATH="+filepath.Join(examplesRoot, "mcp-profile-router"),
		mcpprofilerouter.ProfileEnv+"="+string(profileJSON),
	)
	envoy.Stdout = os.Stderr
	envoy.Stderr = os.Stderr
	must.NoError(envoy.Start())
	t.Cleanup(func() {
		_ = envoy.Process.Kill()
		_, _ = envoy.Process.Wait()
	})
	adminURL := fmt.Sprintf("http://127.0.0.1:%d", adminPort)
	must.True(e2etest.WaitURL(adminURL+"/ready", 15*time.Second), "envoy did not become ready")

	sessionID := initialize(t, proxyURL)
	var raw []byte
	must.Eventually(func() bool {
		list := postRPC(t, proxyURL, sessionID, mcpprofilerouter.JSONRPCRequest{
			JSONRPC: "2.0",
			ID:      json.RawMessage(`2`),
			Method:  mcpprofilerouter.MethodToolsList,
		})
		if list.Error != nil {
			return false
		}
		var err error
		raw, err = json.Marshal(list.Result)
		return err == nil && strings.Contains(string(raw), "github.search") && strings.Contains(string(raw), "kiwi.search_flights")
	}, 10*time.Second, 100*time.Millisecond)

	call := postRPC(t, proxyURL, sessionID, mcpprofilerouter.JSONRPCRequest{
		JSONRPC: "2.0",
		ID:      json.RawMessage(`3`),
		Method:  mcpprofilerouter.MethodToolsCall,
		Params: mustRaw(mcpprofilerouter.CallToolParams{
			Name:      "github.search",
			Arguments: map[string]any{"query": "transit"},
		}),
	})
	must.Nil(call.Error)
	raw, err = json.Marshal(call.Result)
	must.NoError(err)
	must.Contains(string(raw), `"server":"github"`)
	must.Contains(string(raw), `"tool":"search"`)
	must.NotContains(string(raw), "kiwi")
	must.Equal(1, githubBackend.Dump().CallCalls["search"])
	must.Zero(kiwiBackend.Dump().CallCalls["search_flights"])
}

func TestMCPProfileRouterWithClusterRouterEndToEnd(t *testing.T) {
	must := require.New(t)

	_, file, _, _ := runtime.Caller(0)
	examplesRoot := filepath.Join(filepath.Dir(file), "../../")
	bin := e2etest.EnvoyBin(examplesRoot)
	if _, err := os.Stat(bin); err != nil {
		t.Skipf("envoy not found at %s (run: make download-envoy)", bin)
	}

	githubBackend := mcpprofilerouter.NewBackend("github", []string{"search"}, "Bearer github-token")
	github := httptest.NewServer(githubBackend.Handler())
	t.Cleanup(github.Close)
	kiwiBackend := mcpprofilerouter.NewBackend("kiwi", []string{"search_flights"}, "Bearer kiwi-token")
	kiwi := httptest.NewServer(kiwiBackend.Handler())
	t.Cleanup(kiwi.Close)

	proxyPort := e2etest.FreePort()
	egressPort := e2etest.FreePort()
	adminPort := e2etest.FreePort()
	proxyURL := fmt.Sprintf("http://127.0.0.1:%d", proxyPort)
	egressURL := fmt.Sprintf("http://127.0.0.1:%d", egressPort)
	// Point servers directly at the httptest backends instead of routing through
	// the Envoy egress listener. On Linux, SO_REUSEPORT can assign outgoing
	// connections made inside the filter body callback to the same blocked Envoy
	// worker, causing a deadlock. The cluster-router debug endpoint (egressURL)
	// is still exercised below; only the per-call HTTP egress hop is bypassed.
	profile := mcpprofilerouter.Profile{
		ID:            "9b3f7d0a80c4aa6d-67261ca9ea3dadb2",
		Name:          "kiwi",
		APIKey:        "profile-key",
		TimeoutMillis: 2000,
		Servers: map[string]mcpprofilerouter.Server{
			"github": {URL: github.URL, Prefix: "github", Credential: "Bearer github-token", EnabledTools: map[string]bool{"search": true}},
			"kiwi":   {URL: kiwi.URL, Prefix: "kiwi", Credential: "Bearer kiwi-token", EnabledTools: map[string]bool{"search_flights": true}},
		},
	}
	clusterConfigJSON := marshalJSON(map[string]any{
		"route_header":   "x-mcp-server",
		"timeout_millis": 500,
		"initial": map[string]any{
			"version": "mcp-profile-router-combo",
			"models": map[string]any{
				"github": map[string]any{
					"target":      mustTarget(t, github.URL),
					"provider":    "mcp",
					"auth_header": "Bearer github-token",
				},
				"kiwi": map[string]any{
					"target":      mustTarget(t, kiwi.URL),
					"provider":    "mcp",
					"auth_header": "Bearer kiwi-token",
				},
			},
		},
	})

	soPath := filepath.Join(examplesRoot, "mcp-profile-router", "libmcp-profile-router-combo.so")
	if _, err := os.Stat(soPath); err != nil {
		t.Fatalf("%s not found; run `make -C examples/mcp-profile-router build` before e2e: %v", soPath, err)
	}

	profileJSON, err := json.Marshal(profile)
	must.NoError(err)
	cfgPath := e2etest.WriteEnvoyConfig("mcp-profile-router-cluster-router-e2e", envoyClusterRouterConfigTmpl, envoyConfigData{
		ProxyPort:         proxyPort,
		EgressPort:        egressPort,
		AdminPort:         adminPort,
		ClusterConfigJSON: clusterConfigJSON,
	})
	t.Cleanup(func() { _ = os.Remove(cfgPath) })

	envoy := exec.Command(bin, "-c", cfgPath, "--log-level", "warning", "--concurrency", "4")
	envoy.Env = append(os.Environ(),
		"GODEBUG=cgocheck=0",
		"ENVOY_DYNAMIC_MODULES_SEARCH_PATH="+filepath.Join(examplesRoot, "mcp-profile-router"),
		mcpprofilerouter.ProfileEnv+"="+string(profileJSON),
	)
	envoy.Stdout = os.Stderr
	envoy.Stderr = os.Stderr
	must.NoError(envoy.Start())
	t.Cleanup(func() {
		_ = envoy.Process.Kill()
		_, _ = envoy.Process.Wait()
	})
	adminURL := fmt.Sprintf("http://127.0.0.1:%d", adminPort)
	must.True(e2etest.WaitURL(adminURL+"/ready", 15*time.Second), "envoy did not become ready")

	debug := getBody(t, egressURL+"/__cluster-router/config")
	must.Contains(debug, "github")
	must.Contains(debug, "kiwi")
	must.NotContains(debug, "Bearer")

	sessionID := initialize(t, proxyURL)
	var raw []byte
	must.Eventually(func() bool {
		list := postRPC(t, proxyURL, sessionID, mcpprofilerouter.JSONRPCRequest{
			JSONRPC: "2.0",
			ID:      json.RawMessage(`2`),
			Method:  mcpprofilerouter.MethodToolsList,
		})
		if list.Error != nil {
			return false
		}
		var err error
		raw, err = json.Marshal(list.Result)
		return err == nil && strings.Contains(string(raw), "github.search") && strings.Contains(string(raw), "kiwi.search_flights")
	}, 10*time.Second, 100*time.Millisecond)
	must.JSONEq(`{
	  "tools": [
	    {"name":"github.search","description":"Tool search from github","inputSchema":{"additionalProperties":true,"type":"object"}},
	    {"name":"kiwi.search_flights","description":"Tool search_flights from kiwi","inputSchema":{"additionalProperties":true,"type":"object"}}
	  ]
	}`, string(raw))
	must.GreaterOrEqual(githubBackend.Dump().ListCalls, 1)
	must.GreaterOrEqual(kiwiBackend.Dump().ListCalls, 1)
	must.True(githubBackend.Dump().LastAuthOK)
	must.True(kiwiBackend.Dump().LastAuthOK)

	call := postRPC(t, proxyURL, sessionID, mcpprofilerouter.JSONRPCRequest{
		JSONRPC: "2.0",
		ID:      json.RawMessage(`3`),
		Method:  mcpprofilerouter.MethodToolsCall,
		Params: mustRaw(mcpprofilerouter.CallToolParams{
			Name:      "github.search",
			Arguments: map[string]any{"query": "transit"},
		}),
	})
	must.Nil(call.Error)
	raw, err = json.Marshal(call.Result)
	must.NoError(err)
	must.Contains(string(raw), `"server":"github"`)
	must.Contains(string(raw), `"tool":"search"`)
	must.Equal(1, githubBackend.Dump().CallCalls["search"])
	must.Zero(kiwiBackend.Dump().CallCalls["search_flights"])
}

type envoyConfigData struct {
	ProxyPort         int
	EgressPort        int
	AdminPort         int
	GitHubHost        string
	GitHubPort        string
	KiwiHost          string
	KiwiPort          string
	ClusterConfigJSON string
}

func initialize(t *testing.T, baseURL string) string {
	t.Helper()
	resp := postRPCRaw(t, baseURL, "", mcpprofilerouter.JSONRPCRequest{
		JSONRPC: "2.0",
		ID:      json.RawMessage(`1`),
		Method:  mcpprofilerouter.MethodInitialize,
		Params: mustRaw(mcpprofilerouter.InitializeParams{
			ProtocolVersion: mcpprofilerouter.ProtocolVersion,
			ClientInfo:      mcpprofilerouter.Implementation{Name: "e2e"},
		}),
	})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		var body bytes.Buffer
		_, _ = body.ReadFrom(resp.Body)
		require.Equal(t, http.StatusOK, resp.StatusCode, body.String())
	}
	sessionID := resp.Header.Get(mcpprofilerouter.SessionIDHeader)
	require.NotEmpty(t, sessionID)
	return sessionID
}

func postRPC(t *testing.T, baseURL, sessionID string, req mcpprofilerouter.JSONRPCRequest) mcpprofilerouter.JSONRPCResponse {
	t.Helper()
	resp := postRPCRaw(t, baseURL, sessionID, req)
	defer resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)
	var out mcpprofilerouter.JSONRPCResponse
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&out))
	return out
}

func postRPCRaw(t *testing.T, baseURL, sessionID string, req mcpprofilerouter.JSONRPCRequest) *http.Response {
	t.Helper()
	return postRPCRawWithHeaders(t, baseURL, sessionID, req, map[string]string{"authorization": "Bearer profile-key"})
}

func postRPCRawWithHeaders(t *testing.T, baseURL, sessionID string, req mcpprofilerouter.JSONRPCRequest, headers map[string]string) *http.Response {
	t.Helper()
	body, err := json.Marshal(req)
	require.NoError(t, err)
	httpReq, err := http.NewRequest(http.MethodPost, baseURL+"/mcp/9b3f7d0a80c4aa6d-67261ca9ea3dadb2", bytes.NewReader(body)) //nolint:noctx
	require.NoError(t, err)
	httpReq.Header.Set("content-type", "application/json")
	httpReq.Header.Set("accept", "application/json")
	if headers == nil {
		// If nil, set default auth
		httpReq.Header.Set("authorization", "Bearer profile-key")
	} else {
		// If non-nil (even if empty), use exactly what was provided
		for name, value := range headers {
			httpReq.Header.Set(name, value)
		}
	}
	if sessionID != "" {
		httpReq.Header.Set(mcpprofilerouter.SessionIDHeader, sessionID)
	}
	resp, err := http.DefaultClient.Do(httpReq)
	require.NoError(t, err)
	return resp
}

func mustRaw(value any) json.RawMessage {
	raw, err := json.Marshal(value)
	if err != nil {
		panic(err)
	}
	return raw
}

func marshalJSON(value any) string {
	raw, err := json.Marshal(value)
	if err != nil {
		panic(err)
	}
	return string(raw)
}

func mustTarget(t *testing.T, raw string) string {
	t.Helper()
	u, err := url.Parse(raw)
	require.NoError(t, err)
	return u.Host
}

func getBody(t *testing.T, url string) string {
	t.Helper()
	resp, err := http.Get(url) //nolint:noctx
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)
	var body bytes.Buffer
	_, err = body.ReadFrom(resp.Body)
	require.NoError(t, err)
	return body.String()
}
