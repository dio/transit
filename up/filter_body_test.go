package up

import (
	"testing"

	"github.com/envoyproxy/envoy/source/extensions/dynamic_modules/sdk/go/shared"
	"github.com/envoyproxy/envoy/source/extensions/dynamic_modules/sdk/go/shared/fake"
	"github.com/envoyproxy/envoy/source/extensions/dynamic_modules/sdk/go/shared/mocks"
	"github.com/stretchr/testify/require"
	"go.uber.org/mock/gomock"
)

// ── helpers ──────────────────────────────────────────────────────────────────

// mockHandle wraps the SDK mock and fixes GetAttributeNumber's return type.
// The SDK mock was generated against the old interface (uint64) but shared now
// returns float64. The wrapper satisfies the current interface; no test in this
// file exercises GetAttributeNumber, so the no-op stub is sufficient.
type mockHandle struct{ *mocks.MockHttpFilterHandle }

func (h *mockHandle) GetAttributeNumber(_ shared.AttributeID) (float64, bool) { return 0, false }

func newMockHandle(ctrl *gomock.Controller) *mockHandle {
	return &mockHandle{mocks.NewMockHttpFilterHandle(ctrl)}
}

func noopHandler(_ *Writer, _ *Request) {}

func newFilterWithBody(handle shared.HttpFilterHandle, rb RequestBodyHandlerFunc) *filter {
	return &filter{handle: handle, handler: noopHandler, requestBodyHandler: rb}
}

func newFilterWithBodyBuffered(handle shared.HttpFilterHandle, rb RequestBodyHandlerFunc) *filter {
	return &filter{handle: handle, handler: noopHandler, requestBodyHandler: rb, bufferBody: true, mutableRequestBody: true}
}

func newFilterWithReadOnlyBody(handle shared.HttpFilterHandle, rb RequestBodyHandlerFunc) *filter {
	return &filter{handle: handle, handler: noopHandler, requestBodyHandler: rb, bufferBody: true}
}

func newFilterWithResponse(handle shared.HttpFilterHandle, r ResponseHandlerFunc) *filter {
	return &filter{handle: handle, handler: noopHandler, responseHandler: r}
}

func newFilterWithResponseBuffered(handle shared.HttpFilterHandle, r ResponseHandlerFunc) *filter {
	return &filter{handle: handle, handler: noopHandler, responseHandler: r, bufferBody: true}
}

// ── OnRequestHeaders — body handler ──────────────────────────────────────────

// Bodyless requests (GET, DELETE, …) must trigger a synthetic body call with
// Data: nil and EndStream: true so body-dependent logic has a single completion
// point regardless of whether a real body exists.
func TestFilter_OnRequestHeaders_bodyHandler_endOfStream_syntheticCall(t *testing.T) {
	ctrl := gomock.NewController(t)
	handle := newMockHandle(ctrl)

	var got *BodyChunk
	f := newFilterWithBody(handle, func(_ *Writer, c *BodyChunk) { got = c })

	status := f.OnRequestHeaders(fake.NewFakeHeaderMap(nil), true)

	require.Equal(t, shared.HeadersStatusContinue, status)
	require.NotNil(t, got)
	require.Nil(t, got.Data)
	require.True(t, got.EndStream)
}

// Body handler must NOT be called synthetically when the request has a body
// (endOfStream=false); that call will come from OnRequestBody.
func TestFilter_OnRequestHeaders_bodyHandler_notEndOfStream_noSyntheticCall(t *testing.T) {
	ctrl := gomock.NewController(t)
	handle := newMockHandle(ctrl)

	called := false
	reqHeaders := fake.NewFakeHeaderMap(nil)
	f := newFilterWithBody(handle, func(_ *Writer, _ *BodyChunk) { called = true })

	f.OnRequestHeaders(reqHeaders, false)

	require.False(t, called)
}

