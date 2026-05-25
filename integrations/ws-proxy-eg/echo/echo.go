// Package echo is the minimal echo WebSocket proxy used for P1 structural
// validation of the Envoy Gateway integration. It contains no auth, no
// metering, and no upstream dial — it simply accepts a WS connection and
// echoes every frame back to the sender.
//
// This validates the three EG gates:
//   - Gate 1: upgrade_configs injected via EPP (Envoy accepts WS upgrades)
//   - Gate 2: WS frames pass intact through the EPP-replaced STATIC loopback cluster
//   - Gate 3: plain HTTP to /v1/responses returns 400 (non-WS rejected before Accept)
package echo

import (
	"fmt"
	"net"
	"net/http"
	"os"

	"github.com/coder/websocket"

	"github.com/dio/transit/up"
)

const ExtensionName = "ws-proxy"

// Register wires the echo filter into Envoy.
func Register() {
	addr := "127.0.0.1:10001"
	if v := os.Getenv("WSPROXY_LISTEN_ADDR"); v != "" {
		addr = v
	}

	g := up.NewGroup()
	g.Add(
		func() error {
			ln, err := net.Listen("tcp", addr)
			if err != nil {
				fmt.Fprintf(os.Stderr, "ws-proxy-echo: listen %s: %v\n", addr, err)
				return err
			}
			fmt.Fprintf(os.Stderr, "ws-proxy-echo: listening on %s\n", ln.Addr())
			srv := &http.Server{Handler: http.HandlerFunc(serveEcho)}
			return srv.Serve(ln)
		},
		func() {},
	)
	up.RegisterWithGroup(ExtensionName, g, func(w *up.Writer, r *up.Request) {})
}

// serveEcho accepts a WS upgrade and echoes every frame back.
// Gate 3: rejects non-WS requests with 400 before calling websocket.Accept.
func serveEcho(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get("Upgrade") != "websocket" {
		http.Error(w, `{"error":"WebSocket upgrade required"}`, http.StatusBadRequest)
		return
	}
	conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{InsecureSkipVerify: true})
	if err != nil {
		fmt.Fprintf(os.Stderr, "ws-proxy-echo: accept: %v\n", err)
		return
	}
	defer conn.CloseNow()
	ctx := r.Context()
	for {
		typ, data, err := conn.Read(ctx)
		if err != nil {
			return
		}
		if err := conn.Write(ctx, typ, data); err != nil {
			return
		}
	}
}
