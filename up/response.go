package up

import "github.com/envoyproxy/envoy/source/extensions/dynamic_modules/sdk/go/shared"

// ResponseChunk is passed to a ResponseHandlerFunc on every response event.
//
// On the response headers call, StatusCode is non-zero and Headers is set.
// On response body calls, StatusCode is 0 and Data holds the received bytes.
// Context is a per-stream slot; the same pointer is passed for every call on
// one stream, allowing state to be carried from headers to body callbacks.
type ResponseChunk struct {
	StatusCode int
	Headers    shared.HeaderMap
	Data       []byte
	EndStream  bool
	Context    *any
}

// ResponseHandlerFunc is called for each response event on a stream.
type ResponseHandlerFunc func(w *Writer, chunk *ResponseChunk)
