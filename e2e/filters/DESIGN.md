# HTTP filter ↔ access logger correlation

## Problem

The HTTP filter and the access logger see different slices of a request's lifecycle:

| Phase | Available to |
|---|---|
| Status code, response headers, upstream address, trace/span | HTTP filter (`OnResponseHeaders`) |
| Duration, byte counts, response flags, code details, retry count | Access logger (`OnLog / DownstreamEnd`) |

To emit a complete record you need both halves.

## Mechanism

Use `x-request-id` (Envoy auto-adds it) as a correlation key.

```
HTTP filter (OnResponseHeaders)         Access logger (OnLog/DownstreamEnd)
────────────────────────────────        ───────────────────────────────────
read AttributeIDRequestId               read h.GetHeader(request, x-request-id)
read chunk.StatusCode                   read h.GetTimingInfo(), GetBytesInfo(),
store partial record in sync.Map            GetResponseFlags(), GetResponseCode()
                                        pop partial record from sync.Map
                                        enrich with finalized fields
                                        emit complete record
```

The `sync.Map` is package-level, shared between the filter and the access logger registered
under the same name (both `init()`-registered in the same package).

## Key APIs used

**Filter side** (`RegisterWithResponse`):
- `w.GetAttributeString(up.AttributeIDRequestId)` — correlation key
- `chunk.StatusCode` — HTTP status from response headers
- `chunk.Headers` — full response header map (if needed)
- `chunk.Context` — per-stream `*any` slot for accumulating state across callbacks

**Access logger side** (`RegisterAccessLogger`):
- `h.GetHeader(up.HttpHeaderTypeRequest, "x-request-id")` — correlation key
- `h.GetTimingInfo()` — `RequestCompleteDurationNs`
- `h.GetBytesInfo()` — `BytesSent`, `BytesReceived`, wire bytes
- `h.GetResponseCode()` — finalized response code
- `h.GetResponseFlags()` + `up.ResponseFlagsString(...)` — error flags
- `h.GetAttributeString(up.AttributeIDResponseCodeDetails)` — code details

## e2e correlator filter

`correlator.go` in this package implements a minimal version of the pattern:
- HTTP filter `e2e-correlator`: deposits `{requestID → statusCode}` on response headers
- Access logger `e2e-correlator-logger`: pops the record, enriches it, POSTs to the sink
- `e2e/correlator_test.go` asserts `status_filter == response_code`