// ContentType and ContentEncoding from the request headers must be captured and
// forwarded to the body handler via the BodyChunk fields.
func TestFilter_OnRequestHeaders_bodyHandler_capturesContentMetadata(t *testing.T) {
	ctrl := gomock.NewController(t)
	handle := newMockHandle(ctrl)

	var got *BodyChunk
	headers := fake.NewFakeHeaderMap(map[string][]string{
		"content-type":     {"application/json"},
		"content-encoding": {"gzip"},
	})
	f := newFilterWithBody(handle, func(_ *Writer, c *BodyChunk) { got = c })

	f.OnRequestHeaders(headers, true) // endOfStream=true → synthetic call

	require.Equal(t, "application/json", got.ContentType)
	require.Equal(t, "gzip", got.ContentEncoding)
}

func TestFilter_OnRequestBody_writerReadsRequestHeaders(t *testing.T) {
	ctrl := gomock.NewController(t)
	handle := newMockHandle(ctrl)

	headers := fake.NewFakeHeaderMap(map[string][]string{
		"x-provider": {"openai"},
	})
	handle.EXPECT().RequestHeaders().Return(headers).AnyTimes()

	var got string
	f := newFilterWithBody(handle, func(w *Writer, _ *BodyChunk) {
		got = w.RequestHeader("x-provider")
	})

	f.OnRequestHeaders(headers, false)
	status := f.OnRequestBody(fake.NewFakeBodyBuffer([]byte(`{}`)), true)

	require.Equal(t, shared.BodyStatusContinue, status)
	require.Equal(t, "openai", got)
}

// In buffered mode, content-length and transfer-encoding must be removed from
// request headers before they are forwarded, since the body may be replaced.
// The filter must return Stop (not StopAllAndBuffer — that freezes the chain).
func TestFilter_OnRequestHeaders_bufferedMode_stripsLengthHeaders(t *testing.T) {
	ctrl := gomock.NewController(t)
	handle := newMockHandle(ctrl)

	headers := fake.NewFakeHeaderMap(map[string][]string{
		"content-length":    {"42"},
		"transfer-encoding": {"chunked"},
		"content-type":      {"text/plain"},
	})
	// flush calls handle.RequestHeaders() to apply the framing strip after queued
	// mutations; stub it to return the same map the test passes to OnRequestHeaders.
	handle.EXPECT().RequestHeaders().Return(headers).AnyTimes()
	f := newFilterWithBodyBuffered(handle, func(_ *Writer, _ *BodyChunk) {})

	status := f.OnRequestHeaders(headers, false)

	require.Equal(t, shared.HeadersStatusStop, status)
	require.Empty(t, headers.Headers["content-length"])
	require.Empty(t, headers.Headers["transfer-encoding"])
	require.NotEmpty(t, headers.Headers["content-type"]) // unrelated header untouched
}

func TestFilter_OnRequestHeaders_readOnlyBody_preservesLengthHeaders(t *testing.T) {
	ctrl := gomock.NewController(t)
	handle := newMockHandle(ctrl)

	headers := fake.NewFakeHeaderMap(map[string][]string{
		"content-length":    {"42"},
		"transfer-encoding": {"chunked"},
		"content-type":      {"text/plain"},
	})
	f := newFilterWithReadOnlyBody(handle, func(_ *Writer, _ *BodyChunk) {})

	status := f.OnRequestHeaders(headers, false)

	require.Equal(t, shared.HeadersStatusStop, status)
	require.Equal(t, "42", headers.Headers["content-length"][0])
	require.Equal(t, "chunked", headers.Headers["transfer-encoding"][0])
	require.Equal(t, "text/plain", headers.Headers["content-type"][0])
}

// Buffered mode with endOfStream=true still issues the synthetic body call and
// must NOT strip headers (there is no body to replace).
func TestFilter_OnRequestHeaders_bufferedMode_endOfStream_syntheticCall(t *testing.T) {
	ctrl := gomock.NewController(t)
	handle := newMockHandle(ctrl)

	var got *BodyChunk
	f := newFilterWithBodyBuffered(handle, func(_ *Writer, c *BodyChunk) { got = c })

	f.OnRequestHeaders(fake.NewFakeHeaderMap(nil), true)

	require.NotNil(t, got)
	require.True(t, got.EndStream)
}

// ── OnRequestBody ─────────────────────────────────────────────────────────────

