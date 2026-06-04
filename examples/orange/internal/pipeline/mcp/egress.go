package mcp

import (
	"net/http"
	"net/url"
	"strings"

	"github.com/dio/transit/examples/orange/internal/config"
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

	cfg := config.Get()
	route, ok := lookupMCPRoute(cfg, routeName)
	if !ok {
		send.Errorf(w, http.StatusBadRequest, send.InvalidRequestError,
			"orange.mcp_route_not_found",
			"MCP route %q not found in active orange config", routeName)
		return
	}
	backend, ok := route.Backends[backendName]
	if !ok {
		send.Errorf(w, http.StatusBadRequest, send.InvalidRequestError,
			"orange.mcp_backend_not_found",
			"MCP backend %q not found in route %q", backendName, routeName)
		return
	}

	host, err := endpointHost(backend.Endpoint)
	if err != nil {
		send.Errorf(w, http.StatusBadGateway, send.InvalidRequestError,
			"orange.mcp_endpoint_invalid",
			"MCP backend %q endpoint is invalid: %v", backendName, err)
		return
	}

	w.SetFilterState(match.StateUpstream, backendName)
	w.SetRequestHeader(up.HeaderAuthority, host)
	w.SetRequestHeader(":path", backend.Path())
	if credential := cfg.MCPCredential(routeName, backendName); credential != "" {
		w.SetRequestHeader("authorization", bearerCredential(credential))
	}
	w.SetMetadata(metadataNamespace, metadataRoute, routeName)
	w.SetMetadata(metadataNamespace, metadataBackend, backendName)
	w.SetMetadata(metadataNamespace, metadataMethod, method)
	w.SetMetadata(metadataNamespace, metadataRequestID, requestID)
	if tool != "" {
		w.SetMetadata(metadataNamespace, metadataTool, tool)
	}
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
