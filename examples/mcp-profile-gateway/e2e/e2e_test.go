package e2e

import (
	"bytes"
	"context"
	_ "embed"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"testing"
	"time"

	"github.com/dio/transit/examples/internal/e2etest"
	mcpprofilegateway "github.com/dio/transit/examples/mcp-profile-gateway"
	mcpprofilerouter "github.com/dio/transit/examples/mcp-profile-router"
	"github.com/stretchr/testify/require"
)

//go:embed testdata/envoy.tmpl.yaml
var envoyConfigTmpl string

func TestProfileGatewayEnvoyCatalogForwarding(t *testing.T) {
	_, file, _, _ := runtime.Caller(0)
	examplesRoot := filepath.Join(filepath.Dir(file), "../../")
	envoyBin := e2etest.EnvoyBin(examplesRoot)
	if _, err := os.Stat(envoyBin); err != nil {
		t.Skipf("envoy not found at %s (run: make download-envoy)", envoyBin)
	}
	require.NoError(t, e2etest.CheckSharedLibrary(examplesRoot, "mcp-profile-gateway", "libmcp-profile-gateway.so"))

	var gotPath, gotCredRef string
	l2Listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	l2 := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotCredRef = r.Header.Get("x-mcp-credential-ref")
		w.Header().Set(mcpprofilerouter.SessionIDHeader, "l2-session")
		writeJSON(w, http.StatusOK, mcpprofilerouter.JSONRPCResponse{
			JSONRPC: "2.0",
			ID:      json.RawMessage(`1`),
			Result: mcpprofilerouter.InitializeResult{
				ProtocolVersion: mcpprofilerouter.ProtocolVersion,
				Capabilities:    map[string]any{"tools": map[string]any{}},
				ServerInfo:      mcpprofilerouter.Implementation{Name: "l2-aws"},
			},
		})
	}))
	l2.Listener = l2Listener
	l2.Start()
	defer l2.Close()
	l2URL, err := url.Parse(l2.URL)
	require.NoError(t, err)

	configJSON, err := json.Marshal(mcpprofilegateway.Config{
		CatalogServers: map[string]mcpprofilegateway.CatalogServer{
			"aws-knowledge": {URL: "http://l2-catalog.local", Cluster: "l2-catalog"},
		},
	})
	require.NoError(t, err)

	proxyPort := e2etest.FreePort()
	adminPort := e2etest.FreePort()
	cfgPath := e2etest.WriteEnvoyConfig("mcp-profile-gateway-e2e", envoyConfigTmpl, envoyConfigData{
		ProxyPort: proxyPort,
		AdminPort: adminPort,
		L2Host:    l2URL.Hostname(),
		L2Port:    mustAtoi(t, l2URL.Port()),
	})
	defer func() { _ = os.Remove(cfgPath) }()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cmd := exec.CommandContext(ctx, envoyBin, "-c", cfgPath, "--log-level", "warning", "--component-log-level", "dynamic_modules:info")
	cmd.Env = append(os.Environ(),
		"GODEBUG=cgocheck=0",
		"ENVOY_DYNAMIC_MODULES_SEARCH_PATH="+filepath.Join(examplesRoot, "mcp-profile-gateway"),
		mcpprofilegateway.ConfigEnv+"="+string(configJSON),
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
	resp := postRPCRaw(t, baseURL, mcpprofilerouter.JSONRPCRequest{
		JSONRPC: "2.0",
		ID:      json.RawMessage(`1`),
		Method:  mcpprofilerouter.MethodInitialize,
		Params: mustRaw(t, mcpprofilerouter.InitializeParams{
			ProtocolVersion: mcpprofilerouter.ProtocolVersion,
			ClientInfo:      mcpprofilerouter.Implementation{Name: "mcp-profile-gateway-e2e"},
		}),
	}, map[string]string{
		"x-mcp-credential-ref": "profile/aws/user",
	})
	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, resp.StatusCode, string(body))
	require.Equal(t, "l2-session", resp.Header.Get(mcpprofilerouter.SessionIDHeader))
	require.Equal(t, "/mcp/s/aws-knowledge", gotPath)
	require.Empty(t, gotCredRef)
}

func postRPCRaw(t *testing.T, url string, req mcpprofilerouter.JSONRPCRequest, headers map[string]string) *http.Response {
	t.Helper()
	body, err := json.Marshal(req)
	require.NoError(t, err)
	httpReq, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(body)) //nolint:noctx
	require.NoError(t, err)
	httpReq.Header.Set("content-type", "application/json")
	httpReq.Header.Set("accept", "application/json")
	for name, value := range headers {
		httpReq.Header.Set(name, value)
	}
	resp, err := http.DefaultClient.Do(httpReq)
	require.NoError(t, err)
	return resp
}

type envoyConfigData struct {
	ProxyPort int
	AdminPort int
	L2Host    string
	L2Port    int
}

func mustRaw(t *testing.T, v any) json.RawMessage {
	t.Helper()
	raw, err := json.Marshal(v)
	require.NoError(t, err)
	return raw
}

func mustAtoi(t *testing.T, s string) int {
	t.Helper()
	v, err := strconv.Atoi(s)
	require.NoError(t, err)
	return v
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("content-type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
