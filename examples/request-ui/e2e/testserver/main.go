// testserver is a minimal HTTP backend for request-ui e2e tests.
// It listens on $TESTSERVER_ADDR (default 127.0.0.1:11000) and responds
// with simple JSON for all routes.
package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strings"
)

func main() {
	addr := os.Getenv("TESTSERVER_ADDR")
	if addr == "" {
		addr = "127.0.0.1:11000"
	}
	// Route by path suffix so tests can send /{runID}/api/xxx without the server
	// needing to know the runID prefix.
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		p := r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.HasSuffix(p, "/health"):
			json.NewEncoder(w).Encode(map[string]bool{"ok": true}) //nolint:errcheck
		case strings.HasSuffix(p, "/api/hello"):
			json.NewEncoder(w).Encode(map[string]string{"message": "hello"}) //nolint:errcheck
		case strings.HasSuffix(p, "/api/create") && r.Method == "POST":
			w.WriteHeader(http.StatusCreated)
			json.NewEncoder(w).Encode(map[string]int{"id": 1}) //nolint:errcheck
		case strings.HasSuffix(p, "/api/error"):
			w.WriteHeader(http.StatusInternalServerError)
			json.NewEncoder(w).Encode(map[string]string{"error": "internal"}) //nolint:errcheck
		default:
			json.NewEncoder(w).Encode(map[string]string{"path": p}) //nolint:errcheck
		}
	})

	fmt.Fprintf(os.Stderr, "[testserver] listening on %s\n", addr)
	if err := http.ListenAndServe(addr, mux); err != nil {
		fmt.Fprintf(os.Stderr, "[testserver] error: %v\n", err)
		os.Exit(1)
	}
}