func TestFilter_OnRequestBody_noHandler_returnsContinue(t *testing.T) {
	ctrl := gomock.NewController(t)
	handle := newMockHandle(ctrl)
	f := &filter{handle: handle, handler: noopHandler}

	status := f.OnRequestBody(fake.NewFakeBodyBuffer([]byte("data")), true)
	require.Equal(t, shared.BodyStatusContinue, status)
}

// Streaming mode: non-final chunk must pass through immediately.
func TestFilter_OnRequestBody_streaming_notEndOfStream_returnsContinue(t *testing.T) {
	ctrl := gomock.NewController(t)
	handle := newMockHandle(ctrl)

	called := false
	f := newFilterWithBody(handle, func(_ *Writer, _ *BodyChunk) { called = true })

	status := f.OnRequestBody(fake.NewFakeBodyBuffer([]byte("partial")), false)

	require.Equal(t, shared.BodyStatusContinue, status)
	require.True(t, called) // streaming: handler fires per chunk
}

// Streaming mode: final chunk delivers data and EndStream=true.
func TestFilter_OnRequestBody_streaming_endOfStream_callsHandlerWithData(t *testing.T) {
	ctrl := gomock.NewController(t)
	handle := newMockHandle(ctrl)

	var got *BodyChunk
	f := newFilterWithBody(handle, func(_ *Writer, c *BodyChunk) { got = c })

	f.OnRequestBody(fake.NewFakeBodyBuffer([]byte("hello")), true)

	require.Equal(t, []byte("hello"), got.Data)
	require.True(t, got.EndStream)
}

// Buffered mode: non-final chunk must be held — the handler must not fire yet.
func TestFilter_OnRequestBody_buffered_notEndOfStream_returnsStopAndBuffer(t *testing.T) {
	ctrl := gomock.NewController(t)
	handle := newMockHandle(ctrl)

	called := false
	f := newFilterWithBodyBuffered(handle, func(_ *Writer, _ *BodyChunk) { called = true })

	status := f.OnRequestBody(fake.NewFakeBodyBuffer([]byte("partial")), false)

	require.Equal(t, shared.BodyStatusStopAndBuffer, status)
	require.False(t, called)
}

// Buffered mode: when the final chunk arrives the handler fires once with the
// full accumulated body from BufferedRequestBody.
func TestFilter_OnRequestBody_buffered_endOfStream_callsHandlerWithBufferedData(t *testing.T) {
	ctrl := gomock.NewController(t)
	handle := newMockHandle(ctrl)
	buf := fake.NewFakeBodyBuffer([]byte("full body"))
	reqHeaders := fake.NewFakeHeaderMap(nil)
	handle.EXPECT().BufferedRequestBody().Return(buf).AnyTimes()
	handle.EXPECT().RequestHeaders().Return(reqHeaders).AnyTimes()

	var got *BodyChunk
	f := newFilterWithBodyBuffered(handle, func(_ *Writer, c *BodyChunk) { got = c })

	status := f.OnRequestBody(fake.NewFakeBodyBuffer(nil), true)

	require.Equal(t, shared.BodyStatusContinue, status)
	require.Equal(t, []byte("full body"), got.Data)
	require.True(t, got.EndStream)
	require.Equal(t, "9", reqHeaders.Headers["content-length"][0])
}

// Buffered mode: upstream filter chains don't pre-fill BufferedRequestBody on
// the first endOfStream=true call (no prior StopAndBuffer). The body argument
// must be used as fallback so the handler sees the actual data, not an empty slice.
func TestFilter_OnRequestBody_buffered_upstreamChain_usesBodyWhenBufferEmpty(t *testing.T) {
	ctrl := gomock.NewController(t)
	handle := newMockHandle(ctrl)
	emptyBuf := fake.NewFakeBodyBuffer(nil)
	reqHeaders := fake.NewFakeHeaderMap(nil)
	handle.EXPECT().BufferedRequestBody().Return(emptyBuf).AnyTimes()
	handle.EXPECT().RequestHeaders().Return(reqHeaders).AnyTimes()

	var got *BodyChunk
	f := newFilterWithBodyBuffered(handle, func(_ *Writer, c *BodyChunk) { got = c })

	status := f.OnRequestBody(fake.NewFakeBodyBuffer([]byte("upstream body")), true)

	require.Equal(t, shared.BodyStatusContinue, status)
	require.Equal(t, []byte("upstream body"), got.Data)
	require.True(t, got.EndStream)
	require.Equal(t, "13", reqHeaders.Headers["content-length"][0])
}

