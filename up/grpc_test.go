package up

import (
	"encoding/binary"
	"testing"

	"github.com/envoyproxy/envoy/source/extensions/dynamic_modules/sdk/go/shared"
	"github.com/stretchr/testify/require"

	"github.com/dio/transit/up/testutil"
)

func TestEncodeGRPCFrame(t *testing.T) {
	msg := []byte("hello")
	frame := encodeGRPCFrame(msg)

	require.Len(t, frame, 10)
	require.Zero(t, frame[0])
	require.EqualValues(t, 5, binary.BigEndian.Uint32(frame[1:5]))
	require.Equal(t, []byte("hello"), frame[5:])
}

func TestEncodeGRPCFrame_empty(t *testing.T) {
	frame := encodeGRPCFrame(nil)
	require.Len(t, frame, 5)
	require.Zero(t, binary.BigEndian.Uint32(frame[1:5]))
}

func TestDecodeGRPCBody_roundtrip(t *testing.T) {
	msg := []byte("world")
	frame := encodeGRPCFrame(msg)
	chunks := []shared.UnsafeEnvoyBuffer{{Ptr: &frame[0], Len: uint64(len(frame))}}
	got := decodeGRPCBody(chunks)
	require.Equal(t, []byte("world"), got)
}

func TestDecodeGRPCBody_tooShort(t *testing.T) {
	data := []byte{0, 0, 0} // shorter than 5-byte header
	chunks := []shared.UnsafeEnvoyBuffer{{Ptr: &data[0], Len: uint64(len(data))}}
	require.Nil(t, decodeGRPCBody(chunks))
}

func TestDecodeGRPCBody_empty(t *testing.T) {
	require.Nil(t, decodeGRPCBody(nil))
}

func TestDecodeGRPCBody_compressedFrameRejected(t *testing.T) {
	frame := encodeGRPCFrame([]byte("compressed"))
	frame[0] = 1
	chunks := []shared.UnsafeEnvoyBuffer{{Ptr: &frame[0], Len: uint64(len(frame))}}

	require.Nil(t, decodeGRPCBody(chunks))
}

func TestDecodeGRPCBody_truncatedFrameRejected(t *testing.T) {
	frame := encodeGRPCFrame([]byte("truncated"))
	frame = frame[:len(frame)-1]
	chunks := []shared.UnsafeEnvoyBuffer{{Ptr: &frame[0], Len: uint64(len(frame))}}

	require.Nil(t, decodeGRPCBody(chunks))
}

func TestDecodeGRPCBody_multiChunk(t *testing.T) {
	msg := []byte("split")
	frame := encodeGRPCFrame(msg)
	// Split frame across two chunks: header in first, body in second.
	chunks := []shared.UnsafeEnvoyBuffer{
		{Ptr: &frame[0], Len: 5},
		{Ptr: &frame[5], Len: uint64(len(frame) - 5)},
	}
	got := decodeGRPCBody(chunks)
	require.Equal(t, []byte("split"), got)
}

func unsafeBufFromString(s string) shared.UnsafeEnvoyBuffer {
	b := []byte(s)
	return shared.UnsafeEnvoyBuffer{Ptr: &b[0], Len: uint64(len(b))}
}

func TestParseGRPCHeaders_ok(t *testing.T) {
	headers := [][2]shared.UnsafeEnvoyBuffer{
		{unsafeBufFromString(":status"), unsafeBufFromString("200")},
		{unsafeBufFromString("grpc-status"), unsafeBufFromString("0")},
		{unsafeBufFromString("grpc-message"), unsafeBufFromString("OK")},
	}
	status, msg := parseGRPCHeaders(headers)
	require.Zero(t, status)
	require.Equal(t, "OK", msg)
}

func TestParseGRPCHeaders_error(t *testing.T) {
	headers := [][2]shared.UnsafeEnvoyBuffer{
		{unsafeBufFromString("grpc-status"), unsafeBufFromString("7")},
		{unsafeBufFromString("grpc-message"), unsafeBufFromString("permission%20denied")},
	}
	status, msg := parseGRPCHeaders(headers)
	require.EqualValues(t, 7, status)
	require.Equal(t, "permission denied", msg)
}

func TestParseGRPCHeaders_absent(t *testing.T) {
	headers := [][2]shared.UnsafeEnvoyBuffer{
		{unsafeBufFromString(":status"), unsafeBufFromString("200")},
	}
	status, msg := parseGRPCHeaders(headers)
	require.Zero(t, status)
	require.Empty(t, msg)
}

func TestWriterGRPCCallout_wrapsHTTPStream(t *testing.T) {
	var gotCluster string
	var gotHeaders [][2]string
	var gotBody []byte
	var gotEndStream bool
	var gotTimeout uint64

	handle := testutil.NewFilterHandle(
		testutil.WithHTTPStreamFunc(func(cluster string, headers [][2]string, body []byte, endOfStream bool, timeoutMs uint64, cb shared.HttpStreamCallback) (shared.HttpCalloutInitResult, uint64) {
			gotCluster = cluster
			gotHeaders = headers
			gotBody = body
			gotEndStream = endOfStream
			gotTimeout = timeoutMs

			respFrame := encodeGRPCFrame([]byte("response"))
			cb.OnHttpStreamHeaders(1, [][2]shared.UnsafeEnvoyBuffer{
				{unsafeBufFromString(":status"), unsafeBufFromString("200")},
			}, false)
			cb.OnHttpStreamData(1, []shared.UnsafeEnvoyBuffer{{Ptr: &respFrame[0], Len: uint64(len(respFrame))}}, false)
			cb.OnHttpStreamTrailers(1, [][2]shared.UnsafeEnvoyBuffer{
				{unsafeBufFromString("grpc-status"), unsafeBufFromString("0")},
			})
			cb.OnHttpStreamComplete(1)
			return shared.HttpCalloutInitSuccess, 1
		}),
	)
	f := &filter{
		handle: handle,
		handler: func(w *Writer, _ *Request) {
			init, err := w.GRPCCallout(GRPCCalloutRequest{
				Cluster:       "rls",
				Authority:     "rls.local",
				Method:        "/svc/Method",
				Message:       []byte("request"),
				TimeoutMillis: 25,
			}, func(resp GRPCCalloutResponse) {
				require.Equal(t, HTTPCalloutSuccess, resp.Result)
				require.Zero(t, resp.GRPCStatus)
				require.Equal(t, []byte("response"), resp.Body)
			})
			require.NoError(t, err)
			require.Equal(t, HTTPCalloutInitSuccess, init)
		},
	}

	status := f.OnRequestHeaders(handle.RequestHeaders(), true)
	require.Equal(t, shared.HeadersStatusContinue, status)
	require.Equal(t, "rls", gotCluster)
	require.Equal(t, uint64(25), gotTimeout)
	require.True(t, gotEndStream)
	require.Equal(t, [][2]string{
		{":method", "POST"},
		{":path", "/svc/Method"},
		{":scheme", "http"},
		{"host", "rls.local"},
		{"content-type", "application/grpc+proto"},
		{"te", "trailers"},
	}, gotHeaders)
	require.Equal(t, []byte("request"), decodeGRPCBody([]shared.UnsafeEnvoyBuffer{{Ptr: &gotBody[0], Len: uint64(len(gotBody))}}))
}
