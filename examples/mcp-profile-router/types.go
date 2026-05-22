package mcpprofilerouter

import (
	"encoding/json"
	"fmt"
)

const (
	MethodInitialize = "initialize"
	MethodToolsList  = "tools/list"
	MethodToolsCall  = "tools/call"

	SessionIDHeader       = "mcp-session-id"
	ProtocolVersionHeader = "mcp-protocol-version"
	ProtocolVersion       = "2025-06-18"
)

type Profile struct {
	Name          string            `json:"name"`
	APIKey        string            `json:"api_key,omitempty"`
	TimeoutMillis int               `json:"timeout_millis,omitempty"`
	Servers       map[string]Server `json:"servers"`
}

type Server struct {
	URL        string `json:"url"`
	Prefix     string `json:"prefix"`
	Credential string `json:"credential,omitempty"`
}

type JSONRPCRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type JSONRPCResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Result  any             `json:"result,omitempty"`
	Error   *JSONRPCError   `json:"error,omitempty"`
}

type JSONRPCError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

type ListToolsResult struct {
	Tools []Tool `json:"tools"`
}

type InitializeParams struct {
	ProtocolVersion string         `json:"protocolVersion,omitempty"`
	Capabilities    map[string]any `json:"capabilities,omitempty"`
	ClientInfo      Implementation `json:"clientInfo,omitempty"`
}

type InitializeResult struct {
	ProtocolVersion string         `json:"protocolVersion"`
	Capabilities    map[string]any `json:"capabilities"`
	ServerInfo      Implementation `json:"serverInfo"`
}

type Implementation struct {
	Name    string `json:"name"`
	Version string `json:"version,omitempty"`
}

type Tool struct {
	Name        string         `json:"name"`
	Description string         `json:"description,omitempty"`
	InputSchema map[string]any `json:"inputSchema"`
}

type CallToolParams struct {
	Name      string         `json:"name"`
	Arguments map[string]any `json:"arguments,omitempty"`
}

type CallToolResult struct {
	Content           []ContentBlock `json:"content"`
	StructuredContent map[string]any `json:"structuredContent,omitempty"`
	IsError           bool           `json:"isError,omitempty"`
}

type ContentBlock struct {
	Type string `json:"type"`
	Text string `json:"text,omitempty"`
}

func response(id json.RawMessage, result any) JSONRPCResponse {
	return JSONRPCResponse{
		JSONRPC: "2.0",
		ID:      cloneRaw(id),
		Result:  result,
	}
}

func errorResponse(id json.RawMessage, code int, format string, args ...any) JSONRPCResponse {
	return JSONRPCResponse{
		JSONRPC: "2.0",
		ID:      cloneRaw(id),
		Error: &JSONRPCError{
			Code:    code,
			Message: fmt.Sprintf(format, args...),
		},
	}
}

func cloneRaw(raw json.RawMessage) json.RawMessage {
	if len(raw) == 0 {
		return nil
	}
	return append(json.RawMessage(nil), raw...)
}
