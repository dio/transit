// Package debug starts an embedded HTTP server that exposes orange-internal
// state for tests and operators. Blank-imported from cmd/main.go so it loads
// alongside the other orange filters; no-op when ORANGE_DEBUG_ADDR is unset.
//
// Endpoints:
//
//	GET /pending/size  — {"size": N}, current pending.registry entry count.
//	GET /healthz       — "ok\n", trivial readiness probe.
//
// This is not load-bearing for production traffic; e2e tests use it to assert
// the post-Phase-3 cleanup contract holds end-to-end (registry returns to
// baseline after every stream Envoy terminates, including aborted ones).
package debug

import (
	"encoding/json"
	"net"
	"net/http"
	"os"

	"github.com/dio/transit/examples/orange/pending"
)

func init() {
	addr := os.Getenv("ORANGE_DEBUG_ADDR")
	if addr == "" {
		return
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/pending/size", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]int{"size": pending.Size()})
	})
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("ok\n"))
	})

	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return
	}
	srv := &http.Server{Handler: mux}
	go func() { _ = srv.Serve(ln) }()
}
