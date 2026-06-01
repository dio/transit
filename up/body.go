package up

// BodyChunk is passed to a RequestBodyHandlerFunc on every request body event.
//
// In streaming mode the handler is called once per chunk as data arrives.
// In buffered mode ([WithMutableBody]) the handler is called exactly
// once with the full accumulated body when EndStream is true.
//
// The handler is also called synthetically with Data: nil, EndStream: true
// when the request has no body (GET, DELETE, HEAD, etc.) so body-dependent
// logic always has a single completion point.
//
// ContentEncoding and ContentType are populated automatically from the
// request's Content-Encoding and Content-Type headers.
type BodyChunk struct {
	Data            []byte
	EndStream       bool
	ContentEncoding string
	ContentType     string
	Context         *any // same per-stream slot as ResponseChunk.Context
}

// RequestBodyHandlerFunc is called for each request body event.
type RequestBodyHandlerFunc func(w *Writer, chunk *BodyChunk)
