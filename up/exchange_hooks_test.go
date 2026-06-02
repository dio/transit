package up

import (
	"testing"

	"github.com/envoyproxy/envoy/source/extensions/dynamic_modules/sdk/go/shared/fake"
	"github.com/stretchr/testify/require"

	"github.com/dio/transit/up/testutil"
)

// applyExchangeHooks returns the three phase functions extracted from a fresh
// configFactory after applying WithExchangeObserver. Used only in tests.
func applyExchangeHooks[T any](hooks ExchangeHooks[T]) (HandlerFunc, ResponseHandlerFunc, OnStreamFinalizedFunc) {
	cf := &configFactory{}
	for _, o := range WithExchangeObserver(hooks) {
		o(cf)
	}
	return cf.handler, cf.responseHandler, cf.onStreamFinalized
}

// newExchangeFilter creates a filter wired with the given phase functions and a fresh handle.
func newExchangeFilter(h HandlerFunc, rh ResponseHandlerFunc, fin OnStreamFinalizedFunc) (*filter, *testutil.FakeFilterHandle) {
	handle := testutil.NewFilterHandle()
	return &filter{
		name:              "test-exchange",
		handle:            handle,
		handler:           h,
		responseHandler:   rh,
		onStreamFinalized: fin,
	}, handle
}

func getHeaders() *fake.FakeHeaderMap {
	return fake.NewFakeHeaderMap(map[string][]string{
		":method": {"GET"},
		":path":   {"/"},
	})
}

// TestExchangeHooks_onRequestToFinalized verifies that OnRequest initializes
// state and OnFinalized receives the same value.
func TestExchangeHooks_onRequestToFinalized(t *testing.T) {
	type accum struct{ method string }

	var got accum
	handler, _, fin := applyExchangeHooks(ExchangeHooks[accum]{
		OnRequest:   func(_ *Writer, r *Request) accum { return accum{method: r.Method} },
		OnFinalized: func(s accum, _ FinalizedInfo) { got = s },
	})

	f, _ := newExchangeFilter(handler, nil, fin)
	f.OnRequestHeaders(fake.NewFakeHeaderMap(map[string][]string{
		":method": {"DELETE"},
		":path":   {"/item"},
	}), true)

	fin(&f.context, FinalizedInfo{})
	require.Equal(t, "DELETE", got.method)
}

// TestExchangeHooks_onResponseHeadersAndBody verifies that OnResponse fires for
// both the headers phase (chunk.StatusCode != 0) and the body phase
// (chunk.Data != nil). Uses a pointer accumulator so the callback can mutate state.
func TestExchangeHooks_onResponseHeadersAndBody(t *testing.T) {
	type accum struct {
		statusCode int
		body       string
	}

	var callCount int
	var gotCodes []int
	var gotBody string

	handler, rh, fin := applyExchangeHooks(ExchangeHooks[*accum]{
		OnRequest: func(_ *Writer, _ *Request) *accum { return &accum{} },
		OnResponse: func(s *accum, _ *Writer, chunk *ResponseChunk) {
			callCount++
			if chunk.StatusCode != 0 {
				s.statusCode = chunk.StatusCode
				gotCodes = append(gotCodes, chunk.StatusCode)
			}
			if len(chunk.Data) > 0 {
				s.body += string(chunk.Data)
				gotBody = s.body
			}
		},
		OnFinalized: func(s *accum, _ FinalizedInfo) {},
	})

	f, _ := newExchangeFilter(handler, rh, fin)
	f.OnRequestHeaders(getHeaders(), true)

	// Headers phase.
	f.OnResponseHeaders(fake.NewFakeHeaderMap(map[string][]string{":status": {"200"}}), false)
	require.Equal(t, 1, callCount)
	require.Equal(t, []int{200}, gotCodes)

	// Body phase — call responseHandler directly to simulate a body chunk.
	w := &Writer{f: f, directWrite: true}
	f.responseHandler(w, &ResponseChunk{Data: []byte("hi"), EndStream: true, Context: &f.context})
	require.Equal(t, 2, callCount)
	require.Equal(t, "hi", gotBody)
}

