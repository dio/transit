// Package codec handles Content-Encoding compression for HTTP body inspection.
//
// Typical usage in a response body handler:
//
//	decoded, err := codec.Decode(chunk.ContentEncoding, chunk.Data)
//
// To prevent upstream from sending compressed responses, call NegotiateIdentity
// from the request headers handler instead:
//
//	func onReq(w *up.Writer, r *up.Request) { codec.NegotiateIdentity(w) }
package codec

import (
	"bytes"
	"compress/gzip"
	"compress/zlib"
	"fmt"
	"io"
	"strings"

	"github.com/andybalholm/brotli"
	"github.com/klauspost/compress/zstd"
)

// Decode decompresses data according to the Content-Encoding header value.
// Supports "gzip", "deflate", "zstd", "br", "identity", and "".
// Returns data unchanged for identity or empty encoding.
func Decode(encoding string, data []byte) ([]byte, error) {
	switch strings.ToLower(strings.TrimSpace(encoding)) {
	case "", "identity":
		return data, nil
	case "gzip":
		return decodeGzip(data)
	case "deflate":
		return decodeDeflate(data)
	case "br":
		return decodeBrotli(data)
	case "zstd":
		return decodeZstd(data)
	default:
		return nil, fmt.Errorf("codec: unsupported Content-Encoding %q", encoding)
	}
}

// Encode compresses data with the given encoding.
// Supports "gzip", "deflate", "zstd", "br", "identity", and "".
func Encode(encoding string, data []byte) ([]byte, error) {
	switch strings.ToLower(strings.TrimSpace(encoding)) {
	case "", "identity":
		return data, nil
	case "gzip":
		return encodeGzip(data)
	case "deflate":
		return encodeDeflate(data)
	case "br":
		return encodeBrotli(data)
	case "zstd":
		return encodeZstd(data)
	default:
		return nil, fmt.Errorf("codec: unsupported encoding %q", encoding)
	}
}

// RequestHeaderSetter is satisfied by *up.Writer after SetRequestHeader is added.
type RequestHeaderSetter interface {
	SetRequestHeader(name, value string)
}

// NegotiateIdentity sets Accept-Encoding: identity on the outgoing request so
// upstream responds with uncompressed bodies. Call from OnRequestHeaders.
// This is the preferred strategy; use Decode as a fallback for upstreams that
// ignore Accept-Encoding (some CDNs always compress).
func NegotiateIdentity(h RequestHeaderSetter) {
	h.SetRequestHeader("accept-encoding", "identity")
}

func decodeGzip(data []byte) ([]byte, error) {
	r, err := gzip.NewReader(bytes.NewReader(data))
	if err != nil {
		return nil, fmt.Errorf("codec: gzip reader: %w", err)
	}
	defer r.Close()
	out, err := io.ReadAll(r)
	if err != nil {
		return nil, fmt.Errorf("codec: gzip read: %w", err)
	}
	return out, nil
}

func encodeGzip(data []byte) ([]byte, error) {
	var buf bytes.Buffer
	w := gzip.NewWriter(&buf)
	if _, err := w.Write(data); err != nil {
		return nil, fmt.Errorf("codec: gzip write: %w", err)
	}
	if err := w.Close(); err != nil {
		return nil, fmt.Errorf("codec: gzip close: %w", err)
	}
	return buf.Bytes(), nil
}

func decodeDeflate(data []byte) ([]byte, error) {
	r, err := zlib.NewReader(bytes.NewReader(data))
	if err != nil {
		return nil, fmt.Errorf("codec: deflate reader: %w", err)
	}
	defer r.Close()
	out, err := io.ReadAll(r)
	if err != nil {
		return nil, fmt.Errorf("codec: deflate read: %w", err)
	}
	return out, nil
}

func encodeDeflate(data []byte) ([]byte, error) {
	var buf bytes.Buffer
	w := zlib.NewWriter(&buf)
	if _, err := w.Write(data); err != nil {
		return nil, fmt.Errorf("codec: deflate write: %w", err)
	}
	if err := w.Close(); err != nil {
		return nil, fmt.Errorf("codec: deflate close: %w", err)
	}
	return buf.Bytes(), nil
}

func decodeBrotli(data []byte) ([]byte, error) {
	out, err := io.ReadAll(brotli.NewReader(bytes.NewReader(data)))
	if err != nil {
		return nil, fmt.Errorf("codec: brotli read: %w", err)
	}
	return out, nil
}

func encodeBrotli(data []byte) ([]byte, error) {
	var buf bytes.Buffer
	w := brotli.NewWriter(&buf)
	if _, err := w.Write(data); err != nil {
		return nil, fmt.Errorf("codec: brotli write: %w", err)
	}
	if err := w.Close(); err != nil {
		return nil, fmt.Errorf("codec: brotli close: %w", err)
	}
	return buf.Bytes(), nil
}

func decodeZstd(data []byte) ([]byte, error) {
	d, err := zstd.NewReader(bytes.NewReader(data))
	if err != nil {
		return nil, fmt.Errorf("codec: zstd reader: %w", err)
	}
	defer d.Close()
	out, err := io.ReadAll(d)
	if err != nil {
		return nil, fmt.Errorf("codec: zstd read: %w", err)
	}
	return out, nil
}

func encodeZstd(data []byte) ([]byte, error) {
	var buf bytes.Buffer
	w, err := zstd.NewWriter(&buf)
	if err != nil {
		return nil, fmt.Errorf("codec: zstd writer: %w", err)
	}
	if _, err := w.Write(data); err != nil {
		return nil, fmt.Errorf("codec: zstd write: %w", err)
	}
	if err := w.Close(); err != nil {
		return nil, fmt.Errorf("codec: zstd close: %w", err)
	}
	return buf.Bytes(), nil
}
