# e2e/filters

This package contains the Envoy dynamic-module filters that are compiled into
`libe2e.so` and loaded by the e2e test harness. Each filter is registered in
its own file via `init()`. See the package-level doc comment in each file for
design details; this document serves as an index and cross-reference.

---

## echo — [`echo.go`](echo.go)

A pass-through filter that logs the request method and path at INFO level.
Used by `EchoSuite` to verify that requests reach the filter and that
non-stopping handlers do not interfere with the response.

## guard — [`guard.go`](guard.go)

Rejects requests lacking an `x-api-key` header with a 401 local response.
Used by `GuardSuite` to verify `SendLocalResponse` and the
`HeadersStatusStop` path.

## e2e-body / e2e-mutable-body — [`body.go`](body.go)

Two filters that exercise body handling:

- **e2e-body** (streaming) — passes body through; echoes request body
  metadata as `x-body-len` in response headers.
- **e2e-mutable-body** (buffered) — replaces the request body with
  `"replaced:<original>"` and echoes the replacement length as
  `x-replaced-len`.

Used by `BodySuite` and `MutableBodySuite`.

## e2e-codec — [`codec.go`](codec.go)

A buffered response-body filter that calls `NegotiateIdentity`, decodes a
gzip-compressed upstream response, strips `Content-Encoding`, and replaces
the body with the plain-text version via `SetResponseBody`.

Used by `CodecSuite` to verify the full gzip-decode pipeline.

## e2e-logger — [`access_logger.go`](access_logger.go)

A minimal access logger that POSTs a JSON record (timing, byte counts,
response flags, code details) to a configurable `sink_url` on each
`DownstreamEnd` event.

Config shape in Envoy YAML: `{"sink_url":"http://..."}`.

Used by `AccessLoggerSuite`.

## e2e-correlator / e2e-correlator-logger — [`correlator.go`](correlator.go)

The most complex filter in this package: it demonstrates how to combine an
HTTP filter and an access logger to emit a **complete** per-request record.

### Why two registrations?

Each callback sees a different slice of the request lifecycle:

| Phase | Data available |
|---|---|
| HTTP filter — `OnResponseHeaders` | status code, response headers, upstream address, trace/span |
| Access logger — `OnLog / DownstreamEnd` | duration, byte counts, response flags, code details |

To emit a record that contains both halves, the two registrations share a
package-level `sync.Map` keyed by `x-request-id` (which Envoy auto-generates).

### Flow

```
HTTP filter (OnResponseHeaders)          Access logger (OnLog/DownstreamEnd)
────────────────────────────────         ───────────────────────────────────
read AttributeIDRequestId                read h.GetHeader(request, x-request-id)
read chunk.StatusCode                    read h.GetTimingInfo(), GetBytesInfo(),
store partial record in sync.Map             GetResponseFlags(), GetResponseCode()
                                         pop partial record from sync.Map
                                         enrich with finalized fields
                                         POST complete record to sink
```

### APIs used

Filter side (`RegisterWithResponse`):
- `w.GetAttributeString(up.AttributeIDRequestId)` — correlation key
- `chunk.StatusCode` — HTTP status from response headers

Access logger side (`RegisterAccessLogger`):
- `h.GetHeader(up.HttpHeaderTypeRequest, "x-request-id")` — correlation key
- `h.GetTimingInfo()` — `RequestCompleteDurationNs`
- `h.GetBytesInfo()` — `BytesSent`, `BytesReceived`
- `h.GetResponseCode()` — finalized response code
- `h.GetResponseFlags()` + `up.ResponseFlagsString(...)` — error flags

`CorrelatorSuite` in `e2e/correlator_test.go` asserts `status_filter == response_code`.
