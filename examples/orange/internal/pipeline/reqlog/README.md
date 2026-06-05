# reqlog

Envoy dynamic module filter that records every request and response as a structured `Record` and fans it out to all registered `Exporter` instances.

## Capture phases

| Phase | Fields captured |
|-------|----------------|
| **Request headers** | `method`, `path`, `host`, `request_id`, `trace_id`, `span_id`, request headers (if `record_request_headers`) |
| **Request body** | Accumulated up to `max_body_bytes` (if `record_request_body`); sets `request_truncated` when the limit is hit |
| **Response headers** | `status_code`, response headers (if `record_response_headers`) |
| **Response body end-of-stream** | Response body (if `record_response_body`); all `orange` and `orange_meter` dynamic metadata from `match`, `adapt`, and `meter` |
| **OnStreamFinalized** | Timing, byte counts, upstream address/attempts, response flags, local reply body. Record is assembled, field-filtered, and dispatched to exporters. |

**Position requirement**: place `orange-reqlog` as the first entry in `http_filters`. This ensures it captures the original (pre-match) request body and receives the response after all upstream filters (`adapt`, `meter`) have written their metadata.

## Exporters

```go
// Implement to receive finalized records. Must not block.
type Exporter interface {
    Export(r *Record)
    Close()
}
```

Register exporters from an `init()` in the binary before Envoy starts serving:

```go
reqlog.AddExporter(reqlog.NewStdoutExporter()) // JSON lines to stdout
```

Multiple exporters receive every record via a fan-out; each call is synchronous on the Envoy worker thread. `FilteredExporter` wraps any `Exporter` with an additional per-exporter `FieldFilter`, useful when different sinks need different projections (e.g. full bodies to a local file, stripped bodies to an HTTP backend).

## Field filtering

Filtering is applied globally in `OnStreamFinalized` before dispatch. The pipeline, in order:

1. **`allow_request_headers` / `allow_response_headers`** — if non-empty, only headers whose lowercase name is in the list are kept.
2. **`deny_request_headers` / `deny_response_headers`** — headers whose lowercase name is in the list are dropped (applied after the allow pass).
3. **`body_remove_paths`** — sjson dot-notation paths deleted from both request and response bodies (valid JSON only; non-JSON bodies are passed through unchanged).
4. **`body_redact_paths`** — sjson dot-notation paths whose string values are replaced with `"[REDACTED]"`.

Default config records request and response headers but not bodies. `max_body_bytes` defaults to 64 KiB.

## Record fields

`Record` combines HTTP semantics, orange LLM routing metadata, and Envoy stream-finalized counters into a single JSON-serialisable value. Key groups:

| Group | Fields |
|-------|--------|
| Identity | `request_id`, `trace_id`, `span_id` |
| Request | `method`, `path`, `host`, `request_headers`, `request_body`, `request_truncated` |
| Response | `status_code`, `response_headers`, `response_body`, `response_truncated` |
| Orange routing | `model`, `provider_backend`, `provider_kind`, `endpoint`, `backend_model`, `passthrough`, `gateway_client` |
| Token usage | `input_tokens`, `output_tokens`, `cached_input_tokens`, `reasoning_output_tokens`, `cache_creation_input_tokens`, `cache_read_input_tokens` |
| Image generation | `image_count`, `image_size`, `image_quality`, `response_modalities` |
| Upstream | `upstream_address`, `upstream_local_address`, `upstream_attempts`, `protocol` |
| Timing | `duration_ms`, `first_upstream_tx_byte_sent_ns`, `last_upstream_rx_byte_received_ns`, `upstream_cx_pool_ready_ms` |
| Bytes | `request_size_bytes`, `response_size_bytes`, `wire_bytes_received`, `wire_bytes_sent` |
| Error / flags | `has_error`, `response_flags`, `response_details`, `upstream_failure`, `local_reply_body` |

`has_error` is true when `status_code >= 500`, `upstream_failure` is non-empty, or `response_flags` contains an error flag (connection failure, timeout, no healthy upstream, etc.).
