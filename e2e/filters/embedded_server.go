// e2e-embedded-server exercises up.RegisterWithGroup with a net.Listen embedded
// HTTP server. The Group starts a plain net/http server on the address given by
// E2E_EMBEDDED_SERVER_ADDR; Envoy routes requests to it via a STATIC loopback
// cluster. This is the canonical pattern used in production by examples/ws-proxy.
package filters

import (
	"fmt"
	"net"
	"net/http"
	"os"

	"github.com/dio/transit/up"
)

func init() {
	addr := os.Getenv("E2E_EMBEDDED_SERVER_ADDR")

	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("x-embedded-server", "ran")
		fmt.Fprintln(w, "ok") //nolint:errcheck
	})

	srv := &http.Server{Handler: mux}
	g := up.NewGroup()
	if addr != "" {
		g.Add(
			func() error {
				ln, err := net.Listen("tcp", addr)
				if err != nil {
					return err
				}
				return srv.Serve(ln)
			},
			func() { _ = srv.Close() },
		)
	}

	up.RegisterWithGroup("e2e-embedded-server", g, func(_ *up.Writer, _ *up.Request) {})
}
