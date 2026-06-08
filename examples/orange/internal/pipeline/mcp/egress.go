package mcp

import (
	"context"
	"net/http"
	"net/url"
	"strings"

	"github.com/dio/transit/examples/orange/internal/pipeline/match"
	"github.com/dio/transit/examples/orange/internal/send"
	"github.com/dio/transit/up"
)

const (
	metadataNamespace = "orange_mcp"
	metadataRoute     = "route"
	metadataBackend   = "backend"
	metadataMethod    = "method"
	metadataTool      = "tool"
	metadataRequestID = "request_id"
)

var internalHeaders = [...]string{
	headerRoute,
	headerBackend,
	headerMethod,
	headerRequestID,
	headerTool,
	headerSession,
	headerLastEventID,
}

func egressHandler(w *up.Writer, r *up.Request) {
	routeName := r.Header(headerRoute)
	backendName := r.Header(headerBackend)
	method := r.Header(headerMethod)
	requestID := r.Header(headerRequestID)
	tool := r.Header(headerTool)

	for _, h := range internalHeaders {
		w.RemoveRequestHeader(h)
	}

	if routeName == "" || backendName == "" || method == "" || requestID == "" {
		send.Error(w, http.StatusBadRequest, send.InvalidRequestError,
			"orange.mcp_headers_missing",
			"orange-mcp internal headers are missing; request did not originate from orange-mcp sidecar")
		return
	}

	// Use new AppState when available; fall back to legacy singleton.
	if mcpAppState != nil {
		cfgSnap := mcpAppState.Snapshot()
		if cfgSnap == nil || cfgSnap.Global == nil {
			send.Errorf(w, http.StatusInternalServerError, send.InternalServerError,
				"orange.mcp_config_not_loaded",
				"orange config is not loaded")
			return
		}
		server, ok := cfgSnap.Global.Servers[backendName]
		if !ok {
			send.Errorf(w, http.StatusBadRequest, send.InvalidRequestError,
				"orange.mcp_backend_not_found",
				"MCP backend %q not found in active orange config", backendName)
			return
		}
		host, err := endpointHost(server.Endpoint)
		if err != nil {
			send.Errorf(w, http.StatusBadGateway, send.InvalidRequestError,
				"orange.mcp_endpoint_invalid",
				"MCP backend %q endpoint is invalid: %v", backendName, err)
			return
		}
		w.SetFilterState(match.StateUpstream, backendName)
		w.SetRequestHeader(up.HeaderAuthority, host)
		w.SetRequestHeader(":path", server.Path())
		if server.Auth != nil && server.Auth.SecretRef != "" && mcpSecResolver != nil {
			if credential, err := mcpSecResolver.Resolve(context.Background(), server.Auth.SecretRef); err == nil && credential != "" {
				w.SetRequestHeader("authorization", bearerCredential(credential))
			}
		}
		w.SetMetadata(metadataNamespace, metadataRoute, routeName)
		w.SetMetadata(metadataNamespace, metadataBackend, backendName)
		w.SetMetadata(metadataNamespace, metadataMethod, method)
		w.SetMetadata(metadataNamespace, metadataRequestID, requestID)
		if tool != "" {
			w.SetMetadata(metadataNamespace, metadataTool, tool)
		}
		return
	}

	// mcpAppState must be set before the filter handles any requests.
	send.Errorf(w, http.StatusInternalServerError, send.InternalServerError,
		"orange.mcp_config_not_configured",
		"orange MCP config is not configured; call mcp.SetAppState before serving")
}

func endpointHost(raw string) (string, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return "", err
	}
	if u.Host == "" {
		return "", url.InvalidHostError(raw)
	}
	return u.Host, nil
}

func bearerCredential(v string) string {
	if strings.Contains(v, " ") {
		return v
	}
	return "Bearer " + v
}
