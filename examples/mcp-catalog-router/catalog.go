package mcpcatalogrouter

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	mcpprofilerouter "github.com/dio/transit/examples/mcp-profile-router"
	"github.com/dio/transit/up"
)

const ConfigEnv = "MCP_CATALOG_ROUTER_CONFIG"

type Config struct {
	RouteHeader   string            `json:"route_header,omitempty"`
	TimeoutMillis int               `json:"timeout_millis,omitempty"`
	Servers       map[string]Server `json:"servers"`
}

type Server struct {
	URL        string `json:"url"`
	Credential string `json:"credential,omitempty"`
}

type CatalogRouter struct {
	config Config
	client *http.Client

	mu      sync.Mutex
	servers map[string]ServerDump
}

type ServerDump struct {
	Target               string `json:"target"`
	CredentialConfigured bool   `json:"credential_configured"`
	LastRequest          string `json:"last_request,omitempty"`
}

func LoadConfigFromEnv() (Config, error) {
	raw := os.Getenv(ConfigEnv)
	if strings.TrimSpace(raw) == "" {
		return Config{}, fmt.Errorf("%s is required", ConfigEnv)
	}
	var config Config
	if err := json.Unmarshal([]byte(raw), &config); err != nil {
		return Config{}, fmt.Errorf("parse %s: %w", ConfigEnv, err)
	}
	if err := ValidateConfig(config); err != nil {
		return Config{}, fmt.Errorf("%s: %w", ConfigEnv, err)
	}
	return config, nil
}

func ValidateConfig(config Config) error {
	if len(config.Servers) == 0 {
		return errors.New("at least one server is required")
	}
	seen := make(map[string]struct{}, len(config.Servers))
	for id, server := range config.Servers {
		if strings.TrimSpace(id) == "" {
			return errors.New("server id is required")
		}
		if strings.Contains(id, "/") {
			return fmt.Errorf("server %q must not contain /", id)
		}
		if _, ok := seen[id]; ok {
			return fmt.Errorf("duplicate server %q", id)
		}
		seen[id] = struct{}{}
		if strings.TrimSpace(server.URL) == "" {
			return fmt.Errorf("server %q url is required", id)
		}
		u, err := url.Parse(server.URL)
		if err != nil || u.Scheme == "" || u.Host == "" {
			return fmt.Errorf("server %q url must be absolute", id)
		}
	}
	return nil
}

func New(config Config) *CatalogRouter {
	timeout := time.Duration(config.TimeoutMillis) * time.Millisecond
	if timeout <= 0 {
		timeout = 800 * time.Millisecond
	}
	return &CatalogRouter{
		config:  config,
		client:  &http.Client{Timeout: timeout},
		servers: make(map[string]ServerDump, len(config.Servers)),
	}
}

func (c *CatalogRouter) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("GET /dump", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, c.Dump())
	})
	mux.HandleFunc("POST /mcp/s/{server}", c.handleServer)
	return mux
}

func (c *CatalogRouter) Dump() map[string]ServerDump {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make(map[string]ServerDump, len(c.servers))
	for k, v := range c.servers {
		out[k] = v
	}
	return out
}

func (c *CatalogRouter) handleServer(w http.ResponseWriter, r *http.Request) {
	serverID := r.PathValue("server")
	server, ok := c.config.Servers[serverID]
	if !ok {
		writeJSON(w, http.StatusNotFound, errorResponse(nil, -32004, "unknown catalog server: %s", serverID))
		return
	}
	resp, err := c.forward(r, serverID, server)
	if err != nil {
		c.record(serverID, server, "error: "+err.Error())
		writeJSON(w, http.StatusBadGateway, errorResponse(nil, -32003, "catalog backend failed: %s", serverID))
		return
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		c.record(serverID, server, "error: "+err.Error())
		writeJSON(w, http.StatusBadGateway, errorResponse(nil, -32003, "catalog backend failed: %s", serverID))
		return
	}
	c.record(serverID, server, "ok")
	copyResponseHeaders(w.Header(), resp.Header)
	w.WriteHeader(resp.StatusCode)
	_, _ = w.Write(body)
}

func (c *CatalogRouter) forward(r *http.Request, serverID string, server Server) (*http.Response, error) {
	defer func() { _ = r.Body.Close() }()
	body, err := io.ReadAll(r.Body)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(r.Context(), http.MethodPost, strings.TrimRight(server.URL, "/")+"/mcp", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("content-type", r.Header.Get("content-type"))
	if req.Header.Get("content-type") == "" {
		req.Header.Set("content-type", "application/json")
	}
	req.Header.Set("accept", r.Header.Get("accept"))
	if req.Header.Get("accept") == "" {
		req.Header.Set("accept", "application/json")
	}
	req.Header.Set(c.routeHeader(), serverID)
	if server.Credential != "" {
		req.Header.Set("authorization", server.Credential)
	}
	if sessionID := r.Header.Get(mcpprofilerouter.SessionIDHeader); sessionID != "" {
		req.Header.Set(mcpprofilerouter.SessionIDHeader, sessionID)
	}
	if protocol := r.Header.Get(mcpprofilerouter.ProtocolVersionHeader); protocol != "" {
		req.Header.Set(mcpprofilerouter.ProtocolVersionHeader, protocol)
	}
	return c.client.Do(req)
}

func (c *CatalogRouter) routeHeader() string {
	if c.config.RouteHeader != "" {
		return c.config.RouteHeader
	}
	return "x-mcp-server"
}

func (c *CatalogRouter) record(id string, server Server, state string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.servers[id] = ServerDump{
		Target:               server.URL,
		CredentialConfigured: server.Credential != "",
		LastRequest:          state,
	}
}

type transitRequest struct {
	method  string
	path    string
	headers [][2]string
}

func NewTransitFilter(router *CatalogRouter) (up.HandlerFunc, up.RequestBodyHandlerFunc) {
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
		httpReq := httptest.NewRequest(req.method, req.path, bytes.NewReader(chunk.Data))
		for _, h := range req.headers {
			if strings.HasPrefix(h[0], ":") {
				continue
			}
			httpReq.Header.Add(h[0], h[1])
		}
		rec := httptest.NewRecorder()
		router.Handler().ServeHTTP(rec, httpReq)
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
	return handler, body
}

func RegisterTransitFilter(name string, config Config) {
	handler, body := NewTransitFilter(New(config))
	up.RegisterWithMutableBody(name, handler, body, nil)
}

func SortedServerIDs(servers map[string]Server) []string {
	out := make([]string, 0, len(servers))
	for id := range servers {
		out = append(out, id)
	}
	sort.Strings(out)
	return out
}

func stripQuery(path string) string {
	if i := strings.IndexByte(path, '?'); i >= 0 {
		return path[:i]
	}
	return path
}

func copyResponseHeaders(dst, src http.Header) {
	for name, values := range src {
		if strings.EqualFold(name, "content-length") {
			continue
		}
		for _, value := range values {
			dst.Add(name, value)
		}
	}
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("content-type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func errorResponse(id json.RawMessage, code int, format string, args ...any) mcpprofilerouter.JSONRPCResponse {
	return mcpprofilerouter.JSONRPCResponse{
		JSONRPC: "2.0",
		ID:      id,
		Error: &mcpprofilerouter.JSONRPCError{
			Code:    code,
			Message: fmt.Sprintf(format, args...),
		},
	}
}
