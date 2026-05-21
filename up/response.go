package up

import "github.com/envoyproxy/envoy/source/extensions/dynamic_modules/sdk/go/shared"

// ResponseChunk is passed to a ResponseHandlerFunc on every response event.
//
// On the response headers call, StatusCode is non-zero and Headers is set.
// On response body calls, StatusCode is 0 and Data holds the received bytes.
// A synthetic body call with Data: nil, EndStream: true is issued when the
// response has no body (204, HEAD, etc.) so body logic always fires once.
//
// ContentEncoding and ContentType are populated automatically from the
// response's Content-Encoding and Content-Type headers.
// Context is a per-stream slot shared across all callbacks on one stream.
type ResponseChunk struct {
	StatusCode      int
	Headers         shared.HeaderMap
	Data            []byte
	EndStream       bool
	ContentEncoding string
	ContentType     string
	Context         *any
}

// ResponseHandlerFunc is called for each response event on a stream.
type ResponseHandlerFunc func(w *Writer, chunk *ResponseChunk)
