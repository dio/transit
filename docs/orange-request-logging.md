# Orange Request Logging

This document plans the addition of structured request/response logging (access logs with body capture) to `examples/orange`, adapted from the pattern in `examples/request-ui` but extended with LLM-specific metadata and a multi-exporter architecture.

## Goals

- Capture per-request records that include HTTP headers, request/response bodies, timing, byte counts, upstream details, and orange-specific LLM metadata (model, provider, token counts).
- Support **multi-exporter** output so records can be fanned out to several sinks simultaneously (e.g., stdout JSON, file, an HTTP backend, OpenTelemetry) without changing filter code.
- Provide rich **field filtering** — allow/deny header lists, body JSON-path redaction, field removal — so operators can strip sensitive or high-volume fields per exporter.
- Skip the browser UI (no SSE, no `sink/ui.go` equivalent).

## Non-Goals

- Specific exporter implementations (sink transport is out of scope here; the plan defines the interface and wiring, not the backends).
- Changing the existing Envoy access log format (the new logger is additive).

---

## Architecture

### New package: `internal/pipeline/reqlog/`

A new Envoy dynamic module that registers itself **as an access logger** (not an HTTP filter). This mirrors `request-ui`'s `filter.go` pattern:

```
internal/pipeline/reqlog/
  filter.go        — Envoy module registration, per-stream accumulator, OnStreamFinalized
  record.go        — Record struct (all captured fields)
  config.go        — filterConfig with field selection and exporter wiring
  exporter.go      — Exporter interface and multi-exporter fan-out
  filter_test.go   — unit tests
```

The module registers under the name `orange-reqlog` using `up.RegisterAccessLogger` (the same ABI that `request-ui` uses via `up.ExchangeHooks`).

### Exporter interface

```go
// Exporter receives finalized request records. Each implementation is responsible
// for its own buffering and backpressure.
type Exporter interface {
    Export(ctx context.Context, r Record) error
    Close() error
}
```

A `MultiExporter` wraps a slice of `Exporter` values and fans out every `Record` in parallel (or sequentially — TBD based on latency requirements). If one exporter returns an error, the others still receive the record.

Exporters are registered at process startup (before `Envoy::Server::Configuration::NamedHttpFilterConfigFactory` calls `init`). The `orange-reqlog` filter holds a reference to the active `MultiExporter`.

---

## Record

```go
type Record struct {
    // Identity
    RequestID string
    TraceID   string
    SpanID    string

    // Request
    Method          string
    Path            string
    Host            string
    RequestHeaders  [][2]string // ordered pairs
    RequestBody     []byte      // nil if not captured or filtered out

    // Response
    StatusCode      int
    ResponseHeaders [][2]string
    ResponseBody    []byte

    // Orange LLM metadata (from dynamic metadata written by match + meter)
    Model                   string
    ProviderBackend         string
    ProviderKind            string
    Endpoint                string
    BackendModel            string
    Passthrough             bool
    GatewayClient           string
    InputTokens             int64
    OutputTokens            int64
    CachedInputTokens       int64
    ReasoningOutputTokens   int64
    CacheCreationInputTokens int64
    CacheReadInputTokens    int64

    // Upstream
    UpstreamAddress   string
    UpstreamAttempts  int
    LocalAddress      string
    Protocol          string

    // Timing and bytes (from OnStreamFinalized)
    DurationMs                       float64
    FirstUpstreamTxByteSentNs        int64
    LastUpstreamRxByteReceivedNs     int64
    UpstreamCxPoolReadyMs            float64
    RequestSizeBytes                 uint64
    ResponseSizeBytes                uint64
    WireBytesReceived                uint64
    WireBytesSent                    uint64

    // Error / flags
    HasError      bool
    ResponseFlags string
    ResponseCode  string // Envoy grpc status if applicable
    LocalReplyBody []byte
}
```

Orange-specific metadata fields are read inside `OnStreamFinalized` from `up.StreamInfo.DynamicMetadata` using the `orange` and `orange_meter` namespaces, consistent with how `envoy.yaml` reads them in the existing JSON access log format.

---

## Filter configuration

The module config is supplied via the `typed_config` in `envoy.yaml` under the access logger entry. It is a JSON object:

```json
{
  "record_request_headers":  true,
  "record_response_headers": true,
  "record_request_body":     false,
  "record_response_body":    false,
  "max_body_bytes":          65536,

  "field_filter": {
    "deny_request_headers":  ["authorization", "x-api-key", "x-orange-api-key"],
    "deny_response_headers": ["set-cookie"],
    "allow_request_headers": [],
    "allow_response_headers": [],
    "body_redact_paths": ["$.messages[*].content", "$.prompt"],
    "body_remove_paths":  ["$.stream_options"]
  }
}
```

### Field filtering rules

Fields are processed in this order:

