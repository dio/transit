// e2e-stream-finalized exercises up.WithOnStreamFinalized. The filter stashes
// a per-stream value in r.Context; the OnStreamFinalized callback runs at
// stream finalization and increments atomic counters that record what
// FinalizedInfo carried.
//
// The loopback HTTP server (addr = E2E_STREAM_FINALIZED_LOOPBACK_ADDR, started
// via WithGroup) serves two endpoints:
//
//	GET /          — proxied through Envoy; returns "ok\n".
//	GET /counters  — JSON snapshot of the counters. Tests hit this directly
//	                 on the loopback port so the read does not itself drive
//	                 OnStreamFinalized.
package filters

import (
	"encoding/json"
	"net"
	"net/http"
	"os"
	"sync/atomic"

	"github.com/dio/transit/up"
)

type StreamFinalizedCounters struct {
	Fired                uint64 `json:"fired"`
	ContextOK            uint64 `json:"context_ok"`
	NilContext           uint64 `json:"nil_context"`
	NonZeroResponseCode  uint64 `json:"nonzero_response_code"`
	NonZeroBytesReceived uint64 `json:"nonzero_bytes_received"`
	HasResponseFlags     uint64 `json:"has_response_flags"`
	HasLocalReplyBody    uint64 `json:"has_local_reply_body"`
}

var (
	streamFinalizedFired                atomic.Uint64
	streamFinalizedContextOK            atomic.Uint64
	streamFinalizedNilContext           atomic.Uint64
	streamFinalizedNonZeroResponseCode  atomic.Uint64
	streamFinalizedNonZeroBytesReceived atomic.Uint64
	streamFinalizedHasResponseFlags     atomic.Uint64
	streamFinalizedHasLocalReplyBody    atomic.Uint64
)

type streamFinalizedState struct{ path string }

func init() {
	addr := os.Getenv("E2E_STREAM_FINALIZED_LOOPBACK_ADDR")

	mux := http.NewServeMux()
	mux.HandleFunc("/counters", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(StreamFinalizedCounters{
			Fired:                streamFinalizedFired.Load(),
			ContextOK:            streamFinalizedContextOK.Load(),
			NilContext:           streamFinalizedNilContext.Load(),
			NonZeroResponseCode:  streamFinalizedNonZeroResponseCode.Load(),
			NonZeroBytesReceived: streamFinalizedNonZeroBytesReceived.Load(),
			HasResponseFlags:     streamFinalizedHasResponseFlags.Load(),
			HasLocalReplyBody:    streamFinalizedHasLocalReplyBody.Load(),
		})
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("ok\n"))
	})

	// Start the loopback HTTP server directly from init() rather than via
	// up.WithGroup: the filter is referenced by three listeners, so the
	// configFactory's group.Start would be called three times, racing
	// net.Listen on the same port and tearing the whole group down on the
	// "address in use" error from the second start.
	if addr != "" {
		ln, err := net.Listen("tcp", addr)
		if err != nil {
			panic("e2e-stream-finalized loopback listen: " + err.Error())
		}
		srv := &http.Server{Handler: mux}
		go func() { _ = srv.Serve(ln) }()
	}

	up.Register("e2e-stream-finalized",
		func(_ *up.Writer, r *up.Request) {
			if r.Context != nil {
				*r.Context = &streamFinalizedState{path: r.Path}
			}
		},
		up.WithOnStreamFinalized(func(ctx *any, info up.FinalizedInfo) {
			streamFinalizedFired.Add(1)
			if ctx == nil || *ctx == nil {
				streamFinalizedNilContext.Add(1)
			} else if _, ok := (*ctx).(*streamFinalizedState); ok {
				streamFinalizedContextOK.Add(1)
			} else {
				streamFinalizedNilContext.Add(1)
			}
			if info.ResponseCode != 0 {
				streamFinalizedNonZeroResponseCode.Add(1)
			}
			if info.Bytes.BytesReceived > 0 || info.Bytes.WireBytesReceived > 0 {
				streamFinalizedNonZeroBytesReceived.Add(1)
			}
			if info.ResponseFlags != 0 {
				streamFinalizedHasResponseFlags.Add(1)
			}
			if info.LocalReplyBody != "" {
				streamFinalizedHasLocalReplyBody.Add(1)
			}
		}),
	)
}
