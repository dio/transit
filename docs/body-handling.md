# Body handling

## Architecture

Two separate layers:

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

Content-Type parsing is out of scope for the library. `BodyChunk` carries
`ContentType` so user-space handlers can dispatch to their own parsers
(JSON, protobuf, form, etc.) without any registry in transit.

---

## Layer 1: body mechanics (`up`)

### The central invariant

`OnRequestHeaders` and `OnResponseHeaders` receive `endOfStream bool`.
When `true`, Envoy will **never** call `OnRequestBody` / `OnResponseBody`
(GET, DELETE, HEAD, 204, 304, ...).

> The body handler is always called exactly once with `EndStream: true`,
> whether or not a body exists. When there is no body it is invoked
> synthetically from the headers callback with `Data: nil`.

This rule applies to both the request and response paths.

### New types

```go
type BodyChunk struct {
    Data             []byte
    EndStream        bool
    ContentEncoding  string  // from Content-Encoding header; "" = identity
    ContentType      string  // from Content-Type header; "" = unknown
    Context          *any    // same per-stream slot as ResponseChunk.Context
}

type RequestBodyHandlerFunc func(w *Writer, chunk *BodyChunk)
```

`ResponseChunk` is extended with `ContentEncoding` and `ContentType` fields
(populated automatically by the filter from the response headers call).
No new type is needed for response body — `ResponseChunk` already carries `Data`
and `EndStream`.

### Two operating modes

| Mode | Use case | `BodyStatus` returned | Handler called |
|---|---|---|---|
| **Streaming** | Inspection, forwarding to external sink | `Continue` | Once per chunk |
| **Buffered** | Full-body mutation, content-based routing | `StopAndBuffer` until EOS, then `Continue` | Once, with full body |

Mode is chosen at registration time (not per-request).

### Registration functions

```go
// RegisterWithBody adds streaming body handling.
func RegisterWithBody(name string, h HandlerFunc, rb RequestBodyHandlerFunc, r ResponseHandlerFunc)

// RegisterWithMutableBody adds buffered body handling (full body before handler fires).
func RegisterWithMutableBody(name string, h HandlerFunc, rb RequestBodyHandlerFunc, r ResponseHandlerFunc)
```

`RegisterWithResponse` (existing) stays as-is; response body in that form is
streaming. `RegisterWithMutableResponse` can be added when response mutation
is needed.

### `filter` struct additions

```
requestBodyHandler   RequestBodyHandlerFunc
bufferBody           bool   // set by RegisterWithMutableBody
requestContentType   string // captured in OnRequestHeaders
requestContentEnc    string // captured in OnRequestHeaders
```

### `OnRequestHeaders` change

```
existing: handler call, stopped check
NEW: capture Content-Type and Content-Encoding from headers
NEW: if endOfStream && requestBodyHandler != nil:
         call requestBodyHandler(w, &BodyChunk{Data: nil, EndStream: true, ...})
         return Continue   ← no body coming, nothing to hold
NEW: if bufferBody && requestBodyHandler != nil && !endOfStream:
         headers.Remove("content-length")    ← stale once body is replaced
         headers.Remove("transfer-encoding") ← ditto
         return Continue   ← headers forwarded; body buffered in OnRequestBody
```

`StopAllAndBuffer` is NOT used here. Envoy's `StopAllAndBuffer` from
`OnRequestHeaders` prevents `OnRequestBody` from ever being called — the filter
chain is frozen with no mechanism to resume it. Instead, `content-length` and
`transfer-encoding` are stripped from the request headers before they are
forwarded. The correct value is re-written after body replacement in
`OnRequestBody`.

### `OnRequestBody` (new)

```
if bufferBody && !endOfStream → return StopAndBuffer
data:
  buffered: BufferedRequestBody().GetChunks()
  streaming: body.GetChunks()
call requestBodyHandler(w, &BodyChunk{...})
if bufferBody && w.hasRequestBodyReplacement:
    BufferedRequestBody().Drain(size); Append(replacement)
    RequestHeaders().Remove("transfer-encoding")
    RequestHeaders().Set("content-length", len(replacement))
return Continue
```

### `OnResponseHeaders` fix

After calling the response handler with the headers chunk:
```
if bufferBody:
    headers.Remove("content-length")  ← stale once body is replaced
NEW: if endOfStream:
    call responseHandler(w, &ResponseChunk{EndStream: true, ...})  ← synthetic
return Continue  ← always; StopAllAndBuffer is NOT used (same reason as request side)
```

### `OnResponseBody` change

Same buffered/streaming split and Content-Length update as request side.
Captures `ContentEncoding` and `ContentType` in `OnResponseHeaders` for use
in body chunks.

### New `Writer` header mutation methods

`HeaderMap` already has `Set`/`Add`/`Remove`. Expose them on `Writer`:

```go
func (w *Writer) SetRequestHeader(name, value string)
func (w *Writer) AddRequestHeader(name, value string)
func (w *Writer) RemoveRequestHeader(name string)
func (w *Writer) SetResponseHeader(name, value string)
func (w *Writer) AddResponseHeader(name, value string)
func (w *Writer) RemoveResponseHeader(name string)
```

### New `Writer` body mutation methods (buffered mode only)

```go
func (w *Writer) SetRequestBody(data []byte)   // Drain full buffer, Append data
func (w *Writer) SetResponseBody(data []byte)
```

Calling outside a buffered body callback is a no-op.

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
// request. Call from OnRequestHeaders to ensure upstream sends uncompressed
// responses, avoiding the need to decode on the response path.
func NegotiateIdentity(w *up.Writer)
```

### Dependency plan

| Encoding | Dependency |
|---|---|
| `gzip` | stdlib `compress/gzip` |
| `deflate` | stdlib `compress/zlib` |
| `zstd` | `github.com/klauspost/compress/zstd` |
| `br` | `github.com/andybalholm/brotli` |
| `identity` / `""` | none |

Brotli and zstd included as regular deps (no build tags). Both are pure Go,
well-maintained, and commonly needed for CDN-served APIs.

`NegotiateIdentity` is the primary strategy; `Decode` is the safety net for
upstreams that ignore `Accept-Encoding`.

### Typical usage

```go
// Request headers: negotiate away compression.
func onReqHeaders(w *up.Writer, r *up.Request) {
    compress.NegotiateIdentity(w)
}

// Response headers: captured automatically into ResponseChunk fields.
// Body: decode then forward.
func onRespBody(w *up.Writer, chunk *up.ResponseChunk) {
    if !chunk.EndStream { return }
    decoded, err := compress.Decode(chunk.ContentEncoding, chunk.Data)
    if err != nil { /* handle */ }
    // dispatch on chunk.ContentType for user-space parsing
    switch {
    case strings.HasPrefix(chunk.ContentType, "application/json"):
        // user's JSON logic
    case chunk.ContentType == "application/x-protobuf":
        // user's proto logic
    }
}
```

---

## Future: multi-.so composition

Go's `plugin.Open` is not viable in a `-buildmode=c-shared` host (each `.so`
gets its own Go runtime; registries are separate instances).

The viable path when needed: a C ABI bridge — the user's `.so` exports a
C-compatible init symbol; the main `.so` loads it with `dlopen` and calls back
into its own exported `Register*` symbols. This mirrors the Envoy dynamic
modules ABI model and is a separate future project (`transit-bridge`).

The current `Register*` functions are already the stable interface this bridge
would target. No changes to transit are needed to keep that door open.

