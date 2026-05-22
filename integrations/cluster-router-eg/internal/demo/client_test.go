package demo

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestClientAddsModelAndReadsDump(t *testing.T) {
	store, err := NewConfigStore(DefaultRouteConfig())
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	RegisterControlHandlers(mux, store)

	client := Client{HTTP: &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		return serveInMemory(mux, req), nil
	})}}
	raw, err := client.AddModel(context.Background(), "http://cluster-router-control.test", ModelUpdate{
		Name:       "gpt-slow",
		Target:     "upstream-a.default.svc.cluster.local:8080",
		Provider:   "openai",
		AuthHeader: "Bearer slow-token",
		Version:    "updated",
	})
	if err != nil {
		t.Fatal(err)
	}
	var cfg RouteConfig
	if err := json.Unmarshal(raw, &cfg); err != nil {
		t.Fatal(err)
	}
	if cfg.Version != "updated" {
		t.Fatalf("version = %q, want updated", cfg.Version)
	}
	if _, ok := cfg.Models["gpt-slow"]; !ok {
		t.Fatalf("gpt-slow not added: %#v", cfg.Models)
	}

	raw, err = client.Dump(context.Background(), "http://cluster-router-control.test")
	if err != nil {
		t.Fatal(err)
	}
	if string(raw) == "" {
		t.Fatal("empty dump")
	}
	if json.Valid(raw) == false {
		t.Fatalf("dump is not JSON: %s", raw)
	}
}

func TestClientRequestSetsGatewayHeaders(t *testing.T) {
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Host != "cluster-router.example.com" {
			t.Fatalf("host = %q", r.Host)
		}
		if got := r.Header.Get("x-model"); got != "kimi-fast" {
			t.Fatalf("x-model = %q", got)
		}
		w.Header().Set("content-type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true}`))
	})

	client := Client{HTTP: &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		return serveInMemory(handler, req), nil
	})}}
	raw, err := client.Request(context.Background(), GatewayRequest{
		GatewayURL: "http://gateway.test",
		Host:       "cluster-router.example.com",
		Model:      "kimi-fast",
	})
	if err != nil {
		t.Fatal(err)
	}
	if string(raw) != `{"ok":true}` {
		t.Fatalf("response = %s", raw)
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

func serveInMemory(handler http.Handler, req *http.Request) *http.Response {
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	return rec.Result()
}
