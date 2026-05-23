package mcpprofilegateway

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"

	mcpprofilerouter "github.com/dio/transit/examples/mcp-profile-router"
	"github.com/dio/transit/up"
	"github.com/envoyproxy/envoy/source/extensions/dynamic_modules/sdk/go/shared"
)

type transitRequest struct {
	method  string
	path    string
	headers [][2]string
}

// NewTransitFilter exposes the Envoy dynamic-module implementation. Catalog
// forwarding uses HTTPCallout so routing, retries, TLS, DNS, and telemetry stay
// under Envoy rather than bypassing it through Go's net/http client.
func NewTransitFilter(gateway *Gateway) (up.HandlerFunc, up.RequestBodyHandlerFunc) {
	handler := func(_ *up.Writer, r *up.Request) {
		path := stripQuery(r.Path)
		switch {
		case path == "/healthz" || path == "/dump":
		case strings.HasPrefix(path, "/mcp/s/"):
		default:
			return
		}
		if r.Context != nil {
			*r.Context = transitRequest{
				method:  r.Method,
				path:    r.Path,
				headers: r.AllHeaders(),
			}
		}
	}

	body := func(w *up.Writer, chunk *up.BodyChunk) {
		if !chunk.EndStream || chunk.Context == nil {
			return
		}
		req, ok := (*chunk.Context).(transitRequest)
		if !ok || req.path == "" {
			return
		}
		if strings.HasPrefix(stripQuery(req.path), "/mcp/s/") {
			gateway.calloutCatalogServerTransit(w, req, chunk.Data)
			return
		}
		gateway.serveNetHTTPInTransit(w, req, chunk.Data)
	}
	return handler, body
}

// RegisterTransitFilter is the API used by the .so entrypoint. Keeping it in
// the package lets unit tests exercise the net/http implementation without
// importing the Envoy ABI implementation.
func RegisterTransitFilter(name string, config Config) {
	handler, body := NewTransitFilter(New(config))
	up.RegisterWithMutableBody(name, handler, body, nil)
}

func (g *Gateway) calloutCatalogServerTransit(w *up.Writer, r transitRequest, body []byte) {
	serverID, server, ok := g.catalogServerFromTransitPath(r.path)
	if !ok {
		writeTransitJSON(w, http.StatusNotFound, errorResponse(nil, -32004, "unknown catalog server: %s", serverID))
		return
	}
	req, err := g.catalogCalloutRequest(r, body, serverID, server)
	if err != nil {
		g.record(serverID, server, "error: "+err.Error())
		writeTransitJSON(w, http.StatusBadGateway, errorResponse(nil, -32003, "catalog gateway failed: %s", serverID))
		return
	}
	_, err = w.HTTPCallout(req, func(result up.HTTPCalloutResult, headers [][2]shared.UnsafeEnvoyBuffer, respBody []shared.UnsafeEnvoyBuffer) {
		if result != up.HTTPCalloutSuccess {
			g.record(serverID, server, fmt.Sprintf("error: callout result %d", result))
			writeTransitJSON(w, http.StatusBadGateway, errorResponse(nil, -32003, "catalog gateway failed: %s", serverID))
			return
		}
		g.record(serverID, server, "ok")
		status, responseHeaders := responseFromCalloutHeaders(headers)
		w.SendLocalResponse(status, calloutBodyBytes(respBody), responseHeaders...)
	})
	if err != nil {
		g.record(serverID, server, "error: "+err.Error())
		writeTransitJSON(w, http.StatusBadGateway, errorResponse(nil, -32003, "catalog gateway failed: %s", serverID))
	}
}

