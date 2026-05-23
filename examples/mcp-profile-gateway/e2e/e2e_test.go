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
	"sync"
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

	var mu sync.Mutex
	gotPaths := map[string]int{}
	gotCredRefs := map[string]string{}
	gotSessions := map[string]string{}
	l2Listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	l2 := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		gotPaths[r.URL.Path]++
		gotCredRefs[r.URL.Path] = r.Header.Get("x-mcp-credential-ref")
		gotSessions[r.URL.Path] = r.Header.Get(mcpprofilerouter.SessionIDHeader)
		mu.Unlock()

		var req mcpprofilerouter.JSONRPCRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeJSON(w, http.StatusBadRequest, mcpprofilerouter.JSONRPCResponse{
				JSONRPC: "2.0",
				Error:   &mcpprofilerouter.JSONRPCError{Code: -32700, Message: "invalid JSON"},
			})
			return
		}
		server := pathBase(r.URL.Path)
		switch req.Method {
		case mcpprofilerouter.MethodInitialize:
			w.Header().Set(mcpprofilerouter.SessionIDHeader, "session-"+server)
			writeJSON(w, http.StatusOK, mcpprofilerouter.JSONRPCResponse{
				JSONRPC: "2.0",
				ID:      req.ID,
				Result: mcpprofilerouter.InitializeResult{
					ProtocolVersion: mcpprofilerouter.ProtocolVersion,
					Capabilities:    map[string]any{"tools": map[string]any{}},
					ServerInfo:      mcpprofilerouter.Implementation{Name: "l2-aws"},
				},
			})
		case mcpprofilerouter.MethodToolsList:
			writeJSON(w, http.StatusOK, mcpprofilerouter.JSONRPCResponse{
				JSONRPC: "2.0",
				ID:      req.ID,
				Result: mcpprofilerouter.ListToolsResult{Tools: []mcpprofilerouter.Tool{
					{Name: server + "_tool", Description: server, InputSchema: map[string]any{"type": "object"}},
				}},
			})
		default:
			writeJSON(w, http.StatusBadRequest, mcpprofilerouter.JSONRPCResponse{
				JSONRPC: "2.0",
				ID:      req.ID,
				Error:   &mcpprofilerouter.JSONRPCError{Code: -32601, Message: "unsupported"},
			})
		}
	}))
	l2.Listener = l2Listener
	l2.Start()
	defer l2.Close()
	l2URL, err := url.Parse(l2.URL)
	require.NoError(t, err)

	configJSON, err := json.Marshal(mcpprofilegateway.Config{
		CatalogServers: map[string]mcpprofilegateway.CatalogServer{
			"aws-knowledge": {URL: "http://l2-catalog.local", Cluster: "l2-catalog"},
			"github":        {URL: "http://l2-catalog.local", Cluster: "l2-catalog"},
		},
		Profiles: map[string]mcpprofilegateway.Profile{
			"profile": {
				Name:   "profile",
				APIKey: "profile-key",
				Servers: map[string]mcpprofilegateway.ProfileServer{
					"aws-knowledge": {
						URL:           "http://l2-catalog.local",
						Prefix:        "aws",
						CredentialRef: "profile/aws/user",
					},
					"github": {
						URL:    "http://l2-catalog.local",
						Prefix: "github",
					},
				},
			},
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
	require.Equal(t, "session-aws-knowledge", resp.Header.Get(mcpprofilerouter.SessionIDHeader))
	mu.Lock()
	require.GreaterOrEqual(t, gotPaths["/mcp/s/aws-knowledge"], 1)
	mu.Unlock()

	profileURL := fmt.Sprintf("http://127.0.0.1:%d/mcp/profile", proxyPort)
	resp = postRPCRaw(t, profileURL, mcpprofilerouter.JSONRPCRequest{
		JSONRPC: "2.0",
		ID:      json.RawMessage(`2`),
		Method:  mcpprofilerouter.MethodInitialize,
		Params: mustRaw(t, mcpprofilerouter.InitializeParams{
			ProtocolVersion: mcpprofilerouter.ProtocolVersion,
			ClientInfo:      mcpprofilerouter.Implementation{Name: "mcp-profile-gateway-e2e"},
		}),
	}, map[string]string{"authorization": "Bearer profile-key"})
	defer func() { _ = resp.Body.Close() }()

	body, err = io.ReadAll(resp.Body)
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, resp.StatusCode, string(body))
	profileSessionID := resp.Header.Get(mcpprofilerouter.SessionIDHeader)
	require.NotEmpty(t, profileSessionID)
	require.NotEqual(t, "session-aws-knowledge", profileSessionID)

	resp = postRPCRaw(t, profileURL, mcpprofilerouter.JSONRPCRequest{
		JSONRPC: "2.0",
		ID:      json.RawMessage(`3`),
		Method:  mcpprofilerouter.MethodToolsList,
	}, map[string]string{
		"authorization":                  "Bearer profile-key",
		mcpprofilerouter.SessionIDHeader: profileSessionID,
	})
	defer func() { _ = resp.Body.Close() }()

	body, err = io.ReadAll(resp.Body)
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, resp.StatusCode, string(body))
	var rpc mcpprofilerouter.JSONRPCResponse
	require.NoError(t, json.Unmarshal(body, &rpc))
	raw, err := json.Marshal(rpc.Result)
	require.NoError(t, err)
	var list mcpprofilerouter.ListToolsResult
	require.NoError(t, json.Unmarshal(raw, &list))
	require.Len(t, list.Tools, 2)
	require.Equal(t, []string{"aws.aws-knowledge_tool", "github.github_tool"}, []string{list.Tools[0].Name, list.Tools[1].Name})
	mu.Lock()
	require.GreaterOrEqual(t, gotPaths["/mcp/s/github"], 1)
	require.Equal(t, "profile/aws/user", gotCredRefs["/mcp/s/aws-knowledge"])
	require.Equal(t, "session-aws-knowledge", gotSessions["/mcp/s/aws-knowledge"])
	require.Equal(t, "session-github", gotSessions["/mcp/s/github"])
	mu.Unlock()
}

