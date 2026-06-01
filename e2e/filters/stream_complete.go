// e2e-stream-complete exercises up.WithOnStreamComplete. The request handler
// stashes a per-stream value in r.Context; the OnStreamComplete callback runs
// at stream termination and increments atomic counters that the test reads via
// a loopback HTTP server.
//
// Two endpoints are exposed by the loopback server (addr =
// E2E_STREAM_COMPLETE_LOOPBACK_ADDR, started via WithGroup):
//
//   GET /          — returned to clients that route through Envoy; "ok\n".
//   GET /counters  — JSON snapshot of the counters. Tests hit this directly
//                    on the loopback port so the read does not itself drive
//                    OnStreamComplete.
package filters

import (
	"encoding/json"
	"net"
	"net/http"
	"os"
	"sync/atomic"

	"github.com/dio/transit/up"
)

type StreamCompleteCounters struct {
	Fired      uint64 `json:"fired"`
	ContextOK  uint64 `json:"context_ok"`
	NilContext uint64 `json:"nil_context"`
}

var (
	streamCompleteFired      atomic.Uint64
	streamCompleteContextOK  atomic.Uint64
	streamCompleteNilContext atomic.Uint64
)

type streamCompleteState struct{ path string }

func init() {
	addr := os.Getenv("E2E_STREAM_COMPLETE_LOOPBACK_ADDR")

	mux := http.NewServeMux()
	mux.HandleFunc("/counters", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(StreamCompleteCounters{
			Fired:      streamCompleteFired.Load(),
			ContextOK:  streamCompleteContextOK.Load(),
			NilContext: streamCompleteNilContext.Load(),
		})
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("ok\n"))
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

	up.Register("e2e-stream-complete",
		func(_ *up.Writer, r *up.Request) {
			if r.Context != nil {
				*r.Context = &streamCompleteState{path: r.Path}
			}
		},
		up.WithGroup(g),
		up.WithOnStreamComplete(func(ctx *any) {
			streamCompleteFired.Add(1)
			if ctx == nil || *ctx == nil {
				streamCompleteNilContext.Add(1)
				return
			}
			if _, ok := (*ctx).(*streamCompleteState); ok {
				streamCompleteContextOK.Add(1)
				return
			}
			streamCompleteNilContext.Add(1)
		}),
	)
}