1. **Allow list** (if non-empty): only headers matching the allow list are kept. Case-insensitive.
2. **Deny list**: headers matching the deny list are dropped after the allow-list pass.
3. **Body redact paths** (`body_redact_paths`): JSONPath expressions; matching leaf string values are replaced with `"[REDACTED]"`. Applied to both request and response body independently.
4. **Body remove paths** (`body_remove_paths`): JSONPath expressions; matching keys/elements are deleted entirely from the JSON before the record is emitted.
5. **`max_body_bytes`**: body is truncated to this length before JSONPath processing. Bodies that are not valid JSON after truncation are stored as raw bytes with a `truncated` flag set.

Field filtering is applied **inside the filter, before passing the record to the `MultiExporter`**, so individual exporters receive pre-filtered records and do not need their own redaction logic. If exporters need different field sets, a per-exporter `FieldFilter` wrapper can be layered on top (see below).

### Per-exporter field overrides (optional extension)

Each exporter can optionally specify its own `FieldFilter` that is applied on top of the global filter. This enables, for example, exporting full bodies to a local file while stripping them from an HTTP backend.

```go
type FilteredExporter struct {
    inner  Exporter
    filter FieldFilter
}
```

This is defined in the interface layer but specific exporter configurations are left to each exporter's own setup.

---

## Envoy configuration changes

Add the `orange-reqlog` access logger alongside the existing `envoy.access_loggers.stdout` logger. Because this is an **access logger** (not an HTTP filter), it appears in the `access_log` list on the listener's `http_connection_manager`, not in `http_filters`:

```yaml
access_log:
  - name: envoy.access_loggers.stdout
    typed_config:
      "@type": type.googleapis.com/envoy.extensions.access_loggers.stream.v3.StdoutAccessLog
      log_format: ...  # existing format unchanged

  - name: envoy.access_loggers.dynamic_modules
    typed_config:
      "@type": type.googleapis.com/envoy.extensions.access_loggers.dynamic_modules.v3.DynamicModuleAccessLog
      dynamic_module_config:
        name: orange
      logger_name: orange-reqlog
      log_format:
        json_format:
          # minimal envelope — the filter captures everything internally
          ts: "%START_TIME%"
          request_id: "%REQ(x-request-id)%"
      filter_state_objects_to_log: []
```

The `log_format` fields passed through `OnStreamFinalized` are supplemental; the filter reads dynamic metadata directly from the stream info, so the `json_format` block above can be minimal or empty.

---

## Integration with `cmd/main.go`

Add a blank import for the new package so its `init()` function registers the filter:

```go
_ "github.com/envoyproxy/dynamic-modules-sdk/examples/orange/internal/pipeline/reqlog"
```

The `init()` function calls `up.RegisterAccessLogger("orange-reqlog", ...)` and wires the configured `MultiExporter`. The exporter list is either hardcoded in `init()` or driven by environment variables (e.g., `ORANGE_REQLOG_EXPORTERS=stdout,file`).

---

## Implementation steps

1. **`record.go`** — define `Record` struct with all fields above.
2. **`exporter.go`** — define `Exporter` interface, `MultiExporter`, `FilteredExporter`, `FieldFilter`.
3. **`config.go`** — define `filterConfig`, JSON unmarshaling, `FieldFilter` application logic (allow/deny lists, JSONPath redact/remove using a lightweight JSONPath library or hand-rolled pointer traversal).
4. **`filter.go`** — register access logger, implement per-stream accumulator (`reqState`), `OnRequest` (headers, body buffering), `OnResponse` (status, response headers, response body buffering), `OnStreamFinalized` (read metadata, apply field filter, call `MultiExporter.Export`).
5. **`filter_test.go`** — unit tests for field filter logic, default config values, metadata extraction.
6. **`envoy.yaml` / `envoy.tmpl.yaml`** — add the `orange-reqlog` access logger entry.
7. **`cmd/main.go`** — add blank import.

---

## Key differences from `request-ui`

| Aspect | `request-ui` | `orange-reqlog` |
|--------|-------------|-----------------|
| Purpose | Demo capture + browser UI | Production LLM gateway audit log |
| Storage | Memory ring or Postgres | Exporter interface (TBD) |
| UI | Yes (SSE + HTML) | No |
| LLM metadata | No | Yes (model, tokens, provider) |
| Field filtering | No | Yes (allow/deny, redact, remove) |
| Multi-exporter | No (single sink) | Yes |
| Request body capture | Config field exists, not wired | Wired in `OnRequest` via body phase |

The request body hook gap in `request-ui` (config field `RecordRequestBody` exists but `OnRequest` does not buffer body chunks) must be implemented correctly in `orange-reqlog` from the start. Request body chunks arrive in the `OnRequestBody` callback; they must be accumulated into `reqState.RequestBody` with a size cap enforced per chunk.
