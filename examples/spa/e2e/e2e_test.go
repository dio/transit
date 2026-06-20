// Package e2e runs HTTP-level integration tests for the spa filter against a
// real Envoy instance. No browser is required — DOM rendering is covered by
// the Playwright suite in spa.test.mjs (make e2e-js).
//
// Run:
//
//	make -C examples/spa e2e
package e2e

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/dio/transit/examples/internal/e2etest"
)

//go:embed testdata/envoy.tmpl.yaml
var envoyConfigTmpl string

type ports struct {
	ProxyPort int
	AdminPort int
}

var (
	proxyURL     string
	examplesRoot string
)

func TestMain(m *testing.M) {
	_, file, _, _ := runtime.Caller(0)
	// spa/e2e/e2e_test.go → examples/
	examplesRoot = filepath.Join(filepath.Dir(file), "../../")

	bin := e2etest.EnvoyBin(examplesRoot)
	if _, err := os.Stat(bin); err != nil {
		fmt.Fprintf(os.Stderr, "SKIP: envoy not found at %s (run: make download-envoy)\n", bin)
		os.Exit(0)
	}

	if err := e2etest.CheckSharedLibrary(examplesRoot, "spa", "libspa.so"); err != nil {
		fmt.Fprintf(os.Stderr, "e2e: %v\n", err)
		os.Exit(1)
	}

	proxyPort := e2etest.FreePort()
	adminPort := e2etest.FreePort()
	proxyURL = fmt.Sprintf("http://127.0.0.1:%d", proxyPort)

	cfgPath := e2etest.WriteEnvoyConfig("transit-spa-e2e", envoyConfigTmpl, ports{
		ProxyPort: proxyPort,
		AdminPort: adminPort,
	})

	spaDir := filepath.Join(examplesRoot, "spa")
	stop, ok := e2etest.StartEnvoy(bin, cfgPath, spaDir, adminPort, nil)
	if !ok {
		os.Exit(1)
	}
	fmt.Fprintln(os.Stderr, "e2e: envoy ready")

	code := m.Run()
	stop()
	os.Exit(code)
}

// get performs a GET and returns status, headers, and body.
func get(t *testing.T, path string) (int, http.Header, string) {
	t.Helper()
	resp, err := http.Get(proxyURL + path) //nolint:noctx
	if err != nil {
		t.Fatalf("GET %s: %v", path, err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, resp.Header, string(b)
}

// findAssetPath scans HTML for the first src or href under /assets/ with the given extension.
func findAssetPath(html, ext string) string {
	for _, attr := range []string{`src="/assets/`, `href="/assets/`} {
		i := 0
		for {
			idx := strings.Index(html[i:], attr)
			if idx < 0 {
				break
			}
			pos := i + idx + len(attr)
			end := strings.IndexByte(html[pos:], '"')
			if end < 0 {
				break
			}
			name := html[pos : pos+end]
			if strings.HasSuffix(name, ext) {
				return "/assets/" + name
			}
			i = pos
		}
	}
	return ""
}

func TestRoot_ServesHTML(t *testing.T) {
	status, headers, body := get(t, "/")
	if status != http.StatusOK {
		t.Fatalf("want 200, got %d", status)
	}
	if !strings.Contains(strings.ToLower(body), "<html") {
		t.Fatalf("want HTML body, got: %.200s", body)
	}
	if ct := headers.Get("content-type"); !strings.Contains(ct, "text/html") {
		t.Fatalf("want content-type text/html, got %q", ct)
	}
	if cc := headers.Get("cache-control"); cc != "no-cache" {
		t.Fatalf("want cache-control no-cache, got %q", cc)
	}
}

func TestUnknownPath_FallsBackToHTML(t *testing.T) {
	for _, path := range []string{"/about", "/dashboard", "/deep/nested/route"} {
		status, _, body := get(t, path)
		if status != http.StatusOK {
			t.Fatalf("path=%s: want 200, got %d", path, status)
		}
		if !strings.Contains(strings.ToLower(body), "<html") {
			t.Fatalf("path=%s: want HTML fallback", path)
		}
	}
}

func TestJSAsset_ImmutableCache(t *testing.T) {
	_, _, rootHTML := get(t, "/")
	jsPath := findAssetPath(rootHTML, ".js")
	if jsPath == "" {
		t.Skip("no JS asset found in index.html")
	}
	status, headers, body := get(t, jsPath)
	if status != http.StatusOK {
		t.Fatalf("want 200, got %d", status)
	}
	if !strings.Contains(headers.Get("cache-control"), "immutable") {
		t.Fatalf("want immutable cache-control, got %q", headers.Get("cache-control"))
	}
	if len(body) == 0 {
		t.Fatal("JS asset body is empty")
	}
}

func TestCSSAsset_CorrectContentType(t *testing.T) {
	_, _, rootHTML := get(t, "/")
	cssPath := findAssetPath(rootHTML, ".css")
	if cssPath == "" {
		t.Skip("no CSS asset found in index.html")
	}
	status, headers, _ := get(t, cssPath)
	if status != http.StatusOK {
		t.Fatalf("want 200, got %d", status)
	}
	if ct := headers.Get("content-type"); !strings.Contains(ct, "text/css") {
		t.Fatalf("want content-type text/css, got %q", ct)
	}
}

func TestFavicon_Served(t *testing.T) {
	status, headers, _ := get(t, "/favicon.svg")
	if status != http.StatusOK {
		t.Fatalf("want 200, got %d", status)
	}
	if ct := headers.Get("content-type"); !strings.Contains(ct, "svg") {
		t.Fatalf("want svg content-type, got %q", ct)
	}
}

func TestAPI_Hello(t *testing.T) {
	status, headers, body := get(t, "/api/hello")
	if status != http.StatusOK {
		t.Fatalf("want 200, got %d", status)
	}
	if ct := headers.Get("content-type"); !strings.Contains(ct, "application/json") {
		t.Fatalf("want application/json, got %q", ct)
	}
	var m map[string]any
	if err := json.Unmarshal([]byte(body), &m); err != nil {
		t.Fatalf("parse JSON: %v", err)
	}
	if m["message"] != "hello from inside the .so" {
		t.Fatalf("unexpected message: %v", m["message"])
	}
	if m["filter"] != "api-backend" {
		t.Fatalf("unexpected filter: %v", m["filter"])
	}
}

func TestAPI_Time(t *testing.T) {
	status, _, body := get(t, "/api/time")
	if status != http.StatusOK {
		t.Fatalf("want 200, got %d", status)
	}
	var m map[string]any
	if err := json.Unmarshal([]byte(body), &m); err != nil {
		t.Fatalf("parse JSON: %v", err)
	}
	ts, ok := m["time"].(string)
	if !ok || ts == "" {
		t.Fatalf("want time field, got %v", m)
	}
	if _, err := time.Parse(time.RFC3339, ts); err != nil {
		t.Fatalf("time %q not RFC3339: %v", ts, err)
	}
}

func TestAPI_Unknown_Returns404(t *testing.T) {
	status, _, body := get(t, "/api/unknown-endpoint")
	if status != http.StatusNotFound {
		t.Fatalf("want 404, got %d", status)
	}
	var m map[string]any
	if err := json.Unmarshal([]byte(body), &m); err != nil {
		t.Fatalf("parse JSON: %v", err)
	}
	if m["error"] != "not found" {
		t.Fatalf("want error='not found', got %v", m["error"])
	}
}

func TestAPI_Hello_QueryString(t *testing.T) {
	status, _, _ := get(t, "/api/hello?name=world")
	if status != http.StatusOK {
		t.Fatalf("want 200, got %d", status)
	}
}
