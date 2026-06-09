package server

// egress_serve_local_llm.go — "llm" commands for the egress:local REPL.
//
// Mirrors the demos/llm script but runs in-process so there is no curl
// dependency and output stays in the same readline session.
//
// Commands (dispatched by cmdLLM):
//
//	llm [<message>]          POST /v1/chat/completions  (OpenAI compat)
//	llm resp <message>       POST /v1/responses          (OpenAI Responses API)
//	llm msg <message>        POST /v1/messages           (Anthropic)
//	llm stream <message>     streaming chat/completions
//	llm models               GET  /v1/models
//	llm set key   <slug>     switch active key
//	llm set model <name>     pin model (empty = auto per API)
//	llm set base  <url>      change proxy base URL
//	llm set stream on|off    toggle default streaming
//	llm status               show current settings + keys from config

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/dio/transit/examples/orange/internal/rls"
)

// llmDefaultChat is the default model for /v1/chat/completions.
const llmDefaultChat = "gpt-4o-mini"

// llmDefaultResponses is the default model for /v1/responses.
const llmDefaultResponses = "gpt-4o-mini"

// llmDefaultMessages is the default model for /v1/messages.
const llmDefaultMessages = "claude-haiku-4-5"

// cmdLLM routes "llm ..." subcommands.
func (s *serveLocalState) cmdLLM(ctx context.Context, args []string) error {
	if len(args) == 0 {
		return s.llmStatus()
	}
	switch args[0] {
	case "models":
		return s.llmDoModels(ctx)

	case "resp", "responses":
		msg := strings.Join(args[1:], " ")
		if msg == "" {
			return fmt.Errorf("usage: llm resp <message>")
		}
		return s.llmDoResponses(ctx, msg, s.llmStream)

	case "msg", "messages":
		msg := strings.Join(args[1:], " ")
		if msg == "" {
			return fmt.Errorf("usage: llm msg <message>")
		}
		return s.llmDoMessages(ctx, msg, s.llmStream)

	case "stream":
		msg := strings.Join(args[1:], " ")
		if msg == "" {
			return fmt.Errorf("usage: llm stream <message>")
		}
		return s.llmDoChat(ctx, msg, true)

	case "set":
		return s.llmSet(args[1:])

	case "status":
		return s.llmStatus()

	case "help", "?":
		printLLMHelp()
		return nil

	default:
		// Any other text is the message.
		return s.llmDoChat(ctx, strings.Join(args, " "), s.llmStream)
	}
}

// ── chat/completions ──────────────────────────────────────────────────────────

func (s *serveLocalState) llmDoChat(ctx context.Context, message string, stream bool) error {
	model := s.llmModel
	if model == "" {
		model = llmDefaultChat
	}
	payload := map[string]any{
		"model":    model,
		"stream":   stream,
		"messages": []map[string]any{{"role": "user", "content": message}},
	}
	fmt.Fprintf(os.Stderr, "→ POST %s/v1/chat/completions  key=%s model=%s stream=%v\n",
		s.llmBaseURL, s.llmKey, model, stream)
	resp, elapsed, err := s.llmRequest(ctx, "POST", "/v1/chat/completions", payload)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if stream {
		return llmStreamChat(resp)
	}
	return llmPrintChat(resp, elapsed)
}

func llmPrintChat(resp *http.Response, elapsed time.Duration) error {
	fmt.Fprintf(os.Stderr, "← HTTP %d  %.0fms\n", resp.StatusCode, float64(elapsed.Milliseconds()))
	result, err := decodeJSONBody(resp.Body, resp.Status)
	if err != nil {
		return err
	}
	if resp.StatusCode != http.StatusOK {
		return llmPrintError(result)
	}
	if choices, ok := result["choices"].([]any); ok && len(choices) > 0 {
		if choice, ok := choices[0].(map[string]any); ok {
			if msg, ok := choice["message"].(map[string]any); ok {
				fmt.Println(msg["content"])
			}
		}
	}
	llmPrintUsage(result)
	return nil
}

func llmStreamChat(resp *http.Response) error {
	fmt.Fprintf(os.Stderr, "← HTTP %d  (streaming)\n", resp.StatusCode)
	if resp.StatusCode != http.StatusOK {
		var result map[string]any
		_ = json.NewDecoder(resp.Body).Decode(&result)
		return llmPrintError(result)
	}
	scanner := bufio.NewScanner(resp.Body)
	for scanner.Scan() {
		line := scanner.Text()
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		data := strings.TrimPrefix(line, "data: ")
		if data == "[DONE]" {
			fmt.Println()
			break
		}
		var chunk map[string]any
		if err := json.Unmarshal([]byte(data), &chunk); err != nil {
			continue
		}
		if choices, ok := chunk["choices"].([]any); ok && len(choices) > 0 {
			if choice, ok := choices[0].(map[string]any); ok {
				if delta, ok := choice["delta"].(map[string]any); ok {
					if content, ok := delta["content"].(string); ok {
						fmt.Print(content)
					}
				}
				// finish_reason signals end of stream
				if fr, ok := choice["finish_reason"].(string); ok && fr != "" && fr != "null" {
					fmt.Println()
				}
			}
		}
	}
	return scanner.Err()
}

