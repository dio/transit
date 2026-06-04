package mcp

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
)

type rpcRequest struct {
	JSONRPC string          `json:"jsonrpc,omitempty"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method,omitempty"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type rpcResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func readRPCRequest(r io.Reader) (rpcRequest, []byte, error) {
	if r == nil {
		return rpcRequest{}, nil, fmt.Errorf("missing request body")
	}
	raw, err := io.ReadAll(io.LimitReader(r, maxBackendResponseBody))
	if err != nil {
		return rpcRequest{}, nil, err
	}
	var req rpcRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		return rpcRequest{}, nil, err
	}
	if req.JSONRPC != "" && req.JSONRPC != "2.0" {
		return rpcRequest{}, nil, fmt.Errorf("unsupported jsonrpc version")
	}
	if len(req.ID) == 0 && req.Method == "" {
		return rpcRequest{}, nil, fmt.Errorf("missing method or id")
	}
	return req, raw, nil
}

func rpcErrorResponse(id json.RawMessage, code int, message string) rpcResponse {
	return rpcResponse{JSONRPC: "2.0", ID: id, Error: &rpcError{Code: code, Message: message}}
}

func localError(code, message string) map[string]any {
	return map[string]any{
		"error": map[string]any{
			"type":    "invalid_request_error",
			"code":    code,
			"message": message,
		},
	}
}

func writeJSONBytes(w http.ResponseWriter, status int, body []byte) {
	w.Header().Set("content-type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(body)
}

func mergeListResponse(req rpcRequest, results []backendResult) ([]byte, error) {
	key := listResultKey(req.Method)
	if key == "" {
		return nil, fmt.Errorf("unsupported list method %q", req.Method)
	}
	items := make([]map[string]any, 0)
	for _, result := range results {
		if result.err != nil || result.status < 200 || result.status >= 300 {
			continue
		}
		var resp rpcResponse
		if err := json.Unmarshal(result.body, &resp); err != nil {
			continue
		}
		if resp.Error != nil || len(resp.Result) == 0 {
			continue
		}
		var envelope map[string]json.RawMessage
		if err := json.Unmarshal(resp.Result, &envelope); err != nil {
			continue
		}
		var backendItems []map[string]any
		if err := json.Unmarshal(envelope[key], &backendItems); err != nil {
			continue
		}
		for _, item := range backendItems {
			if name, ok := item["name"].(string); ok && name != "" {
				item["name"] = result.backend + toolSeparator + name
			}
			items = append(items, item)
		}
	}
	result := map[string]any{key: items}
	rawResult, err := json.Marshal(result)
	if err != nil {
		return nil, err
	}
	return json.Marshal(rpcResponse{JSONRPC: "2.0", ID: req.ID, Result: rawResult})
}

func listResultKey(method string) string {
	switch method {
	case methodToolsList:
		return "tools"
	case methodPromptsList:
		return "prompts"
	case methodResourcesList:
		return "resources"
	default:
		return ""
	}
}

func rewriteToolCall(req rpcRequest) (backend, tool string, raw []byte, err error) {
	var params map[string]any
	if err := json.Unmarshal(req.Params, &params); err != nil {
		return "", "", nil, err
	}
	name, _ := params["name"].(string)
	backend, tool, ok := strings.Cut(name, toolSeparator)
	if !ok || backend == "" || tool == "" {
		return "", "", nil, fmt.Errorf("tool name %q is not backend-prefixed", name)
	}
	params["name"] = tool
	rawParams, err := json.Marshal(params)
	if err != nil {
		return "", "", nil, err
	}
	req.Params = rawParams
	raw, err = json.Marshal(req)
	return backend, tool, raw, err
}

func capabilitiesFromInitialize(body []byte) capabilities {
	var resp rpcResponse
	if err := json.Unmarshal(body, &resp); err != nil || len(resp.Result) == 0 {
		return capabilities{}
	}
	var result struct {
		Capabilities struct {
			Tools *struct {
				ListChanged bool `json:"listChanged"`
			} `json:"tools"`
			Prompts *struct {
				ListChanged bool `json:"listChanged"`
			} `json:"prompts"`
			Logging   any `json:"logging"`
			Resources *struct {
				ListChanged bool `json:"listChanged"`
				Subscribe   bool `json:"subscribe"`
			} `json:"resources"`
			Completions any `json:"completions"`
		} `json:"capabilities"`
	}
	if err := json.Unmarshal(resp.Result, &result); err != nil {
		return capabilities{}
	}
	return capabilities{
		Tools:                result.Capabilities.Tools != nil,
		ToolsListChanged:     result.Capabilities.Tools != nil && result.Capabilities.Tools.ListChanged,
		Prompts:              result.Capabilities.Prompts != nil,
		PromptsListChanged:   result.Capabilities.Prompts != nil && result.Capabilities.Prompts.ListChanged,
		Logging:              result.Capabilities.Logging != nil,
		Resources:            result.Capabilities.Resources != nil,
		ResourcesListChanged: result.Capabilities.Resources != nil && result.Capabilities.Resources.ListChanged,
		ResourcesSubscribe:   result.Capabilities.Resources != nil && result.Capabilities.Resources.Subscribe,
		Completions:          result.Capabilities.Completions != nil,
	}
}

func toolCallIsError(body []byte) bool {
	var resp rpcResponse
	if err := json.Unmarshal(body, &resp); err != nil || len(resp.Result) == 0 {
		return false
	}
	var result struct {
		IsError bool `json:"isError"`
	}
	_ = json.Unmarshal(resp.Result, &result)
	return result.IsError
}

func rewriteServerRequestID(msg jsonrpcMessage, backend string) jsonrpcMessage {
	var req rpcRequest
	if err := json.Unmarshal(msg, &req); err != nil || req.Method == "" || len(req.ID) == 0 {
		return msg
	}
	var id any
	if err := json.Unmarshal(req.ID, &id); err != nil {
		return msg
	}
	req.ID, _ = json.Marshal(map[string]any{"backend": backend, "id": id})
	raw, err := json.Marshal(req)
	if err != nil {
		return msg
	}
	return raw
}

func backendFromResponseID(raw json.RawMessage) string {
	var envelope struct {
		Backend string `json:"backend"`
	}
	if err := json.Unmarshal(raw, &envelope); err != nil {
		return ""
	}
	return envelope.Backend
}
