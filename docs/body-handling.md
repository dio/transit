# Body handling

## Architecture

Two layers:

```
┌──────────────────────────────────────────────────────────────┐
│  up/compress  — content-encoding decode/encode               │
│  gzip, deflate, br (brotli), zstd; Accept-Encoding helpers    │
├──────────────────────────────────────────────────────────────┤
│  up  — body mechanics                                        │
│  BodyChunk, RequestBodyHandlerFunc, buffered/streaming,      │
│  endOfStream invariant, header mutation, SetRequestBody      │
└──────────────────────────────────────────────────────────────┘
```

Content-Type parsing is out of scope. `BodyChunk` carries `ContentType` so
handlers can dispatch to their own parsers (JSON, protobuf, form, etc.)
without any registry in transit.

---

## Layer 1: body mechanics (`up`)

### The central invariant

`OnRequestHeaders` and `OnResponseHeaders` receive `endOfStream bool`. When
`true`, Envoy will **never** call `OnRequestBody` / `OnResponseBody` (GET,
DELETE, HEAD, 204, 304, …).

Both `RequestBodyHandlerFunc` and `ResponseHandlerFunc` are guaranteed to be
called exactly once with `EndStream: true`, whether or not a body exists. For
bodyless streams the call is issued synthetically from the headers callback
with `Data: nil`.

### Types

```go
type BodyChunk struct {
    Data            []byte
    EndStream       bool
    ContentEncoding string // from Content-Encoding header; "" = identity
    ContentType     string // from Content-Type header; "" = unknown
    Context         *any   // same per-stream slot as ResponseChunk.Context
}

type RequestBodyHandlerFunc func(w *Writer, chunk *BodyChunk)
```

`ResponseChunk` carries `ContentEncoding` and `ContentType` (populated from
response headers) alongside the existing `Data`, `EndStream`, and `Context`
fields — no separate response-body type is needed.

### Operating modes

| Mode | Registration | Body handler called |
|---|---|---|
| **Streaming** | `RegisterWithBody` | Once per arriving chunk |
| **Buffered** | `RegisterWithMutableBody` | Once, with the full accumulated body |

Mode is fixed at registration time, not per-request.

### Registration

```go
// RegisterWithBody — streaming request body inspection.
// The body handler fires once per chunk; use for logging, forwarding to a
// sink, or any read-only inspection that does not need the full body first.
func RegisterWithBody(name string, h HandlerFunc, rb RequestBodyHandlerFunc, r ResponseHandlerFunc)

// RegisterWithMutableBody — buffered request and response body mutation.
// The body handler fires once with the full body. Use Writer.SetRequestBody
// or Writer.SetResponseBody to replace the body content.
func RegisterWithMutableBody(name string, h HandlerFunc, rb RequestBodyHandlerFunc, r ResponseHandlerFunc)
```

### `filter` struct (relevant fields)

```go
requestBodyHandler      RequestBodyHandlerFunc
bufferBody              bool   // true for RegisterWithMutableBody
requestContentType      string // captured in OnRequestHeaders
requestContentEncoding  string // captured in OnRequestHeaders
responseContentType     string // captured in OnResponseHeaders
responseContentEncoding string // captured in OnResponseHeaders
```

### Request path

**`OnRequestHeaders`**: captures `Content-Type` and `Content-Encoding` when a
body handler is registered. When `endOfStream` is true (bodyless request),
fires a synthetic body call immediately (`Data: nil, EndStream: true`) and
returns `Continue`. In buffered mode with a body coming, strips
`content-length` and `transfer-encoding` from the headers before forwarding —
the correct `content-length` is written back in `OnRequestBody` after any body
replacement.

> `HeadersStatusStopAllAndBuffer` is intentionally **not** used. Returning it
> from `OnRequestHeaders` freezes the filter chain permanently — Envoy buffers
> body data but never calls `OnRequestBody` because the SDK has no
> asynchronous resume path.

**`OnRequestBody`**: in buffered mode, returns `StopAndBuffer` until
`endOfStream`, then reads the full body from `BufferedRequestBody`. Calls the
body handler. If `SetRequestBody` was called, drains the buffer, appends the
replacement, and sets the correct `content-length`.

### Response path

**`OnResponseHeaders`**: captures `Content-Type` and `Content-Encoding`. In
buffered mode, strips `content-length` (the correct value is written back in
`OnResponseBody` after any replacement). Then calls the response handler with
a `ResponseChunk` carrying the status code and headers. When `endOfStream` is
true (bodyless response: 204, HEAD, etc.), fires a second synthetic call
(`StatusCode: 0, Data: nil, EndStream: true`) so body logic always has a
single completion point.

**`OnResponseBody`**: mirrors the request path — buffers until `endOfStream`
in buffered mode, calls the response handler, and rewrites `content-length`
if `SetResponseBody` was called.

### `Writer` header methods

```go
func (w *Writer) SetRequestHeader(name, value string)
func (w *Writer) AddRequestHeader(name, value string)
func (w *Writer) RemoveRequestHeader(name string)
func (w *Writer) SetResponseHeader(name, value string)
func (w *Writer) AddResponseHeader(name, value string)
func (w *Writer) RemoveResponseHeader(name string)
```

### `Writer` body replacement methods

```go
// SetRequestBody replaces the request body. Buffered mode only; no-op otherwise.
func (w *Writer) SetRequestBody(data []byte)

// SetResponseBody replaces the response body. Buffered mode only; no-op otherwise.
func (w *Writer) SetResponseBody(data []byte)
```

---

## Layer 2: content encoding (`up/compress`)

### API

```go
// Decode decompresses data according to Content-Encoding.
// Supports: "gzip", "deflate", "zstd", "br", "identity", "".
func Decode(encoding string, data []byte) ([]byte, error)

// Encode compresses data with the given encoding.
func Encode(encoding string, data []byte) ([]byte, error)

// NegotiateIdentity rewrites Accept-Encoding to "identity" on the outgoing
// request so the upstream is asked not to compress. Call from the request
// handler; if the upstream ignores the hint, use Decode in the body handler.
func NegotiateIdentity(h RequestHeaderSetter)
```

`RequestHeaderSetter` is satisfied by `*up.Writer`.

### Encodings

| Encoding | Backend |
|---|---|
| `gzip` | stdlib `compress/gzip` |
| `deflate` | stdlib `compress/zlib` |
| `zstd` | `github.com/klauspost/compress/zstd` |
| `br` | `github.com/andybalholm/brotli` |
| `identity` / `""` | passthrough |

### Typical usage

```go
func onReqHeaders(w *up.Writer, r *up.Request) {
    compress.NegotiateIdentity(w)
}

func onRespBody(w *up.Writer, chunk *up.ResponseChunk) {
    if !chunk.EndStream {
        return
    }
    decoded, err := compress.Decode(chunk.ContentEncoding, chunk.Data)
    if err != nil { /* handle */ }
    switch {
    case strings.HasPrefix(chunk.ContentType, "application/json"):
        // JSON logic
    case chunk.ContentType == "application/x-protobuf":
        // proto logic
    }
}
```
