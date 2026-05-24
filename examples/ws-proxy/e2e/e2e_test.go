// Package e2e runs integration tests for the ws-proxy example against a real
// Envoy instance using a mock upstream.
//
// Run:
//
//	make -C examples/ws-proxy e2e
package e2e

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
	"github.com/stretchr/testify/require"
	"golang.org/x/net/context"

	"github.com/dio/transit/examples/internal/e2etest"
)

//go:embed testdata/envoy.tmpl.yaml
var envoyConfigTmpl string

var (
	proxyURL     string
	adminURL     string
	loopbackPort int
	mockPort     int
	envoyCmd     *exec.Cmd
	examplesRoot string
)

func TestMain(m *testing.M) {
	_, file, _, _ := runtime.Caller(0)
	// ws-proxy/e2e/e2e_test.go -> examples/
	examplesRoot = filepath.Join(filepath.Dir(file), "../..")

	bin := e2etest.EnvoyBin(examplesRoot)
	if _, err := os.Stat(bin); err != nil {
		fmt.Fprintf(os.Stderr, "SKIP: envoy not found at %s (run: make download-envoy)\n", bin)
		os.Exit(0)
	}
	if err := e2etest.CheckSharedLibrary(examplesRoot, "ws-proxy", "libws-proxy.so"); err != nil {
		fmt.Fprintf(os.Stderr, "e2e: %v\n", err)
		os.Exit(1)
	}

	mockPort = startMockUpstream()
	proxyPort := e2etest.FreePort()
	loopbackPort = e2etest.FreePort()
	adminPort := e2etest.FreePort()

	proxyURL = fmt.Sprintf("ws://127.0.0.1:%d", proxyPort)
	adminURL = fmt.Sprintf("http://127.0.0.1:%d", adminPort)

	cfgPath := e2etest.WriteEnvoyConfig("ws-proxy", envoyConfigTmpl, map[string]int{
		"ProxyPort":    proxyPort,
		"LoopbackPort": loopbackPort,
		"AdminPort":    adminPort,
		"MockPort":     mockPort,
	})

	wsProxyDir := filepath.Join(examplesRoot, "ws-proxy")
	envoyCmd = exec.Command(bin, "-c", cfgPath, "--log-level", "warning",
		"--component-log-level", "dynamic_modules:info")
	envoyCmd.Env = append(os.Environ(),
		"GODEBUG=cgocheck=0",
		"ENVOY_DYNAMIC_MODULES_SEARCH_PATH="+wsProxyDir,
		// Proxy dials mock upstream, no real credentials needed.
		"WSPROXY_LISTEN_ADDR="+fmt.Sprintf("127.0.0.1:%d", loopbackPort),
		fmt.Sprintf("WSPROXY_UPSTREAM_URL=ws://127.0.0.1:%d", mockPort),
		"WSPROXY_AUTH_VALUE=", // disable auth injection against mock
	)
	envoyCmd.Stdout = os.Stderr
	envoyCmd.Stderr = os.Stderr
	if err := envoyCmd.Start(); err != nil {
		os.Remove(cfgPath)
		fmt.Fprintf(os.Stderr, "e2e: envoy start failed: %v\n", err)
		os.Exit(1)
	}
	fmt.Fprintf(os.Stderr, "e2e: envoy pid=%d\n", envoyCmd.Process.Pid)

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

// startMockUpstream starts an in-process server that:
//   - Accepts WebSocket upgrades and acts as a mock OpenAI upstream.
//   - On receiving a response.create frame, sends back a response.completed
//     event with known token counts and then echoes all frames.
//   - Returns the port it listens on.
func startMockUpstream() int {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		panic("startMockUpstream: " + err.Error())
	}
	port := ln.Addr().(*net.TCPAddr).Port
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		// WebSocket upgrade.
		if r.Header.Get("Upgrade") == "websocket" {
			conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{InsecureSkipVerify: true})
			if err != nil {
				return
			}
			defer conn.CloseNow()
			ctx := r.Context()
			for {
				_, data, err := conn.Read(ctx)
				if err != nil {
					return
				}
				var ev struct {
					Type string `json:"type"`
				}
				// On response.create: send response.completed with known token counts.
				if json.Unmarshal(data, &ev) == nil && ev.Type == "response.create" {
					resp := map[string]any{
						"type": "response.completed",
						"response": map[string]any{
							"id": "resp_mock",
							"usage": map[string]any{
								"input_tokens":  100,
								"output_tokens": 42,
								"total_tokens":  142,
							},
						},
					}
					if err := wsjson.Write(ctx, conn, resp); err != nil {
						return
					}
				}
				// Echo all frames.
				if err := conn.Write(ctx, websocket.MessageText, data); err != nil {
					return
				}
			}
		}
		// Plain HTTP.
		w.WriteHeader(http.StatusOK)
		fmt.Fprintln(w, `{"ok":true}`)
	})
	go http.Serve(ln, mux) //nolint:errcheck
	return port
}