// ── responses (OpenAI Responses API) ─────────────────────────────────────────

func (s *serveLocalState) llmDoResponses(ctx context.Context, message string, stream bool) error {
	model := s.llmModel
	if model == "" {
		model = llmDefaultResponses
	}
	payload := map[string]any{
		"model":  model,
		"stream": stream,
		"input":  message,
	}
	fmt.Fprintf(os.Stderr, "→ POST %s/v1/responses  key=%s model=%s stream=%v\n",
		s.llmBaseURL, s.llmKey, model, stream)
	resp, elapsed, err := s.llmRequest(ctx, "POST", "/v1/responses", payload)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if stream {
		return llmStreamResponses(resp)
	}
	return llmPrintResponses(resp, elapsed)
}

func llmPrintResponses(resp *http.Response, elapsed time.Duration) error {
	fmt.Fprintf(os.Stderr, "← HTTP %d  %.0fms\n", resp.StatusCode, float64(elapsed.Milliseconds()))
	result, err := decodeJSONBody(resp.Body, resp.Status)
	if err != nil {
		return err
	}
	if resp.StatusCode != http.StatusOK {
		return llmPrintError(result)
	}
	// Responses API: output[].content[].text where type=="output_text"
	if output, ok := result["output"].([]any); ok {
		for _, item := range output {
			m, ok := item.(map[string]any)
			if !ok {
				continue
			}
			content, ok := m["content"].([]any)
			if !ok {
				continue
			}
			for _, block := range content {
				b, ok := block.(map[string]any)
				if !ok {
					continue
				}
				if b["type"] == "output_text" {
					if text, ok := b["text"].(string); ok {
						fmt.Println(text)
					}
				}
			}
		}
	}
	llmPrintUsage(result)
	return nil
}

func llmStreamResponses(resp *http.Response) error {
	fmt.Fprintf(os.Stderr, "← HTTP %d  (streaming)\n", resp.StatusCode)
	if resp.StatusCode != http.StatusOK {
		var result map[string]any
		_ = json.NewDecoder(resp.Body).Decode(&result)
		return llmPrintError(result)
	}
	scanner := bufio.NewScanner(resp.Body)
	var eventType string
	for scanner.Scan() {
		line := scanner.Text()
		if strings.HasPrefix(line, "event: ") {
			eventType = strings.TrimPrefix(line, "event: ")
			continue
		}
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		data := strings.TrimPrefix(line, "data: ")
		if data == "[DONE]" {
			fmt.Println()
			break
		}
		var chunk map[string]any
		if err := json.Unmarshal([]byte(data), &chunk); err != nil {
			continue
		}
		switch eventType {
		case "response.output_text.delta":
			if delta, ok := chunk["delta"].(string); ok {
				fmt.Print(delta)
			}
		case "response.completed":
			fmt.Println()
			if resp, ok := chunk["response"].(map[string]any); ok {
				llmPrintUsage(resp)
			}
		}
	}
	return scanner.Err()
}

// ── messages (Anthropic) ──────────────────────────────────────────────────────

func (s *serveLocalState) llmDoMessages(ctx context.Context, message string, stream bool) error {
	model := s.llmModel
	if model == "" {
		model = llmDefaultMessages
	}
	payload := map[string]any{
		"model":      model,
		"max_tokens": 1024,
		"stream":     stream,
		"messages":   []map[string]any{{"role": "user", "content": message}},
	}
	fmt.Fprintf(os.Stderr, "→ POST %s/v1/messages  key=%s model=%s stream=%v\n",
		s.llmBaseURL, s.llmKey, model, stream)
	resp, elapsed, err := s.llmRequest(ctx, "POST", "/v1/messages", payload)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if stream {
		return llmStreamMessages(resp)
	}

	fmt.Fprintf(os.Stderr, "← HTTP %d  %.0fms\n", resp.StatusCode, float64(elapsed.Milliseconds()))
	result, err := decodeJSONBody(resp.Body, resp.Status)
	if err != nil {
		return err
	}
	if resp.StatusCode != http.StatusOK {
		return llmPrintError(result)
	}
	// Anthropic: content is an array of content blocks.
	if content, ok := result["content"].([]any); ok {
		for _, block := range content {
			if b, ok := block.(map[string]any); ok {
				if b["type"] == "text" {
					fmt.Println(b["text"])
				}
			}
		}
	}
	llmPrintUsage(result)
	return nil
}

