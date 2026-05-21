// testserver is a minimal HTTP backend for request-ui e2e tests.
// It listens on $TESTSERVER_ADDR (default 127.0.0.1:11000) and responds
// with simple JSON for all routes.
package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
)

func main() {
	addr := os.Getenv("TESTSERVER_ADDR")
	if addr == "" {
		addr = "127.0.0.1:11000"
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]bool{"ok": true}) //nolint:errcheck
	})
	mux.HandleFunc("/api/hello", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]string{"message": "hello"}) //nolint:errcheck
	})
	mux.HandleFunc("/api/create", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		json.NewEncoder(w).Encode(map[string]int{"id": 1}) //nolint:errcheck
	})
	mux.HandleFunc("/api/error", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		json.NewEncoder(w).Encode(map[string]string{"error": "internal"}) //nolint:errcheck
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]string{"path": r.URL.Path}) //nolint:errcheck
	})

	fmt.Fprintf(os.Stderr, "[testserver] listening on %s\n", addr)
	if err := http.ListenAndServe(addr, mux); err != nil {
		fmt.Fprintf(os.Stderr, "[testserver] error: %v\n", err)
		os.Exit(1)
	}
}
