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

A buffered response-body filter that calls `RequestIdentity`, decodes a
gzip-compressed upstream response, strips `Content-Encoding`, and replaces
the body with the plain-text version via `SetResponseBody`.

Used by `CodecSuite` to verify the full gzip-decode pipeline.

## e2e-metadata — [`metadata.go`](metadata.go)

Sets two dynamic metadata values (namespace `"e2e"`) on every request so
Envoy's OpenTelemetry access logger can render them via `%DYNAMIC_METADATA(...)%`:

- `custom_field` = `"hello-from-filter"` → appears in the OTLP log body
- `method` = the HTTP method → appears as the `method` attribute

Used by `OtelMetadataSuite` to verify the `SetMetadata` → OTel pipeline.

## e2e-tracer — [`tracer.go`](tracer.go)

Annotates the active Envoy tracing span with two attributes so
`OtelTracesSuite` can verify they appear in the exported OTLP span:

- `e2e.custom` = `"hello-from-tracer"`
- `e2e.method` = the HTTP method

Requires that the HCM listener is configured with an OpenTelemetry tracer
(`envoy.tracers.opentelemetry`) and `generate_request_id: true`.
If `GetActiveSpan()` returns nil (no tracer), the handler is a no-op.

## e2e-upstream — [`upstream.go`](upstream.go)

An upstream HTTP filter (loaded via `HttpProtocolOptions.http_filters` on a
cluster) that stamps `x-upstream-filter: ran` on every response. Demonstrates
that dynamic module filters can run on the cluster side — after Envoy connects
to upstream but before the response reaches the downstream client.

Used by `UpstreamFilterSuite` to verify the upstream filter position is
distinct from the listener-side filter position.

## e2e-upstream-auth — [`upstream_auth.go`](upstream_auth.go)

An upstream HTTP filter that injects `Authorization: Bearer test-token` into
every request before it reaches the upstream server. Demonstrates the
credential-injection use case: auth headers are added at the cluster boundary
so listener-side filters remain auth-agnostic.

The plain upstream server echoes the header back as `x-received-authorization`;
`UpstreamFilterSuite.TestGet_authHeaderInjected` asserts the round-trip value.

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
