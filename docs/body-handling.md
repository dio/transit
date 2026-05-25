# Body handling

## Architecture

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

This means body-finalizing logic (emit metrics, flush state, release buffers)
can always live in the body handler gated on `EndStream: true` — no need to
duplicate it in the headers handler for the bodyless case.

### Operating modes

| Mode | Registration | Body handler called |
|---|---|---|
| **Streaming** | `RegisterWithBody` | Once per arriving chunk |
| **Buffered** | `RegisterWithMutableBody` | Once, with the full accumulated body |

Mode is fixed at registration time, not per-request.

Pick **streaming** when you only need to inspect or log body data without
holding the full payload in memory (token counting, access logging, SSE tap).

Pick **buffered** when you need to replace or rewrite the body
(`SetRequestBody` / `SetResponseBody`). Transit buffers the incoming chunks
internally and delivers one synthetic call with the complete body.

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

### Body replacement gotcha: streaming mode is a no-op

`SetRequestBody` and `SetResponseBody` are silently ignored outside buffered
mode. If you call them from a handler registered with `RegisterWithBody`,
nothing happens and no error is returned. Register with `RegisterWithMutableBody`
whenever body replacement is needed.

---

## Layer 2: content encoding (`up/compress`)

The `up/compress` package exposes `Decode`, `Encode`, and `RequestIdentity`.
Before inspecting a body, check `chunk.ContentEncoding` and call `Decode` if
non-empty; the upstream may still compress even if you sent `Accept-Encoding:
identity`, so always guard with a nil/error check.

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
    compress.RequestIdentity(w)
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
