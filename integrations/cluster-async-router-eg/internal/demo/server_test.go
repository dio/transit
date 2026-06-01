package demo

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestUpstreamEchoesNameAndBody(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		writeJSON(w, http.StatusOK, UpstreamResponse{Upstream: "upstream-a", Body: string(raw)})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	req, err := http.NewRequestWithContext(context.Background(), http.MethodPost, srv.URL+"/", strings.NewReader(`{"target":"a"}`))
	require.NoError(t, err)
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)

	var got UpstreamResponse
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&got))
	require.Equal(t, "upstream-a", got.Upstream)
	require.Equal(t, `{"target":"a"}`, got.Body)
}

func TestClientRequestSendsTargetBody(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		writeJSON(w, http.StatusOK, map[string]string{"got": string(raw), "host": r.Host})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	raw, err := Client{}.Request(context.Background(), GatewayRequest{
		GatewayURL: srv.URL,
		Host:       "cluster-async-router.example.com",
		Target:     "b",
	})
	require.NoError(t, err)
	// The Body field is the literal JSON we POSTed, wrapped inside another
	// JSON envelope, so quotes are escaped once.
	require.Contains(t, string(raw), `\"target\":\"b\"`)
	require.Contains(t, string(raw), `"host":"cluster-async-router.example.com"`)
}
