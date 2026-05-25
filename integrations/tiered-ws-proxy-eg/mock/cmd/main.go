// Package main is the mock WS upstream for the tiered-ws-proxy-eg integration.
// It accepts WS upgrades on /v1/responses and:
//   - replies to response.create frames with a synthetic response.completed
//     (fixed usage: 100 input, 42 output tokens) so Gate 2 and Gate 3 can
//     assert on exact token counts without a real API key;
//   - echoes every other frame back unchanged.
package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"os"

	"github.com/coder/websocket"
)

const (
	fixedInputTokens  = uint32(100)
	fixedOutputTokens = uint32(42)
)

func main() {
	addr := ":8080"
	if v := os.Getenv("MOCK_LISTEN_ADDR"); v != "" {
		addr = v
	}
	http.HandleFunc("/v1/responses", handle)
	slog.Info("mock-upstream listening", "addr", addr)
	if err := http.ListenAndServe(addr, nil); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

var createMarker = []byte(`"response.create"`)

func handle(w http.ResponseWriter, r *http.Request) {
	conn, err := websocket.Accept(w, r, nil)
	if err != nil {
		slog.Error("accept", "err", err)
		return
	}
	defer conn.CloseNow()

	ctx := r.Context()
	for {
		typ, data, err := conn.Read(ctx)
		if err != nil {
			return
		}
		if typ == websocket.MessageText && bytes.Contains(data, createMarker) {
			if err := conn.Write(ctx, websocket.MessageText, buildCompleted(extractModel(data))); err != nil {
				return
			}
			continue
		}
		if err := conn.Write(ctx, typ, data); err != nil {
			return
		}
	}
}

func extractModel(data []byte) string {
	var f struct {
		Model string `json:"model"`
	}
	if json.Unmarshal(data, &f) == nil && f.Model != "" {
		return f.Model
	}
	return "unknown"
}

func buildCompleted(model string) []byte {
	b, _ := json.Marshal(map[string]any{
		"type": "response.completed",
		"response": map[string]any{
			"id":    "resp_mock_001",
			"model": model,
			"usage": map[string]any{
				"input_tokens":  fixedInputTokens,
				"output_tokens": fixedOutputTokens,
				"total_tokens":  fixedInputTokens + fixedOutputTokens,
			},
		},
	})
	return b
}
