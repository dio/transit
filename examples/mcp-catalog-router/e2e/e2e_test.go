package e2e

import (
	"bytes"
	"context"
	_ "embed"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/dio/transit/examples/internal/e2etest"
	mcpcatalogrouter "github.com/dio/transit/examples/mcp-catalog-router"
	mcpprofilerouter "github.com/dio/transit/examples/mcp-profile-router"
	"github.com/stretchr/testify/require"
)

//go:embed testdata/envoy.tmpl.yaml
var envoyConfigTmpl string

func TestCatalogRouterEnvoy(t *testing.T) {
	_, file, _, _ := runtime.Caller(0)
	examplesRoot := filepath.Join(filepath.Dir(file), "../../")
	envoyBin := e2etest.EnvoyBin(examplesRoot)
	if _, err := os.Stat(envoyBin); err != nil {
		t.Skipf("envoy not found at %s (run: make download-envoy)", envoyBin)
	}
	require.NoError(t, e2etest.CheckSharedLibrary(examplesRoot, "mcp-catalog-router", "libmcp-catalog-router.so"))

	var gotRoute string
	backend := mcpprofilerouter.NewBackend("aws-knowledge", []string{"aws____read_documentation"}, "Bearer aws-token")
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotRoute = r.Header.Get("x-mcp-server")
		require.Equal(t, "/mcp", r.URL.Path)
		backend.Handler().ServeHTTP(w, r)
	}))
	defer upstream.Close()

	configJSON, err := json.Marshal(mcpcatalogrouter.Config{
		Servers: map[string]mcpcatalogrouter.Server{
			"aws-knowledge": {URL: upstream.URL, Credential: "Bearer aws-token"},
		},
	})
	require.NoError(t, err)

	proxyPort := e2etest.FreePort()
	adminPort := e2etest.FreePort()
	cfgPath := e2etest.WriteEnvoyConfig("mcp-catalog-router-e2e", envoyConfigTmpl, map[string]int{
		"ProxyPort": proxyPort,
		"AdminPort": adminPort,
	})
	defer func() { _ = os.Remove(cfgPath) }()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cmd := exec.CommandContext(ctx, envoyBin, "-c", cfgPath, "--log-level", "warning", "--component-log-level", "dynamic_modules:info")
	cmd.Env = append(os.Environ(),
		"GODEBUG=cgocheck=0",
		"ENVOY_DYNAMIC_MODULES_SEARCH_PATH="+filepath.Join(examplesRoot, "mcp-catalog-router"),
		mcpcatalogrouter.ConfigEnv+"="+string(configJSON),
	)
	cmd.Stdout = os.Stderr
	cmd.Stderr = os.Stderr
	require.NoError(t, cmd.Start())
	defer func() {
		cancel()
		_ = cmd.Wait()
	}()

	require.Eventually(t, func() bool {
		return e2etest.WaitURL(fmt.Sprintf("http://127.0.0.1:%d/ready", adminPort), 200*time.Millisecond)
	}, 15*time.Second, 200*time.Millisecond)

	baseURL := fmt.Sprintf("http://127.0.0.1:%d/mcp/s/aws-knowledge", proxyPort)
	sessionID := initialize(t, baseURL)
	out := postRPC(t, baseURL, sessionID, mcpprofilerouter.JSONRPCRequest{
		JSONRPC: "2.0",
		ID:      json.RawMessage(`2`),
		Method:  mcpprofilerouter.MethodToolsList,
	})
	require.Nil(t, out.Error)
	require.Equal(t, "aws-knowledge", gotRoute)

	raw, err := json.Marshal(out.Result)
	require.NoError(t, err)
	var list mcpprofilerouter.ListToolsResult
	require.NoError(t, json.Unmarshal(raw, &list))
	require.Len(t, list.Tools, 1)
	require.Equal(t, "aws____read_documentation", list.Tools[0].Name)
}

func initialize(t *testing.T, url string) string {
	t.Helper()
	resp := postRPCRaw(t, url, "", mcpprofilerouter.JSONRPCRequest{
		JSONRPC: "2.0",
		ID:      json.RawMessage(`1`),
		Method:  mcpprofilerouter.MethodInitialize,
		Params: mustRaw(t, mcpprofilerouter.InitializeParams{
			ProtocolVersion: mcpprofilerouter.ProtocolVersion,
			ClientInfo:      mcpprofilerouter.Implementation{Name: "mcp-catalog-router-e2e"},
		}),
	})
	defer func() { _ = resp.Body.Close() }()
	require.Equal(t, http.StatusOK, resp.StatusCode)
	sessionID := resp.Header.Get(mcpprofilerouter.SessionIDHeader)
	require.NotEmpty(t, sessionID)
	return sessionID
}

func postRPC(t *testing.T, url, sessionID string, req mcpprofilerouter.JSONRPCRequest) mcpprofilerouter.JSONRPCResponse {
	t.Helper()
	resp := postRPCRaw(t, url, sessionID, req)
	defer func() { _ = resp.Body.Close() }()
	require.Equal(t, http.StatusOK, resp.StatusCode)
	var out mcpprofilerouter.JSONRPCResponse
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&out))
	return out
}

func postRPCRaw(t *testing.T, url, sessionID string, req mcpprofilerouter.JSONRPCRequest) *http.Response {
	t.Helper()
	body, err := json.Marshal(req)
	require.NoError(t, err)
	httpReq, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(body)) //nolint:noctx
	require.NoError(t, err)
	httpReq.Header.Set("content-type", "application/json")
	httpReq.Header.Set("accept", "application/json")
	if sessionID != "" {
		httpReq.Header.Set(mcpprofilerouter.SessionIDHeader, sessionID)
	}
	resp, err := http.DefaultClient.Do(httpReq)
	require.NoError(t, err)
	return resp
}

func mustRaw(t *testing.T, v any) json.RawMessage {
	t.Helper()
	raw, err := json.Marshal(v)
	require.NoError(t, err)
	return raw
}