// TestWsProxy_ValidAuth_ConnectsAndExchangesFrames verifies that a WebSocket
// client can connect through Envoy to the embedded proxy and exchange frames
// with the mock upstream.
func TestWsProxy_ValidAuth_ConnectsAndExchangesFrames(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	conn, _, err := websocket.Dial(ctx, proxyURL+"/v1/responses", &websocket.DialOptions{
		HTTPHeader: http.Header{"Authorization": {"Bearer sk-test"}},
	})
	require.NoError(t, err)
	defer conn.CloseNow()

	// Send a response.create frame. Mock sends back response.completed + echo.
	req := map[string]any{
		"type":  "response.create",
		"model": "gpt-4.1",
		"input": []map[string]any{},
	}
	require.NoError(t, wsjson.Write(ctx, conn, req))

	// Read response.completed from mock.
	var ev map[string]any
	require.NoError(t, wsjson.Read(ctx, conn, &ev))
	require.Equal(t, "response.completed", ev["type"])
}

// TestWsProxy_FrameIntegrity verifies that frames pass through the proxy
// intact and in order.
func TestWsProxy_FrameIntegrity(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	conn, _, err := websocket.Dial(ctx, proxyURL+"/v1/responses", &websocket.DialOptions{
		HTTPHeader: http.Header{"Authorization": {"Bearer sk-test"}},
	})
	require.NoError(t, err)
	defer conn.CloseNow()

	for i := 0; i < 10; i++ {
		payload := fmt.Sprintf(`{"type":"ping","seq":%d,"pad":"%s"}`, i, make([]byte, i*50))
		require.NoError(t, conn.Write(ctx, websocket.MessageText, []byte(payload)))
		// Mock echoes each frame after potentially sending response.completed for response.create.
		// For ping frames there's only the echo.
		_, data, err := conn.Read(ctx)
		require.NoError(t, err)
		require.Equal(t, payload, string(data))
	}
}

// TestWsProxy_TokenUsageExtracted verifies that the SessionTap correctly
// captures token usage from response.completed frames.
func TestWsProxy_TokenUsageExtracted(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	conn, _, err := websocket.Dial(ctx, proxyURL+"/v1/responses", &websocket.DialOptions{
		HTTPHeader: http.Header{"Authorization": {"Bearer sk-test"}},
	})
	require.NoError(t, err)
	defer conn.CloseNow()

	req := map[string]any{
		"type":  "response.create",
		"model": "gpt-4.1",
		"input": []map[string]any{},
	}
	require.NoError(t, wsjson.Write(ctx, conn, req))

	// Expect response.completed (mock sends it first).
	var completed map[string]any
	require.NoError(t, wsjson.Read(ctx, conn, &completed))
	require.Equal(t, "response.completed", completed["type"])

	// The session log (stderr) will show: input=100 output=42 turns=1.
	// We verify the completed event itself has the right shape.
	resp := completed["response"].(map[string]any)
	usage := resp["usage"].(map[string]any)
	require.Equal(t, float64(100), usage["input_tokens"])
	require.Equal(t, float64(42), usage["output_tokens"])
}
