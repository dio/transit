// Package spa demonstrates serving a Vite + React SPA directly from an Envoy
// dynamic module — no file system access, no separate web server.
//
// Two filters ship in the same .so:
//
//	spa         — serves embedded ui/dist assets; falls back to index.html for
//	              unmatched paths (SPA client-side routing support)
//	api-backend — handles /api/* requests directly from Go, no upstream needed
//
// # How assets are embedded
//
// The ui/dist directory is embedded at compile time using //go:embed. The Vite
// build output (index.html + fingerprinted assets/) lives there. For development,
// run the Vite dev server separately (npm run dev in ui/); for production, run
// `npm run build` in ui/ then rebuild the .so.
//
// # Request routing in Envoy
//
//	GET /              → spa filter → index.html (200, text/html)
//	GET /assets/*.js   → spa filter → fingerprinted asset (200, immutable cache)
//	GET /api/*         → api-backend filter → JSON (200, application/json)
//	GET /unknown-page  → spa filter → index.html (SPA client-side routing fallback)
//
// All responses are generated inside the filter — no upstream cluster is contacted.
// The Envoy router is still required at the end of the chain, but the route can
// point at a blackhole cluster since it is never reached.
package spa

import (
	"embed"
	"encoding/json"
	"io/fs"
	"mime"
	"net/http"
	"path/filepath"
	"strings"
	"time"

	"github.com/dio/transit/up"
)

// UIFS holds the compiled Vite output, exported so tests can discover
// fingerprinted asset filenames at runtime.
//
//go:embed ui/dist
var UIFS embed.FS

// indexHTML is the SPA shell — served for every path that is not a known asset.
//
//go:embed ui/dist/index.html
var indexHTML []byte

func init() {
	// Two filters registered from the same init() — each mapped to a distinct
	// filter_name in envoy.yaml. Both compile into the same .so and share the
	// embedded ui/dist filesystem, but their handlers and lifecycles are independent.
	//
	// Chain order in envoy.yaml matters: api-backend runs first so it can
	// short-circuit /api/* requests before they reach the spa filter.
	up.Register("spa", SPAHandler)
	up.Register("api-backend", up.Chain(APIHandler, apiLogMiddleware))
}

// SPAHandler serves embedded static assets and falls back to index.html for
// all other paths, enabling client-side routing (e.g. React Router).
// Exported for unit testing.
func SPAHandler(w *up.Writer, r *up.Request) {
	path := r.Path
	if i := strings.IndexByte(path, '?'); i >= 0 {
		path = path[:i]
	}

	data, err := fs.ReadFile(UIFS, "ui/dist"+path)
	if err == nil {
		ct := mime.TypeByExtension(filepath.Ext(path))
		if ct == "" {
			ct = "application/octet-stream"
		}
		w.SendLocalResponse(http.StatusOK, data,
			[2]string{"content-type", ct},
			[2]string{"cache-control", cacheControl(path)},
		)
		return
	}

	w.SendLocalResponse(http.StatusOK, indexHTML,
		[2]string{"content-type", "text/html; charset=utf-8"},
		[2]string{"cache-control", "no-cache"},
	)
}

// cacheControl returns an appropriate Cache-Control value.
// Vite fingerprints /assets/* files — safe to cache indefinitely.
func cacheControl(path string) string {
	if strings.HasPrefix(path, "/assets/") {
		return "public, max-age=31536000, immutable"
	}
	return "no-cache"
}

// APIHandler handles /api/* requests and responds directly from the .so.
// No upstream cluster is contacted — this IS the backend the SPA calls.
// Exported for unit testing.
func APIHandler(w *up.Writer, r *up.Request) {
	path := r.Path

	switch {
	case path == "/api/hello" || strings.HasPrefix(path, "/api/hello?"):
		serveHello(w, r)

	case path == "/api/time" || strings.HasPrefix(path, "/api/time?"):
		serveTime(w)

	case strings.HasPrefix(path, "/api/"):
		jsonResponse(w, http.StatusNotFound, map[string]string{
			"error": "not found",
			"path":  path,
		})
	}
	// Non-/api/ path: return without responding; the next filter (spa) handles it.
}

func serveHello(w *up.Writer, r *up.Request) {
	jsonResponse(w, http.StatusOK, map[string]any{
		"message": "hello from inside the .so",
		"filter":  r.FilterName,
		"path":    r.Path,
	})
}

func serveTime(w *up.Writer) {
	jsonResponse(w, http.StatusOK, map[string]any{
		"time": time.Now().UTC().Format(time.RFC3339),
	})
}

func jsonResponse(w *up.Writer, status int, v any) {
	b, _ := json.Marshal(v)
	w.SendLocalResponse(status, b, [2]string{"content-type", "application/json"})
}

// apiLogMiddleware logs each /api request at INFO level.
func apiLogMiddleware(next up.HandlerFunc) up.HandlerFunc {
	return func(w *up.Writer, r *up.Request) {
		w.Log(up.LogInfo, "[api-backend] %s %s", r.Method, r.Path)
		next(w, r)
	}
}