func llmStreamMessages(resp *http.Response) error {
	fmt.Fprintf(os.Stderr, "← HTTP %d  (streaming)\n", resp.StatusCode)
	if resp.StatusCode != http.StatusOK {
		var result map[string]any
		_ = json.NewDecoder(resp.Body).Decode(&result)
		return llmPrintError(result)
	}
	scanner := bufio.NewScanner(resp.Body)
	var eventType string
	for scanner.Scan() {
		line := scanner.Text()
		if strings.HasPrefix(line, "event: ") {
			eventType = strings.TrimPrefix(line, "event: ")
			continue
		}
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		data := strings.TrimPrefix(line, "data: ")
		var chunk map[string]any
		if err := json.Unmarshal([]byte(data), &chunk); err != nil {
			continue
		}
		switch eventType {
		case "content_block_delta":
			if delta, ok := chunk["delta"].(map[string]any); ok {
				if delta["type"] == "text_delta" {
					if text, ok := delta["text"].(string); ok {
						fmt.Print(text)
					}
				}
			}
		case "message_stop":
			fmt.Println()
		case "message_delta":
			llmPrintUsage(chunk)
		}
	}
	return scanner.Err()
}

// ── models ────────────────────────────────────────────────────────────────────

func (s *serveLocalState) llmDoModels(ctx context.Context) error {
	fmt.Fprintf(os.Stderr, "→ GET %s/v1/models  key=%s\n", s.llmBaseURL, s.llmKey)
	resp, elapsed, err := s.llmRequest(ctx, "GET", "/v1/models", nil)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	fmt.Fprintf(os.Stderr, "← HTTP %d  %.0fms\n", resp.StatusCode, float64(elapsed.Milliseconds()))
	result, err := decodeJSONBody(resp.Body, resp.Status)
	if err != nil {
		return err
	}
	if resp.StatusCode != http.StatusOK {
		return llmPrintError(result)
	}
	// Print model IDs in a compact list.
	if data, ok := result["data"].([]any); ok {
		ids := make([]string, 0, len(data))
		for _, item := range data {
			if m, ok := item.(map[string]any); ok {
				if id, ok := m["id"].(string); ok {
					ids = append(ids, id)
				}
			}
		}
		sort.Strings(ids)
		for _, id := range ids {
			fmt.Println(" ", id)
		}
		fmt.Fprintf(os.Stderr, "  %d model(s)\n", len(ids))
		return nil
	}
	llmPrintJSON(result)
	return nil
}

// ── set / status ──────────────────────────────────────────────────────────────

func (s *serveLocalState) llmSet(args []string) error {
	if len(args) < 2 {
		return fmt.Errorf("usage: llm set key|model|base|stream <value>")
	}
	switch args[0] {
	case "key":
		s.llmKey = args[1]
		fmt.Printf("key → %s\n", s.llmKey)
	case "model":
		if args[1] == "-" || args[1] == "auto" {
			s.llmModel = ""
			fmt.Println("model → auto")
		} else {
			s.llmModel = args[1]
			fmt.Printf("model → %s\n", s.llmModel)
		}
	case "base":
		s.llmBaseURL = strings.TrimRight(args[1], "/")
		fmt.Printf("base → %s\n", s.llmBaseURL)
	case "stream":
		s.llmStream = args[1] == "on" || args[1] == "true" || args[1] == "1"
		if s.llmStream {
			fmt.Println("stream → on")
		} else {
			fmt.Println("stream → off")
		}
	default:
		return fmt.Errorf("unknown setting %q — try: key, model, base, stream", args[0])
	}
	return nil
}

