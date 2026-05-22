package demo

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestClientAddsShardModelAndReadsDump(t *testing.T) {
	store, err := NewConfigStore(DefaultConfig())
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	RegisterControlHandlers(mux, store)

	client := Client{HTTP: &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		return serveInMemory(mux, req), nil
	})}}

	raw, err := client.AddShard(context.Background(), "http://tiered-control.test", ShardUpdate{
		Name:     "c",
		Target:   "l2-c.transit-dataplane.svc.cluster.local:80",
		Prefixes: []string{"c"},
		Shard:    "c",
		Status:   "active",
		Version:  "updated",
	})
	if err != nil {
		t.Fatal(err)
	}
	var l1 L1Config
	if err := json.Unmarshal(raw, &l1); err != nil {
		t.Fatal(err)
	}
	if _, ok := l1.Shards["c"]; !ok {
		t.Fatalf("shard c not added: %#v", l1.Shards)
	}

	raw, err = client.AddModel(context.Background(), "http://tiered-control.test", ModelUpdate{
		Shard:      "b",
		Name:       "qwen-coder",
		Target:     "upstream-d.transit-dataplane.svc.cluster.local:8080",
		Provider:   "qwen",
		AuthHeader: "Bearer qwen-token",
		Profile:    "profile-b",
	})
	if err != nil {
		t.Fatal(err)
	}
	var l2 L2Config
	if err := json.Unmarshal(raw, &l2); err != nil {
		t.Fatal(err)
	}
	if _, ok := l2.Models["qwen-coder"]; !ok {
		t.Fatalf("qwen-coder not added: %#v", l2.Models)
	}

	raw, err = client.Dump(context.Background(), "http://tiered-control.test")
	if err != nil {
		t.Fatal(err)
	}
	if json.Valid(raw) == false {
		t.Fatalf("dump is not JSON: %s", raw)
	}
}

func TestClientRequestSetsGatewayHeaders(t *testing.T) {
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Host != "tiered-router.example.com" {
			t.Fatalf("host = %q", r.Host)
		}
		wantHeaders := map[string]string{
			"x-model":       "gpt-fast",
			"x-transit-tag": "a-demo",
			"x-tenant":      "tenant-a",
			"x-user-key":    "user-a",
			"x-byok-key-id": "key-a-001",
		}
		for name, want := range wantHeaders {
			if got := r.Header.Get(name); got != want {
				t.Fatalf("%s = %q, want %q", name, got, want)
			}
		}
		w.Header().Set("content-type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true}`))
	})

	client := Client{HTTP: &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		return serveInMemory(handler, req), nil
	})}}
	raw, err := client.Request(context.Background(), GatewayRequest{
		GatewayURL: "http://gateway.test",
		Host:       "tiered-router.example.com",
		Model:      "gpt-fast",
		Tag:        "a-demo",
		Tenant:     "tenant-a",
		UserKey:    "user-a",
		BYOKKeyID:  "key-a-001",
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
