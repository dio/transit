package filters

import (
	"strconv"

	"github.com/dio/transit/up"
)

func init() {
	up.RegisterWithBody("e2e-body", bodyOnRequest, bodyOnRequestBody, bodyOnResponse)
	up.RegisterWithMutableBody("e2e-mutable-body", mutableOnRequest, mutableOnRequestBody, mutableOnResponse)
}

type bodyCtx struct {
	hasData bool
	dataLen int
}

func bodyOnRequest(_ *up.Writer, _ *up.Request) {}

// bodyOnRequestBody fires once per chunk (streaming) or once with full body (buffered).
// For bodyless requests (GET etc.) it fires synthetically with Data: nil.
func bodyOnRequestBody(_ *up.Writer, chunk *up.BodyChunk) {
	if !chunk.EndStream {
		return
	}
	*chunk.Context = &bodyCtx{
		hasData: chunk.Data != nil,
		dataLen: len(chunk.Data),
	}
}

// bodyOnResponse fires in OnResponseHeaders (StatusCode != 0) and again for
// the synthetic body call (StatusCode == 0). We only act on the headers call
// where we can still modify response headers.
func bodyOnResponse(w *up.Writer, chunk *up.ResponseChunk) {
	if chunk.StatusCode == 0 {
		return
	}
	ctx, ok := (*chunk.Context).(*bodyCtx)
	if !ok {
		return
	}
	if ctx.hasData {
		w.SetResponseHeader("x-body-len", strconv.Itoa(ctx.dataLen))
	} else {
		w.SetResponseHeader("x-body-len", "none")
	}
}

func mutableOnRequest(_ *up.Writer, _ *up.Request) {}

// mutableOnRequestBody replaces the request body with "replaced:<original>" and
// stores the replacement length in the context so the response handler can echo it.
func mutableOnRequestBody(w *up.Writer, chunk *up.BodyChunk) {
	if !chunk.EndStream {
		return
	}
	replacement := []byte("replaced:" + string(chunk.Data))
	w.SetRequestBody(replacement)
	*chunk.Context = len(replacement)
}

func mutableOnResponse(w *up.Writer, chunk *up.ResponseChunk) {
	if chunk.StatusCode == 0 {
		return
	}
	if n, ok := (*chunk.Context).(int); ok {
		w.SetResponseHeader("x-replaced-len", strconv.Itoa(n))
	}
}
