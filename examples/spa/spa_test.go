package spa_test

import (
	"encoding/json"
	"io/fs"
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	spa "github.com/dio/transit/examples/spa"
	"github.com/dio/transit/up"
	"github.com/dio/transit/up/testutil"
)

// runHandler drives handler with a GET request to path and returns the fake handle.
func runHandler(t *testing.T, handler up.HandlerFunc, path, filterName string) *testutil.FakeFilterHandle {
	t.Helper()
	fh := testutil.NewFilterHandle(testutil.WithHeaders(map[string]string{
		":method": "GET",
		":path":   path,
	}))
	w := up.NewWriter(fh)
	r := up.NewRequest(fh.RequestHeaders(), filterName)
	handler(w, r)
	return fh
}

// localResponseHeader returns the first value of key from a LocalResponse's Headers.
func localResponseHeader(lr testutil.LocalResponse, key string) string {
	for _, h := range lr.Headers {
		if strings.EqualFold(h[0], key) {
			return h[1]
		}
	}
	return ""
}

// assetPath finds the first embedded asset with the given extension.
// Vite fingerprints filenames, so tests must discover them at runtime.
func assetPath(t *testing.T, ext string) string {
	t.Helper()
	var found string
	fs.WalkDir(spa.UIFS, "ui/dist/assets", func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		if strings.HasSuffix(path, ext) && found == "" {
			found = strings.TrimPrefix(path, "ui/dist")
		}
		return nil
	})
	require.NotEmpty(t, found, "no %s asset found in ui/dist/assets", ext)
	return found
}

// -- spa filter tests --

func TestSPA_Root_ServesIndexHTML(t *testing.T) {
	fh := runHandler(t, spa.SPAHandler, "/", "spa")
	require.Len(t, fh.LocalResponses, 1)
	lr := fh.LocalResponses[0]
	assert.Equal(t, uint32(http.StatusOK), lr.Status)
	assert.Contains(t, string(lr.Body), "<html", "root should serve HTML")
	assert.Contains(t, localResponseHeader(lr, "content-type"), "text/html")
	assert.Equal(t, "no-cache", localResponseHeader(lr, "cache-control"))
}

func TestSPA_UnknownPath_FallsBackToIndexHTML(t *testing.T) {
	for _, path := range []string{"/about", "/dashboard", "/some/deep/path"} {
		fh := runHandler(t, spa.SPAHandler, path, "spa")
		require.Len(t, fh.LocalResponses, 1, "path=%s", path)
		assert.Equal(t, uint32(http.StatusOK), fh.LocalResponses[0].Status, "path=%s", path)
		assert.Contains(t, string(fh.LocalResponses[0].Body), "<html", "path=%s", path)
	}
}

func TestSPA_JSAsset_ServedWithImmutableCache(t *testing.T) {
	jsPath := assetPath(t, ".js")
	fh := runHandler(t, spa.SPAHandler, jsPath, "spa")
	require.Len(t, fh.LocalResponses, 1)
	lr := fh.LocalResponses[0]
	assert.Equal(t, uint32(http.StatusOK), lr.Status)
	assert.Contains(t, localResponseHeader(lr, "cache-control"), "immutable")
	assert.NotEmpty(t, lr.Body)
}

func TestSPA_CSSAsset_CorrectContentType(t *testing.T) {
	cssPath := assetPath(t, ".css")
	fh := runHandler(t, spa.SPAHandler, cssPath, "spa")
	require.Len(t, fh.LocalResponses, 1)
	assert.Contains(t, localResponseHeader(fh.LocalResponses[0], "content-type"), "text/css")
}

func TestSPA_Favicon_Served(t *testing.T) {
	fh := runHandler(t, spa.SPAHandler, "/favicon.svg", "spa")
	require.Len(t, fh.LocalResponses, 1)
	assert.Equal(t, uint32(http.StatusOK), fh.LocalResponses[0].Status)
}

func TestSPA_QueryString_Stripped(t *testing.T) {
	fh := runHandler(t, spa.SPAHandler, "/?foo=bar", "spa")
	require.Len(t, fh.LocalResponses, 1)
	assert.Contains(t, string(fh.LocalResponses[0].Body), "<html")
}

// -- api-backend filter tests --

func TestAPI_Hello(t *testing.T) {
	fh := runHandler(t, spa.APIHandler, "/api/hello", "api-backend")
	require.Len(t, fh.LocalResponses, 1)
	lr := fh.LocalResponses[0]
	assert.Equal(t, uint32(http.StatusOK), lr.Status)
	assert.Contains(t, localResponseHeader(lr, "content-type"), "application/json")
	var body map[string]any
	require.NoError(t, json.Unmarshal(lr.Body, &body))
	assert.Equal(t, "hello from inside the .so", body["message"])
	assert.Equal(t, "api-backend", body["filter"])
}

func TestAPI_Time(t *testing.T) {
	fh := runHandler(t, spa.APIHandler, "/api/time", "api-backend")
	require.Len(t, fh.LocalResponses, 1)
	var body map[string]any
	require.NoError(t, json.Unmarshal(fh.LocalResponses[0].Body, &body))
	assert.NotEmpty(t, body["time"])
}

func TestAPI_Unknown_Returns404(t *testing.T) {
	fh := runHandler(t, spa.APIHandler, "/api/unknown-endpoint", "api-backend")
	require.Len(t, fh.LocalResponses, 1)
	assert.Equal(t, uint32(http.StatusNotFound), fh.LocalResponses[0].Status)
}

func TestAPI_NonAPIPath_PassThrough(t *testing.T) {
	fh := runHandler(t, spa.APIHandler, "/about", "api-backend")
	assert.Empty(t, fh.LocalResponses, "/about should not be handled by api-backend")
}

func TestAPI_HelloWithQueryString(t *testing.T) {
	fh := runHandler(t, spa.APIHandler, "/api/hello?name=world", "api-backend")
	require.Len(t, fh.LocalResponses, 1)
	assert.Equal(t, uint32(http.StatusOK), fh.LocalResponses[0].Status)
}