// Buffered mode with SetRequestBody: the buffer must be drained and refilled
// with the replacement, and content-length must be updated.
func TestFilter_OnRequestBody_buffered_replacement_updatesBufferAndHeader(t *testing.T) {
	ctrl := gomock.NewController(t)
	handle := newMockHandle(ctrl)

	buf := fake.NewFakeBodyBuffer([]byte("original"))
	reqHeaders := fake.NewFakeHeaderMap(nil)
	handle.EXPECT().BufferedRequestBody().Return(buf).AnyTimes()
	handle.EXPECT().RequestHeaders().Return(reqHeaders).AnyTimes()

	replacement := []byte("replaced")
	f := newFilterWithBodyBuffered(handle, func(w *Writer, _ *BodyChunk) {
		w.SetRequestBody(replacement)
	})

	f.OnRequestBody(fake.NewFakeBodyBuffer(nil), true)

	require.Equal(t, replacement, buf.Body)
	require.Equal(t, "8", reqHeaders.Headers["content-length"][0])
}

func TestFilter_OnRequestBody_buffered_replacementContentLengthWinsAfterQueuedHeaderMutation(t *testing.T) {
	ctrl := gomock.NewController(t)
	handle := newMockHandle(ctrl)

	buf := fake.NewFakeBodyBuffer([]byte("original"))
	reqHeaders := fake.NewFakeHeaderMap(map[string][]string{
		"content-length": {"999"},
	})
	handle.EXPECT().BufferedRequestBody().Return(buf).AnyTimes()
	handle.EXPECT().RequestHeaders().Return(reqHeaders).AnyTimes()

	replacement := []byte("rewritten-body")
	f := newFilterWithBodyBuffered(handle, func(w *Writer, _ *BodyChunk) {
		w.RemoveRequestHeader("content-length")
		w.SetRequestBody(replacement)
	})

	status := f.OnRequestBody(fake.NewFakeBodyBuffer(nil), true)

	require.Equal(t, shared.BodyStatusContinue, status)
	require.Equal(t, replacement, buf.Body)
	require.Equal(t, "14", reqHeaders.Headers["content-length"][0])
}

func TestFilter_OnRequestBody_readOnlyBody_ignoresSetRequestBodyAndPreservesLength(t *testing.T) {
	ctrl := gomock.NewController(t)
	handle := newMockHandle(ctrl)

	buf := fake.NewFakeBodyBuffer([]byte("original"))
	reqHeaders := fake.NewFakeHeaderMap(map[string][]string{
		"content-length": {"8"},
	})
	handle.EXPECT().BufferedRequestBody().Return(buf).AnyTimes()
	handle.EXPECT().RequestHeaders().Return(reqHeaders).AnyTimes()

	f := newFilterWithReadOnlyBody(handle, func(w *Writer, _ *BodyChunk) {
		w.SetRequestBody([]byte("rewritten-body"))
	})

	status := f.OnRequestBody(fake.NewFakeBodyBuffer(nil), true)

	require.Equal(t, shared.BodyStatusContinue, status)
	require.Equal(t, []byte("original"), buf.Body)
	require.Equal(t, "8", reqHeaders.Headers["content-length"][0])
}

// Context pointer is the same per-stream slot across the handler and body handler.
func TestFilter_OnRequestBody_contextSharedWithHandler(t *testing.T) {
	ctrl := gomock.NewController(t)
	handle := newMockHandle(ctrl)

	var ctxFromHeaders *any
	var ctxFromBody *any

	f := &filter{
		handle: handle,
		handler: func(w *Writer, r *Request) {
			// access the same context that body handler will receive
		},
		requestBodyHandler: func(_ *Writer, c *BodyChunk) {
			ctxFromBody = c.Context
			*c.Context = "marker"
		},
	}

	// Synthetic call via OnRequestHeaders sets context.
	f.OnRequestHeaders(fake.NewFakeHeaderMap(nil), true)
	ctxFromHeaders = &f.context

	require.Same(t, ctxFromHeaders, ctxFromBody)
	require.Equal(t, "marker", f.context)
}