func (g *Gateway) catalogCalloutRequest(r transitRequest, body []byte, serverID string, server CatalogServer) (up.HTTPCalloutRequest, error) {
	u, err := url.Parse(catalogURL(server.URL, serverID))
	if err != nil {
		return up.HTTPCalloutRequest{}, err
	}
	headers := [][2]string{
		{":method", http.MethodPost},
		{":path", u.RequestURI()},
		{":scheme", u.Scheme},
		{"host", u.Host},
		{"content-type", headerOrDefault(r.headers, "content-type", "application/json")},
		{"accept", headerOrDefault(r.headers, "accept", "application/json")},
	}
	if sessionID := headerValue(r.headers, mcpprofilerouter.SessionIDHeader); sessionID != "" {
		headers = append(headers, [2]string{mcpprofilerouter.SessionIDHeader, sessionID})
	}
	if protocol := headerValue(r.headers, mcpprofilerouter.ProtocolVersionHeader); protocol != "" {
		headers = append(headers, [2]string{mcpprofilerouter.ProtocolVersionHeader, protocol})
	}
	return up.HTTPCalloutRequest{
		Cluster:       catalogCluster(server),
		Headers:       headers,
		Body:          body,
		TimeoutMillis: timeoutMillis(g.config.TimeoutMillis),
	}, nil
}

func (g *Gateway) serveNetHTTPInTransit(w *up.Writer, r transitRequest, body []byte) {
	httpReq := httptest.NewRequest(r.method, r.path, bytes.NewReader(body))
	for _, h := range r.headers {
		if strings.HasPrefix(h[0], ":") {
			continue
		}
		httpReq.Header.Add(h[0], h[1])
	}
	rec := httptest.NewRecorder()
	g.Handler().ServeHTTP(rec, httpReq)
	resp := rec.Result()
	defer func() { _ = resp.Body.Close() }()

	headers := make([][2]string, 0, len(resp.Header))
	for name, values := range resp.Header {
		for _, value := range values {
			headers = append(headers, [2]string{name, value})
		}
	}
	w.SendLocalResponse(resp.StatusCode, rec.Body.Bytes(), headers...)
}

func (g *Gateway) catalogServerFromTransitPath(path string) (string, CatalogServer, bool) {
	rawID, ok := strings.CutPrefix(stripQuery(path), "/mcp/s/")
	if !ok || rawID == "" {
		return "", CatalogServer{}, false
	}
	serverID, err := url.PathUnescape(rawID)
	if err != nil {
		return rawID, CatalogServer{}, false
	}
	server, ok := g.config.CatalogServers[serverID]
	return serverID, server, ok
}

func catalogCluster(server CatalogServer) string {
	if server.Cluster != "" {
		return server.Cluster
	}
	return DefaultCatalogCalloutCluster
}

func timeoutMillis(configured int) uint64 {
	if configured > 0 {
		return uint64(configured)
	}
	return 800
}

func responseFromCalloutHeaders(headers [][2]shared.UnsafeEnvoyBuffer) (int, [][2]string) {
	status := http.StatusOK
	out := make([][2]string, 0, len(headers))
	for _, h := range headers {
		name := h[0].ToString()
		value := h[1].ToString()
		switch {
		case name == ":status":
			if parsed, err := strconv.Atoi(value); err == nil {
				status = parsed
			}
		case strings.HasPrefix(name, ":"):
			continue
		case strings.EqualFold(name, "content-length"):
			continue
		default:
			out = append(out, [2]string{name, value})
		}
	}
	return status, out
}

func calloutBodyBytes(buffers []shared.UnsafeEnvoyBuffer) []byte {
	if len(buffers) == 0 {
		return nil
	}
	var out []byte
	for _, b := range buffers {
		out = append(out, b.ToString()...)
	}
	return out
}

func headerValue(headers [][2]string, name string) string {
	for _, h := range headers {
		if strings.EqualFold(h[0], name) {
			return h[1]
		}
	}
	return ""
}

func headerOrDefault(headers [][2]string, name, fallback string) string {
	if value := headerValue(headers, name); value != "" {
		return value
	}
	return fallback
}

func writeTransitJSON(w *up.Writer, status int, v any) {
	body, err := json.Marshal(v)
	if err != nil {
		w.SendLocalResponse(http.StatusInternalServerError, []byte(`{"jsonrpc":"2.0","error":{"code":-32603,"message":"internal error"}}`), [2]string{"content-type", "application/json"})
		return
	}
	w.SendLocalResponse(status, body, [2]string{"content-type", "application/json"})
}
