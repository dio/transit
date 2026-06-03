// Package debug starts an embedded HTTP server that exposes orange-internal
// state for tests and operators. Blank-imported from cmd/main.go so it loads
// alongside the other orange filters; no-op when ORANGE_DEBUG_ADDR is unset.
//
// Endpoints:
//
//	GET /debug/pprof/  — standard Go pprof suite.
//	GET /pending/size  — {"size": N}, current pending.registry entry count.
//	GET /healthz       — "ok\n", trivial readiness probe.
//
// This is not load-bearing for production traffic; e2e tests use it to assert
// the post-Phase-3 cleanup contract holds end-to-end (registry returns to
// baseline after every stream Envoy terminates, including aborted ones).
package debug

import (
	"encoding/json"
	"net/http"
	"os"

	"github.com/dio/transit/down"
	"github.com/dio/transit/up"
)

func init() {
	addr := os.Getenv("ORANGE_DEBUG_ADDR")
	if addr == "" {
		return
	}
	admin := up.NewAdminServer(up.AdminServerOptions{ListenAddr: addr})
	admin.RegisterPprof()
	admin.HandleFunc("/pending/size", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]int{"size": down.StreamObjectBagCount()})
	})
	admin.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("ok\n"))
	})
	up.Register("orange-debug", func(*up.Writer, *up.Request) {}, up.WithAdminServer(admin))
}