// ── OnResponseHeaders ─────────────────────────────────────────────────────────

func TestFilter_OnResponseHeaders_noHandler_returnsContinue(t *testing.T) {
	ctrl := gomock.NewController(t)
	handle := newMockHandle(ctrl)
	f := &filter{handle: handle, handler: noopHandler}

	status := f.OnResponseHeaders(fake.NewFakeHeaderMap(map[string][]string{":status": {"200"}}), false)
	require.Equal(t, shared.HeadersStatusContinue, status)
}

// The response handler must receive the correct HTTP status code.
func TestFilter_OnResponseHeaders_callsHandlerWithStatusCode(t *testing.T) {
	ctrl := gomock.NewController(t)
	handle := newMockHandle(ctrl)

	var gotStatus int
	f := newFilterWithResponse(handle, func(_ *Writer, c *ResponseChunk) {
		if c.StatusCode != 0 {
			gotStatus = c.StatusCode
		}
	})

	f.OnResponseHeaders(fake.NewFakeHeaderMap(map[string][]string{":status": {"404"}}), false)
	require.Equal(t, 404, gotStatus)
}

// When endOfStream=true in response headers (204, HEAD, etc.) the handler must
// be called twice: once with the headers chunk (StatusCode!=0) and once with a
// synthetic body chunk (StatusCode==0, EndStream=true).
func TestFilter_OnResponseHeaders_endOfStream_syntheticBodyCall(t *testing.T) {
	ctrl := gomock.NewController(t)
	handle := newMockHandle(ctrl)

	var calls []ResponseChunk
	f := newFilterWithResponse(handle, func(_ *Writer, c *ResponseChunk) {
		calls = append(calls, *c)
	})

	f.OnResponseHeaders(fake.NewFakeHeaderMap(map[string][]string{":status": {"204"}}), true)

	require.Len(t, calls, 2)
	require.Equal(t, 204, calls[0].StatusCode)
	require.True(t, calls[0].EndStream)
	require.Equal(t, 0, calls[1].StatusCode) // synthetic body call
	require.True(t, calls[1].EndStream)
}

// When endOfStream=false there must be exactly one call (headers only).
func TestFilter_OnResponseHeaders_notEndOfStream_singleCall(t *testing.T) {
	ctrl := gomock.NewController(t)
	handle := newMockHandle(ctrl)

	calls := 0
	f := newFilterWithResponse(handle, func(_ *Writer, _ *ResponseChunk) { calls++ })

	f.OnResponseHeaders(fake.NewFakeHeaderMap(map[string][]string{":status": {"200"}}), false)
	require.Equal(t, 1, calls)
}

// ContentType and ContentEncoding from response headers must be forwarded to
// every ResponseChunk (headers call and body calls).
func TestFilter_OnResponseHeaders_capturesContentMetadata(t *testing.T) {
	ctrl := gomock.NewController(t)
	handle := newMockHandle(ctrl)

	var chunks []ResponseChunk
	f := newFilterWithResponse(handle, func(_ *Writer, c *ResponseChunk) {
		chunks = append(chunks, *c)
	})

	headers := fake.NewFakeHeaderMap(map[string][]string{
		":status":          {"200"},
		"content-type":     {"application/json"},
		"content-encoding": {"br"},
	})
	f.OnResponseHeaders(headers, true) // endOfStream=true → 2 calls

	require.Len(t, chunks, 2)
	for _, c := range chunks {
		require.Equal(t, "application/json", c.ContentType)
		require.Equal(t, "br", c.ContentEncoding)
	}
}

