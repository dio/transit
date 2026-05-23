package mcpprofilerouter

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"os"
	"strings"

	"github.com/dio/transit/up"
)

const ProfileEnv = "MCP_PROFILE_ROUTER_PROFILE"

// LoadProfileFromEnv keeps the dynamic-module entrypoint small. The CLI can
// still build profiles from flags, while Envoy demos pass the same shape as
// JSON through the process environment before the .so is loaded.
func LoadProfileFromEnv() (Profile, error) {
	raw := os.Getenv(ProfileEnv)
	if strings.TrimSpace(raw) == "" {
		return Profile{}, fmt.Errorf("%s is required", ProfileEnv)
	}
	var profile Profile
	if err := json.Unmarshal([]byte(raw), &profile); err != nil {
		return Profile{}, fmt.Errorf("parse %s: %w", ProfileEnv, err)
	}
	if err := ValidateProfile(profile); err != nil {
		return Profile{}, fmt.Errorf("%s: %w", ProfileEnv, err)
	}
	return profile, nil
}

type transitRequest struct {
	method  string
	path    string
	headers [][2]string
}

// NewTransitFilter adapts the HTTP-shaped aggregator into Transit callbacks.
// The important part is deployment shape: the MCP profile endpoint is served
// from the dynamic module loaded into Envoy, while backend calls go back through
// Envoy egress routes configured in the profile URLs.
func NewTransitFilter(aggregator *Aggregator) (up.HandlerFunc, up.RequestBodyHandlerFunc) {
	handler := func(_ *up.Writer, r *up.Request) {
		path := stripQuery(r.Path)
		switch {
		case path == "/healthz" || path == "/dump":
			// Body callbacks are invoked synthetically for bodyless requests, so
			// keep the same response path for GET and POST.
		case strings.HasPrefix(path, "/mcp/"):
			// The body callback will replay this request into the aggregator.
		default:
			// Egress requests must pass through untouched so Envoy's router can
			// send them to the backend MCP server clusters.
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
		aggregator.Handler().ServeHTTP(rec, httpReq)
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

// RegisterTransitFilter is the API used by the .so entrypoint. Keeping it in
// the package lets unit tests exercise the HTTP aggregator without importing
// the Envoy ABI implementation.
func RegisterTransitFilter(name string, profile Profile) {
	handler, body := NewTransitFilter(NewAggregator(profile))
	up.RegisterWithMutableBody(name, handler, body, nil)
}

func stripQuery(path string) string {
	if i := strings.IndexByte(path, '?'); i >= 0 {
		return path[:i]
	}
	return path
}
