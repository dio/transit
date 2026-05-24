// Package e2e runs integration tests for the wsproxy example against a real
// Envoy instance using a mock OpenAI upstream.
//
// Run:
//
//	make -C examples/wsproxy e2e
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
	envoyCmd     *exec.Cmd
	examplesRoot string
)

func TestMain(m *testing.M) {
	_, file, _, _ := runtime.Caller(0)
	// wsproxy/e2e/e2e_test.go -> examples/
	examplesRoot = filepath.Join(filepath.Dir(file), "../../..")

	bin := e2etest.EnvoyBin(examplesRoot)
	if _, err := os.Stat(bin); err != nil {
		fmt.Fprintf(os.Stderr, "SKIP: envoy not found at %s (run: make download-envoy)\n", bin)
		os.Exit(0)
	}
	if err := e2etest.CheckSharedLibrary(examplesRoot, "wsproxy", "libwsproxy.so"); err != nil {
		fmt.Fprintf(os.Stderr, "e2e: %v\n", err)
		os.Exit(1)
	}

	// Start mock OpenAI upstream.
	mockPort := startMockUpstream()

	// Allocate ports.
	proxyPort := e2etest.FreePort()
	loopbackPort = e2etest.FreePort()
	adminPort := e2etest.FreePort()

	proxyURL = fmt.Sprintf("ws://127.0.0.1:%d", proxyPort)
	adminURL = fmt.Sprintf("http://127.0.0.1:%d", adminPort)

	_ = mockPort // mock upstream is used by the embedded proxy in wsproxy.go,
	// but in tests we override OPENAI_API_KEY and the embedded proxy's upstreamURL
	// via env. For simplicity in this bootstrap: the mock runs on mockPort but
	// wsproxy.go dials DefaultUpstreamURL. Override via WSPROXY_UPSTREAM_URL.

	cfgPath := e2etest.WriteEnvoyConfig("wsproxy", envoyConfigTmpl, map[string]int{
		"ProxyPort":    proxyPort,
		"LoopbackPort": loopbackPort,
		"AdminPort":    adminPort,
	})

	wsproxyDir := filepath.Join(examplesRoot, "wsproxy")
	envoyCmd = exec.Command(bin, "-c", cfgPath, "--log-level", "warning",
		"--component-log-level", "dynamic_modules:info")
	envoyCmd.Env = append(os.Environ(),
		"GODEBUG=cgocheck=0",
		"ENVOY_DYNAMIC_MODULES_SEARCH_PATH="+wsproxyDir,
		"OPENAI_API_KEY=test-key",
		fmt.Sprintf("WSPROXY_LOOPBACK_ADDR=127.0.0.1:%d", loopbackPort),
		fmt.Sprintf("WSPROXY_UPSTREAM_URL=ws://127.0.0.1:%d", mockPort),
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

// startMockUpstream starts an in-process WebSocket server that simulates
// the OpenAI Responses API. Returns the port it listens on.
func startMockUpstream() int {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		panic("startMockUpstream: " + err.Error())
	}
	port := ln.Addr().(*net.TCPAddr).Port
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/responses", func(w http.ResponseWriter, r *http.Request) {
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
			// On response.create: send response.completed with token counts.
			var ev struct {
				Type string `json:"type"`
			}
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
				_ = wsjson.Write(ctx, conn, resp)
			}
			// Echo all frames back.
			_ = conn.Write(ctx, websocket.MessageText, data)
		}
	})
	go http.Serve(ln, mux) //nolint:errcheck
	return port
}

func TestWsProxy_MissingAuth_401(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	// Connect without Authorization header — should get HTTP 401, not a WS upgrade.
	_, resp, err := websocket.Dial(ctx, proxyURL+"/v1/responses", nil)
	if err == nil {
		t.Fatal("expected dial to fail with 401, got nil error")
	}
	if resp != nil && resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("want 401, got %d", resp.StatusCode)
	}
}

func TestWsProxy_NonWsRequest_Returns400(t *testing.T) {
	resp, err := http.Get("http://" + proxyURL[len("ws://"):] + "/v1/responses")
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusBadRequest, resp.StatusCode)
}

func TestWsProxy_ValidAuth_ExchangesFrames(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	conn, _, err := websocket.Dial(ctx, proxyURL+"/v1/responses", &websocket.DialOptions{
		HTTPHeader: http.Header{"Authorization": {"Bearer sk-test"}},
	})
	require.NoError(t, err)
	defer conn.CloseNow()

	// Send a response.create frame.
	req := map[string]any{
		"type":  "response.create",
		"model": "gpt-4.1",
		"input": []map[string]any{},
	}
	require.NoError(t, wsjson.Write(ctx, conn, req))

	// Read response.completed back (mock sends it on response.create).
	var ev map[string]any
	require.NoError(t, wsjson.Read(ctx, conn, &ev))
	require.Equal(t, "response.completed", ev["type"])
}

func TestWsProxy_FrameIntegrity(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	conn, _, err := websocket.Dial(ctx, proxyURL+"/v1/responses", &websocket.DialOptions{
		HTTPHeader: http.Header{"Authorization": {"Bearer sk-test"}},
	})
	require.NoError(t, err)
	defer conn.CloseNow()

	// Send 10 plain frames of varying sizes and verify they echo back intact.
	// Mock upstream echoes everything, so these come back after response.create handling.
	for i := 0; i < 10; i++ {
		payload := fmt.Sprintf(`{"type":"ping","seq":%d,"pad":"%s"}`, i, make([]byte, i*100))
		require.NoError(t, conn.Write(ctx, websocket.MessageText, []byte(payload)))
		_, data, err := conn.Read(ctx)
		require.NoError(t, err)
		require.Equal(t, payload, string(data))
	}
}
