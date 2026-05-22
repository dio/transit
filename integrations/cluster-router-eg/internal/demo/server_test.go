package demo

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestControlPlaneRoutesAndRedactedDump(t *testing.T) {
	store, err := NewConfigStore(DefaultRouteConfig())
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	RegisterControlHandlers(mux, store)

	update := ModelUpdate{
		Name:       "kimi-fast",
		Target:     "upstream-c.default.svc.cluster.local:8080",
		Provider:   "moonshot",
		AuthHeader: "Bearer moonshot-token",
		Version:    "updated",
	}
	post := httptest.NewRequest(http.MethodPost, "/models", bytes.NewReader(mustJSON(update)))
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, post)
	if rec.Code != http.StatusOK {
		t.Fatalf("POST /models status = %d body = %s", rec.Code, rec.Body.String())
	}

	get := httptest.NewRequest(http.MethodGet, "/routes.json", nil)
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, get)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /routes.json status = %d", rec.Code)
	}
	var cfg RouteConfig
	if err := json.Unmarshal(rec.Body.Bytes(), &cfg); err != nil {
		t.Fatal(err)
	}
	if cfg.Version != "updated" {
		t.Fatalf("version = %q, want updated", cfg.Version)
	}
	if got := cfg.Models["kimi-fast"].AuthHeader; got != "Bearer moonshot-token" {
		t.Fatalf("auth header = %q", got)
	}

	dump := httptest.NewRequest(http.MethodGet, "/dump", nil)
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, dump)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /dump status = %d", rec.Code)
	}
	body := rec.Body.String()
	if strings.Contains(body, "Bearer moonshot-token") {
		t.Fatalf("dump leaked bearer token: %s", body)
	}
	if !strings.Contains(body, `"auth_configured":true`) {
		t.Fatalf("dump did not report configured auth: %s", body)
	}
}

func TestControlPlaneRejectsInvalidModel(t *testing.T) {
	store, err := NewConfigStore(DefaultRouteConfig())
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	RegisterControlHandlers(mux, store)

	update := ModelUpdate{Name: "bad", Provider: "openai"}
	req := httptest.NewRequest(http.MethodPost, "/models", bytes.NewReader(mustJSON(update)))
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusBadRequest)
	}
}
