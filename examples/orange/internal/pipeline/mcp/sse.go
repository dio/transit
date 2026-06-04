package mcp

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
)

var (
	sseEventPrefix = []byte("event:")
	sseIDPrefix    = []byte("id:")
	sseDataPrefix  = []byte("data:")
	sseLF          = []byte{'\n'}
	sseCR          = []byte{'\r'}
	sseCRLF        = []byte{'\r', '\n'}
	sseLFLF        = []byte{'\n', '\n'}
	utf8BOM        = []byte{0xef, 0xbb, 0xbf}
)

type jsonrpcMessage json.RawMessage

type sseEvent struct {
	Event    string
	ID       string
	Messages []jsonrpcMessage
}

type sseParser struct {
	r       io.Reader
	readBuf [4096]byte
	buf     []byte
}

func newSSEParser(r io.Reader) *sseParser {
	return &sseParser{r: r}
}

func tryDecodeJSONRPCMessage(body []byte) (jsonrpcMessage, bool) {
	body = bytes.TrimSpace(bytes.TrimPrefix(bytes.TrimSpace(body), utf8BOM))
	if !json.Valid(body) {
		return nil, false
	}
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(body, &obj); err != nil {
		return nil, false
	}
	if _, ok := obj["jsonrpc"]; !ok {
		if _, hasMethod := obj["method"]; !hasMethod {
			if _, hasResult := obj["result"]; !hasResult {
				if _, hasError := obj["error"]; !hasError {
					return nil, false
				}
			}
		}
	}
	out := make([]byte, len(body))
	copy(out, body)
	return jsonrpcMessage(out), true
}

func (p *sseParser) next() (*sseEvent, error) {
	for {
		ev, ok, err := p.extractEvent()
		if err != nil || ok {
			return ev, err
		}

		n, err := p.r.Read(p.readBuf[:])
		if n > 0 {
			p.buf = append(p.buf, normalizeNewlines(p.readBuf[:n])...)
			continue
		}
		if err != nil {
			if errors.Is(err, io.EOF) && len(p.buf) > 0 {
				ev, parseErr := parseSSEEvent(p.buf)
				p.buf = nil
				return ev, errors.Join(err, parseErr)
			}
			return nil, err
		}
	}
}

func (p *sseParser) extractEvent() (*sseEvent, bool, error) {
	idx := bytes.Index(p.buf, sseLFLF)
	if idx < 0 {
		return nil, false, nil
	}
	chunk := p.buf[:idx]
	p.buf = p.buf[idx+len(sseLFLF):]
	ev, err := parseSSEEvent(chunk)
	return ev, true, err
}

func parseSSEEvent(chunk []byte) (*sseEvent, error) {
	ev := &sseEvent{}
	for line := range bytes.SplitSeq(chunk, sseLF) {
		line = bytes.TrimPrefix(line, utf8BOM)
		switch {
		case bytes.HasPrefix(line, sseEventPrefix):
			ev.Event = string(bytes.TrimSpace(line[len(sseEventPrefix):]))
		case bytes.HasPrefix(line, sseIDPrefix):
			ev.ID = string(bytes.TrimSpace(line[len(sseIDPrefix):]))
		case bytes.HasPrefix(line, sseDataPrefix):
			data := bytes.TrimSpace(line[len(sseDataPrefix):])
			msg, ok := tryDecodeJSONRPCMessage(data)
			if !ok {
				return nil, fmt.Errorf("decode json-rpc from SSE data")
			}
			ev.Messages = append(ev.Messages, msg)
		}
	}
	return ev, nil
}

func (e *sseEvent) writeAndMaybeFlush(w io.Writer) {
	if e.Event != "" {
		_, _ = fmt.Fprintf(w, "event: %s\n", e.Event)
	}
	if e.ID != "" {
		_, _ = fmt.Fprintf(w, "id: %s\n", e.ID)
	}
	for _, msg := range e.Messages {
		_, _ = w.Write([]byte("data: "))
		_, _ = w.Write(msg)
		_, _ = w.Write(sseLF)
	}
	_, _ = w.Write(sseLF)
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}
}

func normalizeNewlines(b []byte) []byte {
	b = bytes.ReplaceAll(b, sseCRLF, sseLF)
	return bytes.ReplaceAll(b, sseCR, sseLF)
}