func (s *serveLocalState) llmStatus() error {
	streamStr := "off"
	if s.llmStream {
		streamStr = "on"
	}
	modelStr := s.llmModel
	if modelStr == "" {
		modelStr = fmt.Sprintf("auto (%s for chat, %s for messages)", llmDefaultChat, llmDefaultMessages)
	}
	fmt.Printf("base:   %s\n", s.llmBaseURL)
	fmt.Printf("key:    %s\n", s.llmKey)
	fmt.Printf("model:  %s\n", modelStr)
	fmt.Printf("stream: %s\n", streamStr)

	raw, err := s.snapshotFn()
	if err == nil && len(raw.Keys) > 0 {
		keys := make([]string, 0, len(raw.Keys))
		for k := range raw.Keys {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		fmt.Printf("config keys: %s\n", strings.Join(keys, ", "))
	}
	return nil
}

// ── core HTTP helper ──────────────────────────────────────────────────────────

func (s *serveLocalState) llmRequest(
	ctx context.Context,
	method, path string,
	payload any,
) (*http.Response, time.Duration, error) {
	var bodyReader *bytes.Reader
	if payload != nil {
		b, err := json.Marshal(payload)
		if err != nil {
			return nil, 0, fmt.Errorf("marshal payload: %w", err)
		}
		bodyReader = bytes.NewReader(b)
	} else {
		bodyReader = bytes.NewReader(nil)
	}

	req, err := http.NewRequestWithContext(ctx, method, s.llmBaseURL+path, bodyReader)
	if err != nil {
		return nil, 0, err
	}
	if payload != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("Authorization", "Bearer "+s.llmKey)
	req.Header.Set("anthropic-version", "2023-06-01")

	start := time.Now()
	resp, err := s.llmClient.Do(req)
	if err != nil {
		return nil, 0, fmt.Errorf("HTTP %s %s: %w", method, path, err)
	}
	return resp, time.Since(start), nil
}

// ── output helpers ────────────────────────────────────────────────────────────

// decodeJSONBody reads the response body and JSON-decodes it into a map.
// When the body is not valid JSON (e.g. Envoy's plain-text error pages on 503),
// it returns a wrapped error that includes the raw body text so the caller can
// display something meaningful instead of a bare JSON parse error.
func decodeJSONBody(body io.ReadCloser, status string) (map[string]any, error) {
	raw, _ := io.ReadAll(body)
	var result map[string]any
	if err := json.Unmarshal(raw, &result); err != nil {
		text := strings.TrimSpace(string(raw))
		if text == "" {
			text = status
		}
		return nil, fmt.Errorf("%s", text)
	}
	return result, nil
}

func llmPrintError(result map[string]any) error {
	if errObj, ok := result["error"].(map[string]any); ok {
		errType, _ := errObj["type"].(string)
		errMsg, _ := errObj["message"].(string)
		if errMsg == "" {
			errMsg, _ = errObj["msg"].(string)
		}
		if errType != "" {
			return fmt.Errorf("%s: %s", errType, errMsg)
		}
		if errMsg != "" {
			return fmt.Errorf("%s", errMsg)
		}
	}
	llmPrintJSON(result)
	return fmt.Errorf("non-200 response (see above)")
}

func llmPrintUsage(result map[string]any) {
	usage, ok := result["usage"].(map[string]any)
	if !ok {
		return
	}
	var parts []string
	for _, k := range []string{
		"input_tokens", "output_tokens", // Anthropic
		"prompt_tokens", "completion_tokens", // OpenAI
		"total_tokens",
	} {
		if v, ok := usage[k]; ok {
			parts = append(parts, fmt.Sprintf("%s=%v", k, v))
		}
	}
	if len(parts) > 0 {
		fmt.Fprintf(os.Stderr, "  usage: %s\n", strings.Join(parts, " "))
	}
}

func llmPrintJSON(v any) {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		fmt.Fprintf(os.Stderr, "(json format error: %v)\n", err)
		return
	}
	fmt.Println(string(b))
}

// ── help ──────────────────────────────────────────────────────────────────────

func printLLMHelp() {
	fmt.Print(`
llm — live LLM test commands (routes through local Envoy on port 8080)

Send:
  llm <message>              POST /v1/chat/completions (OpenAI, gpt-4o-mini)
  llm stream <message>       streaming chat/completions (SSE)
  llm resp <message>         POST /v1/responses (OpenAI Responses API, gpt-4o-mini)
  llm msg <message>          POST /v1/messages (Anthropic, claude-haiku-4-5)
  llm models                 GET  /v1/models (list available models)

Config:
  llm set key   <slug>       switch active API key (e.g. demo/dio/sk-fallback)
  llm set model <name>       pin model; 'auto' or '-' to reset to default
  llm set base  <url>        change proxy base URL (default: http://localhost:8080)
  llm set stream on|off      toggle default streaming for chat/resp/msg
  llm status                 show current settings + config keys

Examples:
  llm hi
  llm count to 5
  llm stream write me a haiku
  llm resp tell me a joke
  llm msg tell me a joke
  llm models
  llm set key demo/dio/sk-fallback
  llm set model claude-haiku-4-5
  llm hi                     (now uses claude-haiku-4-5 via /v1/chat/completions)
  llm set model auto

Tip: request and response metadata (URL, key, HTTP status, timing, token
     usage) go to stderr so they don't clutter copy-pasteable output.

`)
}

// ── init helper ───────────────────────────────────────────────────────────────

// firstConfigKey returns the lexicographically first key slug from the config,
// or "demo/dio/sk-default" if the config has no keys or cannot be read.
func firstConfigKey(fn rls.SnapshotFunc) string {
	raw, err := fn()
	if err != nil || len(raw.Keys) == 0 {
		return "demo/dio/sk-default"
	}
	keys := make([]string, 0, len(raw.Keys))
	for k := range raw.Keys {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys[0]
}
