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
	"testing"
	"time"

	"github.com/dio/transit/examples/internal/e2etest"
	mcpprofilerouter "github.com/dio/transit/examples/mcp-profile-router"
	"github.com/stretchr/testify/require"
)

//go:embed testdata/envoy.tmpl.yaml
var envoyConfigTmpl string

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
	profile := mcpprofilerouter.Profile{
		Name:          "engineering",
		APIKey:        "profile-key",
		TimeoutMillis: 500,
		Servers: map[string]mcpprofilerouter.Server{
			"github": {URL: proxyURL + "/_egress/github", Prefix: "github", Credential: "Bearer github-token"},
			"kiwi":   {URL: proxyURL + "/_egress/kiwi", Prefix: "kiwi", Credential: "Bearer kiwi-token"},
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

	envoy := exec.Command(bin, "-c", cfgPath, "--log-level", "warning")
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
	list := postRPC(t, proxyURL, sessionID, mcpprofilerouter.JSONRPCRequest{
		JSONRPC: "2.0",
		ID:      json.RawMessage(`2`),
		Method:  mcpprofilerouter.MethodToolsList,
	})
	must.Nil(list.Error)
	raw, err := json.Marshal(list.Result)
	must.NoError(err)
	must.Contains(string(raw), "github.search")
	must.Contains(string(raw), "kiwi.search_flights")

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

type envoyConfigData struct {
	ProxyPort  int
	AdminPort  int
	GitHubHost string
	GitHubPort string
	KiwiHost   string
	KiwiPort   string
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
	body, err := json.Marshal(req)
	require.NoError(t, err)
	httpReq, err := http.NewRequest(http.MethodPost, baseURL+"/mcp/profiles/engineering", bytes.NewReader(body)) //nolint:noctx
	require.NoError(t, err)
	httpReq.Header.Set("content-type", "application/json")
	httpReq.Header.Set("accept", "application/json")
	httpReq.Header.Set("authorization", "Bearer profile-key")
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