func TestProfileGatewayEnvoyToolsCall(t *testing.T) {
	_, file, _, _ := runtime.Caller(0)
	examplesRoot := filepath.Join(filepath.Dir(file), "../../")
	envoyBin := e2etest.EnvoyBin(examplesRoot)
	if _, err := os.Stat(envoyBin); err != nil {
		t.Skipf("envoy not found at %s (run: make download-envoy)", envoyBin)
	}
	require.NoError(t, e2etest.CheckSharedLibrary(examplesRoot, "mcp-profile-gateway", "libmcp-profile-gateway.so"))

	var mu sync.Mutex
	type callRecord struct {
		method    string
		toolName  string
		sessionID string
		credRef   string
	}
	var calls []callRecord

	l2Listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	l2 := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req mcpprofilerouter.JSONRPCRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeJSON(w, http.StatusBadRequest, mcpprofilerouter.JSONRPCResponse{
				JSONRPC: "2.0",
				Error:   &mcpprofilerouter.JSONRPCError{Code: -32700, Message: "invalid JSON"},
			})
			return
		}
		server := pathBase(r.URL.Path)
		rec := callRecord{
			method:    req.Method,
			sessionID: r.Header.Get(mcpprofilerouter.SessionIDHeader),
			credRef:   r.Header.Get("x-mcp-credential-ref"),
		}
		if req.Method == mcpprofilerouter.MethodToolsCall {
			var p mcpprofilerouter.CallToolParams
			_ = json.Unmarshal(req.Params, &p)
			rec.toolName = p.Name
		}
		mu.Lock()
		calls = append(calls, rec)
		mu.Unlock()

		switch req.Method {
		case mcpprofilerouter.MethodInitialize:
			w.Header().Set(mcpprofilerouter.SessionIDHeader, "session-"+server)
			writeJSON(w, http.StatusOK, mcpprofilerouter.JSONRPCResponse{
				JSONRPC: "2.0",
				ID:      req.ID,
				Result: mcpprofilerouter.InitializeResult{
					ProtocolVersion: mcpprofilerouter.ProtocolVersion,
					Capabilities:    map[string]any{"tools": map[string]any{}},
					ServerInfo:      mcpprofilerouter.Implementation{Name: "l2-" + server},
				},
			})
		case mcpprofilerouter.MethodToolsCall:
			var p mcpprofilerouter.CallToolParams
			_ = json.Unmarshal(req.Params, &p)
			writeJSON(w, http.StatusOK, mcpprofilerouter.JSONRPCResponse{
				JSONRPC: "2.0",
				ID:      req.ID,
				Result: mcpprofilerouter.CallToolResult{
					Content: []mcpprofilerouter.ContentBlock{{Type: "text", Text: "called " + server + "." + p.Name}},
				},
			})
		default:
			writeJSON(w, http.StatusBadRequest, mcpprofilerouter.JSONRPCResponse{
				JSONRPC: "2.0",
				ID:      req.ID,
				Error:   &mcpprofilerouter.JSONRPCError{Code: -32601, Message: "unsupported"},
			})
		}
	}))
	l2.Listener = l2Listener
	l2.Start()
	defer l2.Close()
	l2URL, err := url.Parse(l2.URL)
	require.NoError(t, err)

	configJSON, err := json.Marshal(mcpprofilegateway.Config{
		CatalogServers: map[string]mcpprofilegateway.CatalogServer{
			"aws-knowledge": {URL: "http://l2-catalog.local", Cluster: "l2-catalog"},
			"github":        {URL: "http://l2-catalog.local", Cluster: "l2-catalog"},
		},
		Profiles: map[string]mcpprofilegateway.Profile{
			"profile": {
				Name:   "profile",
				APIKey: "profile-key",
				Servers: map[string]mcpprofilegateway.ProfileServer{
					"aws-knowledge": {
						URL:           "http://l2-catalog.local",
						Prefix:        "aws",
						CredentialRef: "profile/aws/user",
					},
					"github": {
						URL:    "http://l2-catalog.local",
						Prefix: "github",
					},
				},
			},
		},
	})
	require.NoError(t, err)

	proxyPort := e2etest.FreePort()
	adminPort := e2etest.FreePort()
	cfgPath := e2etest.WriteEnvoyConfig("mcp-profile-gateway-toolscall-e2e", envoyConfigTmpl, envoyConfigData{
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

	profileURL := fmt.Sprintf("http://127.0.0.1:%d/mcp/profile", proxyPort)

	// initialize to get a profile session
	initResp := postRPCRaw(t, profileURL, mcpprofilerouter.JSONRPCRequest{
		JSONRPC: "2.0",
		ID:      json.RawMessage(`1`),
		Method:  mcpprofilerouter.MethodInitialize,
		Params: mustRaw(t, mcpprofilerouter.InitializeParams{
			ProtocolVersion: mcpprofilerouter.ProtocolVersion,
			ClientInfo:      mcpprofilerouter.Implementation{Name: "mcp-profile-gateway-e2e"},
		}),
	}, map[string]string{"authorization": "Bearer profile-key"})
	defer func() { _ = initResp.Body.Close() }()
	initBody, err := io.ReadAll(initResp.Body)
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, initResp.StatusCode, string(initBody))
	profileSessionID := initResp.Header.Get(mcpprofilerouter.SessionIDHeader)
	require.NotEmpty(t, profileSessionID)

	mu.Lock()
	initCallCount := len(calls)
	mu.Unlock()
	require.Equal(t, 2, initCallCount, "initialize must fan out to both backends")

	// tools/call github.search — only github backend should be called
	mu.Lock()
	calls = calls[:0]
	mu.Unlock()

	callResp := postRPCRaw(t, profileURL, mcpprofilerouter.JSONRPCRequest{
		JSONRPC: "2.0",
		ID:      json.RawMessage(`2`),
		Method:  mcpprofilerouter.MethodToolsCall,
		Params:  mustRaw(t, mcpprofilerouter.CallToolParams{Name: "github.search", Arguments: map[string]any{"q": "transit"}}),
	}, map[string]string{
		"authorization":                  "Bearer profile-key",
		mcpprofilerouter.SessionIDHeader: profileSessionID,
	})
	defer func() { _ = callResp.Body.Close() }()
	callBody, err := io.ReadAll(callResp.Body)
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, callResp.StatusCode, string(callBody))

	var rpc mcpprofilerouter.JSONRPCResponse
	require.NoError(t, json.Unmarshal(callBody, &rpc))
	require.Nil(t, rpc.Error, string(callBody))

	mu.Lock()
	toolCallCalls := make([]callRecord, len(calls))
	copy(toolCallCalls, calls)
	mu.Unlock()

	require.Len(t, toolCallCalls, 1, "only the github backend must be called")
	require.Equal(t, mcpprofilerouter.MethodToolsCall, toolCallCalls[0].method)
	require.Equal(t, "search", toolCallCalls[0].toolName, "prefix must be stripped before forwarding to L2")
	require.Equal(t, "session-github", toolCallCalls[0].sessionID)
	require.Empty(t, toolCallCalls[0].credRef, "github server has no credential_ref")
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

func pathBase(path string) string {
	if i := len(path) - 1; i >= 0 {
		for ; i >= 0; i-- {
			if path[i] == '/' {
				return path[i+1:]
			}
		}
	}
	return path
}