// TestExchangeHooks_nilOnResponseIsSafe verifies that omitting OnResponse
// produces a nil responseHandler and causes no panic during the request phase.
func TestExchangeHooks_nilOnResponseIsSafe(t *testing.T) {
	handler, rh, _ := applyExchangeHooks(ExchangeHooks[int]{
		OnRequest: func(_ *Writer, _ *Request) int { return 7 },
	})
	require.Nil(t, rh)

	f, _ := newExchangeFilter(handler, rh, nil)
	require.NotPanics(t, func() {
		f.OnRequestHeaders(getHeaders(), true)
	})
}

// TestExchangeHooks_localReplyPath verifies that OnFinalized fires even when
// Envoy sent a local reply (FinalizedInfo.ResponseCode set, LocalReplyBody non-empty).
func TestExchangeHooks_localReplyPath(t *testing.T) {
	type accum struct{ path string }

	var got accum
	var gotInfo FinalizedInfo

	handler, _, fin := applyExchangeHooks(ExchangeHooks[accum]{
		OnRequest:   func(_ *Writer, r *Request) accum { return accum{path: r.Path} },
		OnFinalized: func(s accum, info FinalizedInfo) { got = s; gotInfo = info },
	})

	f, _ := newExchangeFilter(handler, nil, fin)
	f.OnRequestHeaders(fake.NewFakeHeaderMap(map[string][]string{
		":method": {"GET"},
		":path":   {"/secure"},
	}), true)

	info := FinalizedInfo{ResponseCode: 403, LocalReplyBody: `{"error":"forbidden"}`}
	fin(&f.context, info)

	require.Equal(t, "/secure", got.path)
	require.Equal(t, uint32(403), gotInfo.ResponseCode)
	require.Equal(t, `{"error":"forbidden"}`, gotInfo.LocalReplyBody)
}

// TestExchangeHooks_upstreamFailurePath verifies OnFinalized fires for
// upstream-failure streams (UpstreamFailure non-empty).
func TestExchangeHooks_upstreamFailurePath(t *testing.T) {
	type accum struct{ method string }

	var got accum
	var gotInfo FinalizedInfo

	handler, _, fin := applyExchangeHooks(ExchangeHooks[accum]{
		OnRequest:   func(_ *Writer, r *Request) accum { return accum{method: r.Method} },
		OnFinalized: func(s accum, info FinalizedInfo) { got = s; gotInfo = info },
	})

	f, _ := newExchangeFilter(handler, nil, fin)
	f.OnRequestHeaders(fake.NewFakeHeaderMap(map[string][]string{
		":method": {"DELETE"},
		":path":   {"/item"},
	}), true)

	const failure = "upstream_reset_before_response_started{connection_failure}"
	fin(&f.context, FinalizedInfo{UpstreamFailure: failure})

	require.Equal(t, "DELETE", got.method)
	require.Equal(t, failure, gotInfo.UpstreamFailure)
}

// TestExchangeHooks_poolReuse verifies that calling fin twice on the same
// context is a no-op on the second call (context is nil after first delivery),
// and that the pool slot is zeroed before reuse.
func TestExchangeHooks_poolReuse(t *testing.T) {
	type accum struct{ val int }

	var received []int

	handler, _, fin := applyExchangeHooks(ExchangeHooks[accum]{
		OnRequest:   func(_ *Writer, _ *Request) accum { return accum{val: 42} },
		OnFinalized: func(s accum, _ FinalizedInfo) { received = append(received, s.val) },
	})

	// Stream 1.
	f1, _ := newExchangeFilter(handler, nil, fin)
	f1.OnRequestHeaders(getHeaders(), true)
	fin(&f1.context, FinalizedInfo{})
	// Second call must be a no-op (context cleared).
	fin(&f1.context, FinalizedInfo{})

	// Stream 2 — pool may return the same slot, but zeroed.
	f2, _ := newExchangeFilter(handler, nil, fin)
	f2.OnRequestHeaders(getHeaders(), true)
	fin(&f2.context, FinalizedInfo{})

	require.Equal(t, []int{42, 42}, received)
}

// TestTruncateBody verifies that TruncateBody truncates at the limit and is a
// no-op when data fits.
func TestTruncateBody(t *testing.T) {
	data := []byte("hello world")
	require.Equal(t, []byte("hello"), TruncateBody(data, 5))
	require.Equal(t, data, TruncateBody(data, len(data)))
	require.Equal(t, data, TruncateBody(data, 100))
	require.Nil(t, TruncateBody(nil, 10))
	require.Equal(t, []byte{}, TruncateBody([]byte{}, 10))
}
