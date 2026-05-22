package demo

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestControlPlaneServesL1L2AndRedactedDump(t *testing.T) {
	store, err := NewConfigStore(DefaultConfig())
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	RegisterControlHandlers(mux, store)

	shardUpdate := ShardUpdate{
		Name:     "c",
		Target:   "l2-c.transit-dataplane.svc.cluster.local:80",
		Prefixes: []string{"c"},
		Shard:    "c",
		Status:   "active",
		Version:  "updated",
	}
	post := httptest.NewRequest(http.MethodPost, "/l1/shards", bytes.NewReader(mustJSON(shardUpdate)))
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, post)
	if rec.Code != http.StatusOK {
		t.Fatalf("POST /l1/shards status = %d body = %s", rec.Code, rec.Body.String())
	}

	modelUpdate := ModelUpdate{
		Name:       "qwen-coder",
		Target:     "upstream-d.transit-dataplane.svc.cluster.local:8080",
		Provider:   "qwen",
		AuthHeader: "Bearer qwen-token",
		Profile:    "profile-b",
		BYOKKeyID:  "key-b-003",
	}
	post = httptest.NewRequest(http.MethodPost, "/l2/b/models", bytes.NewReader(mustJSON(modelUpdate)))
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, post)
	if rec.Code != http.StatusOK {
		t.Fatalf("POST /l2/b/models status = %d body = %s", rec.Code, rec.Body.String())
	}

	get := httptest.NewRequest(http.MethodGet, "/l1/shards.json", nil)
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, get)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /l1/shards.json status = %d", rec.Code)
	}
	var l1 L1Config
	if err := json.Unmarshal(rec.Body.Bytes(), &l1); err != nil {
		t.Fatal(err)
	}
	if _, ok := l1.Shards["c"]; !ok {
		t.Fatalf("shard c not added: %#v", l1.Shards)
	}

	dump := httptest.NewRequest(http.MethodGet, "/dump", nil)
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, dump)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /dump status = %d", rec.Code)
	}
	body := rec.Body.String()
	if strings.Contains(body, "Bearer qwen-token") {
		t.Fatalf("dump leaked bearer token: %s", body)
	}
	for _, want := range []string{"qwen-coder", "key-b-003", `"auth_configured":true`} {
		if !strings.Contains(body, want) {
			t.Fatalf("dump does not contain %q: %s", want, body)
		}
	}
}

func TestControlPlaneRejectsInvalidUpdates(t *testing.T) {
	store, err := NewConfigStore(DefaultConfig())
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	RegisterControlHandlers(mux, store)

	req := httptest.NewRequest(http.MethodPost, "/l1/shards", bytes.NewReader(mustJSON(ShardUpdate{Name: "bad"})))
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("shard update status = %d, want %d", rec.Code, http.StatusBadRequest)
	}

	req = httptest.NewRequest(http.MethodPost, "/l2/a/models", bytes.NewReader(mustJSON(ModelUpdate{Name: "bad", Provider: "openai"})))
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("model update status = %d, want %d", rec.Code, http.StatusBadRequest)
	}
}