// Buffered mode must strip content-length from response headers so downstream
// never sees a stale value if SetResponseBody replaces the body later.
func TestFilter_OnResponseHeaders_bufferedMode_removesContentLength(t *testing.T) {
	ctrl := gomock.NewController(t)
	handle := newMockHandle(ctrl)

	headers := fake.NewFakeHeaderMap(map[string][]string{
		":status":        {"200"},
		"content-length": {"99"},
	})
	f := newFilterWithResponseBuffered(handle, func(_ *Writer, _ *ResponseChunk) {})

	f.OnResponseHeaders(headers, false)

	require.Empty(t, headers.Headers["content-length"])
}

// ── OnResponseBody ────────────────────────────────────────────────────────────

func TestFilter_OnResponseBody_noHandler_returnsContinue(t *testing.T) {
	ctrl := gomock.NewController(t)
	handle := newMockHandle(ctrl)
	f := &filter{handle: handle, handler: noopHandler}

	status := f.OnResponseBody(fake.NewFakeBodyBuffer([]byte("data")), true)
	require.Equal(t, shared.BodyStatusContinue, status)
}

// Streaming mode: each chunk delivered to the handler.
func TestFilter_OnResponseBody_streaming_callsHandlerWithData(t *testing.T) {
	ctrl := gomock.NewController(t)
	handle := newMockHandle(ctrl)

	var got *ResponseChunk
	f := newFilterWithResponse(handle, func(_ *Writer, c *ResponseChunk) {
		if c.StatusCode == 0 {
			got = c
		}
	})

	f.OnResponseBody(fake.NewFakeBodyBuffer([]byte("resp body")), true)

	require.Equal(t, []byte("resp body"), got.Data)
	require.True(t, got.EndStream)
}

// Buffered mode: non-final chunk held.
func TestFilter_OnResponseBody_buffered_notEndOfStream_returnsStopAndBuffer(t *testing.T) {
	ctrl := gomock.NewController(t)
	handle := newMockHandle(ctrl)

	called := false
	f := newFilterWithResponseBuffered(handle, func(_ *Writer, c *ResponseChunk) {
		if c.StatusCode == 0 {
			called = true
		}
	})

	status := f.OnResponseBody(fake.NewFakeBodyBuffer([]byte("partial")), false)

	require.Equal(t, shared.BodyStatusStopAndBuffer, status)
	require.False(t, called)
}

// Buffered mode: final chunk delivers accumulated body from BufferedResponseBody.
func TestFilter_OnResponseBody_buffered_endOfStream_callsHandlerWithBufferedData(t *testing.T) {
	ctrl := gomock.NewController(t)
	handle := newMockHandle(ctrl)
	buf := fake.NewFakeBodyBuffer([]byte("full response"))
	handle.EXPECT().BufferedResponseBody().Return(buf).AnyTimes()

	var got *ResponseChunk
	f := newFilterWithResponseBuffered(handle, func(_ *Writer, c *ResponseChunk) {
		if c.StatusCode == 0 {
			got = c
		}
	})

	f.OnResponseBody(fake.NewFakeBodyBuffer(nil), true)

	require.Equal(t, []byte("full response"), got.Data)
	require.True(t, got.EndStream)
}

// Buffered mode with SetResponseBody: buffer drained, replacement appended,
// content-length updated.
func TestFilter_OnResponseBody_buffered_replacement_updatesBufferAndHeader(t *testing.T) {
	ctrl := gomock.NewController(t)
	handle := newMockHandle(ctrl)

	buf := fake.NewFakeBodyBuffer([]byte("original response"))
	respHeaders := fake.NewFakeHeaderMap(nil)
	handle.EXPECT().BufferedResponseBody().Return(buf).AnyTimes()
	handle.EXPECT().ResponseHeaders().Return(respHeaders).AnyTimes()

	replacement := []byte("new response")
	f := newFilterWithResponseBuffered(handle, func(w *Writer, c *ResponseChunk) {
		if c.StatusCode == 0 {
			w.SetResponseBody(replacement)
		}
	})

	f.OnResponseBody(fake.NewFakeBodyBuffer(nil), true)

	require.Equal(t, replacement, buf.Body)
	require.Equal(t, "12", respHeaders.Headers["content-length"][0])
}
