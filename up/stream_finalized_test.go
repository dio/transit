package up

import (
	"testing"

	"github.com/envoyproxy/envoy/source/extensions/dynamic_modules/sdk/go/shared"
	"github.com/stretchr/testify/require"

	"github.com/dio/transit/up/testutil"
)

type fakeFinalizedAccessLogHandle struct {
	requestID      []byte
	fallbackID     []byte
	localReplyBody []byte
	localReplyOK   bool
}

func newFakeFinalizedAccessLogHandle(requestID string) *fakeFinalizedAccessLogHandle {
	return &fakeFinalizedAccessLogHandle{requestID: []byte(requestID)}
}

func bufferFromBytes(b []byte) Buffer {
	if len(b) == 0 {
		return Buffer{}
	}
	return newBuffer(shared.UnsafeEnvoyBuffer{Ptr: &b[0], Len: uint64(len(b))})
}

func (h *fakeFinalizedAccessLogHandle) GetTimingInfo() TimingInfo { return TimingInfo{} }
func (h *fakeFinalizedAccessLogHandle) GetBytesInfo() BytesInfo   { return BytesInfo{} }
func (h *fakeFinalizedAccessLogHandle) GetResponseFlags() uint64  { return 0 }
func (h *fakeFinalizedAccessLogHandle) GetResponseCode() uint32   { return 401 }
func (h *fakeFinalizedAccessLogHandle) GetAttributeString(_ AttributeID) (Buffer, bool) {
	return Buffer{}, false
}
func (h *fakeFinalizedAccessLogHandle) GetAttributeInt(_ AttributeID) (int64, bool) {
	return 0, false
}
func (h *fakeFinalizedAccessLogHandle) GetAttributeBool(_ AttributeID) (bool, bool) {
	return false, false
}
func (h *fakeFinalizedAccessLogHandle) GetHeader(headerType HttpHeaderType, key string) (Buffer, bool) {
	if headerType == HttpHeaderTypeRequest && key == "x-request-id" && len(h.requestID) > 0 {
		return bufferFromBytes(h.requestID), true
	}
	if headerType == HttpHeaderTypeRequest && key == finalizedFallbackKeyHeader && len(h.fallbackID) > 0 {
		return bufferFromBytes(h.fallbackID), true
	}
	return Buffer{}, false
}
func (h *fakeFinalizedAccessLogHandle) GetWorkerIndex() uint32 { return 0 }
func (h *fakeFinalizedAccessLogHandle) GetTraceID() (Buffer, bool) {
	return Buffer{}, false
}
func (h *fakeFinalizedAccessLogHandle) GetSpanID() (Buffer, bool) {
	return Buffer{}, false
}
func (h *fakeFinalizedAccessLogHandle) IsTraceSampled() bool { return false }
func (h *fakeFinalizedAccessLogHandle) GetLocalReplyBody() (Buffer, bool) {
	if h.localReplyOK {
		return bufferFromBytes(h.localReplyBody), true
	}
	return Buffer{}, false
}
func (h *fakeFinalizedAccessLogHandle) GetUpstreamPoolReadyDurationNs() int64 { return -1 }
func (h *fakeFinalizedAccessLogHandle) GetUpstreamRequestAttemptCount() uint32 {
	return 0
}
func (h *fakeFinalizedAccessLogHandle) Log(_ LogLevel, _ string, _ ...any) {}

func TestFinalizedLogger_usesCapturedLocalReplyBodyFallback(t *testing.T) {
	const (
		name      = "finalized-local-reply-fallback"
		requestID = "rid-fallback"
		body      = `{"error":"blocked"}`
	)
	key := streamFinalizedKey(name, requestID)
	capturedBody := body

	var got FinalizedInfo
	putFinalizedEntry(key, &streamFinalizedEntry{
		fn:             func(_ *any, info FinalizedInfo) { got = info },
		ctx:            new(any),
		localReplyBody: &capturedBody,
	})
	t.Cleanup(func() { _, _ = takeFinalizedEntry(key) })

	(&finalizedLogger{name: name}).OnLog(newFakeFinalizedAccessLogHandle(requestID), AccessLogTypeDownstreamEnd)

	require.Equal(t, body, got.LocalReplyBody)
}

func TestFinalizedLogger_prefersEnvoyLocalReplyBody(t *testing.T) {
	const (
		name      = "finalized-local-reply-envoy"
		requestID = "rid-envoy"
	)
	key := streamFinalizedKey(name, requestID)
	capturedBody := "sdk-body"

	var got FinalizedInfo
	putFinalizedEntry(key, &streamFinalizedEntry{
		fn:             func(_ *any, info FinalizedInfo) { got = info },
		ctx:            new(any),
		localReplyBody: &capturedBody,
	})
	t.Cleanup(func() { _, _ = takeFinalizedEntry(key) })

	h := newFakeFinalizedAccessLogHandle(requestID)
	h.localReplyOK = true
	h.localReplyBody = []byte("envoy-body")
	(&finalizedLogger{name: name}).OnLog(h, AccessLogTypeDownstreamEnd)

	require.Equal(t, "envoy-body", got.LocalReplyBody)
}

func TestWriter_SendLocalResponse_capturesLocalReplyBodyForFinalizedFallback(t *testing.T) {
	f := &filter{handle: testutil.NewFilterHandle()}
	w := &Writer{f: f}

	w.SendLocalResponse(401, []byte("first"))
	w.SendLocalResponse(200, []byte("second"))

	require.Equal(t, "first", f.localReplyBody)
}

func TestFinalizedFallbackHeaderSurvivesQueuedLocalReply(t *testing.T) {
	const name = "finalized-local-reply-no-request-id"
	h := testutil.NewFilterHandle()
	finalizedCalled := false

	f := &filter{
		name:   name,
		handle: h,
		handler: func(w *Writer, _ *Request) {
			w.SetStreamObject("obj", "value")
			w.SendLocalResponse(403, []byte("blocked"))
		},
		onStreamFinalized: func(_ *any, _ FinalizedInfo) {
			finalizedCalled = true
		},
	}

	status := f.OnRequestHeaders(h.RequestHeaders(), true)
	require.Equal(t, shared.HeadersStatusStop, status)

	fallback := h.RequestHeaders().GetOne(finalizedFallbackKeyHeader)
	require.NotZero(t, fallback.Len)
	fallbackID := fallback.ToString()

	nonce := f.streamObjectNonce
	require.NotEmpty(t, nonce)
	_, ok := lookupBag(nonce)
	require.True(t, ok)

	f.OnStreamComplete()
	_, ok = lookupBag(nonce)
	require.True(t, ok, "WithOnStreamFinalized defers Primitive A drain to finalizedLogger")

	(&finalizedLogger{name: name}).OnLog(&fakeFinalizedAccessLogHandle{
		fallbackID: []byte(fallbackID),
	}, AccessLogTypeDownstreamEnd)

	require.True(t, finalizedCalled)
	_, ok = lookupBag(nonce)
	require.False(t, ok)
}
