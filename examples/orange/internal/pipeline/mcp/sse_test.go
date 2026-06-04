package mcp

import (
	"bytes"
	"errors"
	"io"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestTryDecodeJSONRPCMessage(t *testing.T) {
	raw := []byte(`{"jsonrpc":"2.0","id":1,"result":{"tools":[]}}`)

	msg, ok := tryDecodeJSONRPCMessage(raw)
	require.True(t, ok)
	assert.JSONEq(t, string(raw), string(msg))

	msg, ok = tryDecodeJSONRPCMessage(append(utf8BOM, raw...))
	require.True(t, ok)
	assert.JSONEq(t, string(raw), string(msg))

	_, ok = tryDecodeJSONRPCMessage([]byte("data: " + string(raw) + "\n\n"))
	assert.False(t, ok)

	_, ok = tryDecodeJSONRPCMessage([]byte(`{"not":"json-rpc"}`))
	assert.False(t, ok)
}

func TestSSEParserSingleEventNewlineVariants(t *testing.T) {
	msg := []byte(`{"jsonrpc":"2.0","method":"ping","id":"1"}`)
	for _, raw := range [][]byte{
		[]byte("event: message\nid: 42\ndata: " + string(msg) + "\n\n"),
		[]byte("event: message\rid: 42\rdata: " + string(msg) + "\r\r"),
		[]byte("event: message\r\nid: 42\r\ndata: " + string(msg) + "\r\n\r\n"),
		[]byte("event:message\nid:42\ndata:" + string(msg) + "\n\n"),
	} {
		p := newSSEParser(bytes.NewReader(raw))
		ev, err := p.next()
		require.NoError(t, err)
		require.Equal(t, "message", ev.Event)
		require.Equal(t, "42", ev.ID)
		require.Len(t, ev.Messages, 1)
		assert.JSONEq(t, string(msg), string(ev.Messages[0]))
	}
}

func TestSSEParserMultipleEvents(t *testing.T) {
	msg1 := []byte(`{"jsonrpc":"2.0","method":"one","id":"1"}`)
	msg2 := []byte(`{"jsonrpc":"2.0","method":"two","id":"2"}`)
	p := newSSEParser(bytes.NewReader([]byte(
		"event: one\ndata: " + string(msg1) + "\n\n" +
			"event: two\ndata: " + string(msg2) + "\n\n",
	)))

	ev1, err := p.next()
	require.NoError(t, err)
	ev2, err := p.next()
	require.NoError(t, err)

	assert.Equal(t, "one", ev1.Event)
	assert.Equal(t, "two", ev2.Event)
}

func TestSSEParserPartialReads(t *testing.T) {
	msg := []byte(`{"jsonrpc":"2.0","method":"ping","id":"1"}`)
	raw := []byte("event: message\nid: 99\ndata: " + string(msg) + "\n\n")
	p := newSSEParser(&partialReader{chunks: [][]byte{raw[:5], raw[5:17], raw[17:31], raw[31:]}})

	ev, err := p.next()
	require.NoError(t, err)
	assert.Equal(t, "message", ev.Event)
	assert.Equal(t, "99", ev.ID)
	require.Len(t, ev.Messages, 1)
}

func TestSSEParserBOMAndIncompleteEvent(t *testing.T) {
	raw := append(append([]byte{}, utf8BOM...), []byte("event: message\ndata: {\"jsonrpc\":\"2.0\",\"method\":\"ping\"}")...)
	p := newSSEParser(bytes.NewReader(raw))

	ev, err := p.next()
	require.ErrorIs(t, err, io.EOF)
	require.NotNil(t, ev)
	assert.Equal(t, "message", ev.Event)
	require.Len(t, ev.Messages, 1)
}

func TestSSEParserInvalidJSONRPC(t *testing.T) {
	p := newSSEParser(bytes.NewReader([]byte("data: {invalid json}\n\n")))
	ev, err := p.next()
	require.Nil(t, ev)
	require.Error(t, err)
}

func TestSSEParserEndOfStream(t *testing.T) {
	msg := []byte(`{"jsonrpc":"2.0","method":"last"}`)
	p := newSSEParser(bytes.NewReader([]byte("data: " + string(msg) + "\n\n")))

	ev, err := p.next()
	require.NoError(t, err)
	require.NotNil(t, ev)
	_, err = p.next()
	require.True(t, errors.Is(err, io.EOF), "got %v", err)
}

func TestSSEEventWriteAndFlush(t *testing.T) {
	ev := &sseEvent{
		Event:    "custom",
		ID:       "7",
		Messages: []jsonrpcMessage{jsonrpcMessage(`{"jsonrpc":"2.0","method":"ping"}`)},
	}
	rr := httptest.NewRecorder()
	ev.writeAndMaybeFlush(rr)

	assert.Contains(t, rr.Body.String(), "event: custom\n")
	assert.Contains(t, rr.Body.String(), "id: 7\n")
	assert.Contains(t, rr.Body.String(), "data: {\"jsonrpc\":\"2.0\",\"method\":\"ping\"}\n\n")
}

type partialReader struct {
	chunks [][]byte
	idx    int
}

func (r *partialReader) Read(p []byte) (int, error) {
	if r.idx >= len(r.chunks) {
		return 0, io.EOF
	}
	n := copy(p, r.chunks[r.idx])
	r.idx++
	return n, nil
}
