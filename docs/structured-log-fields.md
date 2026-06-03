# Structured Log Fields per Request

Transit provides two separate output paths for per-request context:

- **Process log** — one line per log call, free-form text. Written via
  `w.Slog()`. Good for real-time debugging. `filter=<name>` and
  `request_id=<x-request-id>` are included automatically on every line.
- **Access log** — one structured record per completed stream. The right
  place for fields a log pipeline will query (`user_id`, `workspace_id`,
  `model`, token counts). Written via `w.SetMetadata` / `w.AddLogAttrs`.

These are separate pipelines with separate Envoy configuration. The sections
below explain how to populate both.

---

## Automatic process-log fields

Every `w.Slog()` call automatically prepends:

```
<message> filter=<filter-name> request_id=<x-request-id>
```

`request_id` is captured from the `x-request-id` header on the first
`OnRequestHeaders` call. If Envoy has not set the header (no
`generate_request_id: true` in the listener) it is omitted.

No configuration required. No YAML needed.

---

## Per-request attrs: `w.AddLogAttrs`

`AddLogAttrs` attaches key-value pairs to the current stream. Once set, the
attrs are available for the rest of the stream's lifetime across all
callbacks.

```go
func onRequest(w *up.Writer, r *up.Request) {
    claims, err := validateJWT(r.Header("authorization"))
    if err != nil {
        w.SendLocalResponse(401, []byte("unauthorized"))
        return
    }
    w.AddLogAttrs(
        "user_id",      claims.UserID,
        "workspace_id", claims.WorkspaceID,
        "plan",         claims.Plan,
    )
}
```

By itself this stores the attrs on the stream but does not emit them
anywhere. To route them to the Envoy access log, register the filter with
`WithLogMetadata` (see below).

---

## Routing attrs to the access log: `WithLogMetadata`

```go
up.Register("auth", handler,
    up.WithLogMetadata("com.example.auth"),
)
```

When `WithLogMetadata(ns)` is set, every `w.AddLogAttrs("k", v)` call also
calls `w.SetMetadata(ns, "k", v)` automatically. The Envoy access log
formatter reads those values via `%DYNAMIC_METADATA(namespace:key)%`.

Value types that Envoy's metadata store natively supports (`string`, `bool`,
all integer and float types) are passed through unchanged. Any other type
(struct, slice, `time.Duration`, `time.Time`, etc.) is serialised to its
string representation — no panics, best-effort fidelity.

---

## Envoy YAML configuration

### Listener: enable request ID generation

```yaml
# listeners[].filter_chains[].filters[].typed_config (HttpConnectionManager)
generate_request_id: true   # ensures x-request-id is always present
```

### JSON access log

Add an `access_log` stanza to the listener. List every metadata key you
want as a top-level JSON field:

```yaml
access_log:
  - name: envoy.access_loggers.file
    typed_config:
      "@type": type.googleapis.com/envoy.extensions.access_loggers.file.v3.FileAccessLog
      path: /dev/stdout
      log_format:
        json_format:
          # standard Envoy fields
          start_time:    "%START_TIME%"
          method:        "%REQ(:METHOD)%"
          path:          "%REQ(:PATH)%"
          protocol:      "%PROTOCOL%"
          response_code: "%RESPONSE_CODE%"
          duration_ms:   "%DURATION%"
          bytes_sent:    "%BYTES_SENT%"
          bytes_received: "%BYTES_RECEIVED%"
          # request ID — read directly from header, no metadata needed
          request_id:    "%REQ(x-request-id)%"
          # fields written by the filter via w.AddLogAttrs + WithLogMetadata
          user_id:       "%DYNAMIC_METADATA(com.example.auth:user_id)%"
          workspace_id:  "%DYNAMIC_METADATA(com.example.auth:workspace_id)%"
          plan:          "%DYNAMIC_METADATA(com.example.auth:plan)%"
```

Each `%DYNAMIC_METADATA(namespace:key)%` entry maps to one `w.AddLogAttrs`
key. If the filter never wrote that key for a stream, Envoy renders it as
`null` in JSON.

### Text access log (alternative)

```yaml
log_format:
  text_format_source:
    inline_string: >-
      [%START_TIME%] %REQ(:METHOD)% %REQ(:PATH)% -> %RESPONSE_CODE%
      rid=%REQ(x-request-id)%
      uid=%DYNAMIC_METADATA(com.example.auth:user_id)%
      wid=%DYNAMIC_METADATA(com.example.auth:workspace_id)%
```

---

## Full example: auth filter with structured access log

### Go

```go
package main

import up "github.com/dio/transit/up"

func init() {
    up.Register("auth", onRequest,
        up.WithLogMetadata("com.example.auth"),
    )
}

func onRequest(w *up.Writer, r *up.Request) {
    log := w.Slog() // already has filter=auth request_id=<rid>

    claims, err := validateJWT(r.Header("authorization"))
    if err != nil {
        log.Warn("JWT validation failed", "error", err)
        w.SendLocalResponse(401, []byte(`{"error":"unauthorized"}`),
            [2]string{"content-type", "application/json"})
        return
    }

    // Written to dynamic metadata → appears as JSON fields in access log.
    // Also stored on the stream for programmatic use within later callbacks.
    w.AddLogAttrs(
        "user_id",      claims.UserID,
        "workspace_id", claims.WorkspaceID,
        "plan",         claims.Plan,
    )

    log.Info("request authenticated")
    // process log: "request authenticated filter=auth request_id=abc123"
    // access log:  {..., "user_id": "alice", "workspace_id": "acme", "plan": "pro"}
}
```

### Envoy YAML (listener fragment)

```yaml
http_filters:
  - name: envoy.filters.http.dynamic_modules
    typed_config:
      "@type": type.googleapis.com/envoy.extensions.filters.http.dynamic_modules.v3.DynamicModuleFilter
      dynamic_module_config:
        name: auth
      filter_name: auth

access_log:
  - name: envoy.access_loggers.file
    typed_config:
      "@type": type.googleapis.com/envoy.extensions.access_loggers.file.v3.FileAccessLog
      path: /dev/stdout
      log_format:
        json_format:
          start_time:    "%START_TIME%"
          method:        "%REQ(:METHOD)%"
          path:          "%REQ(:PATH)%"
          response_code: "%RESPONSE_CODE%"
          request_id:    "%REQ(x-request-id)%"
          user_id:       "%DYNAMIC_METADATA(com.example.auth:user_id)%"
          workspace_id:  "%DYNAMIC_METADATA(com.example.auth:workspace_id)%"
          plan:          "%DYNAMIC_METADATA(com.example.auth:plan)%"
```

---

## Namespace convention

Use a reverse-DNS prefix that identifies your organisation and filter. All
`AddLogAttrs` keys for a filter share one namespace, so the YAML
`%DYNAMIC_METADATA(...)%` references are predictable.

```
com.example.auth         ← JWT claims: user_id, workspace_id, plan
com.example.proxy        ← routing decisions: model, provider, region
com.example.tokens       ← LLM usage: input_tokens, output_tokens
```

Avoid short generic names (`auth`, `user`, `tokens`). They collide with
Envoy's built-in metadata namespaces (e.g. `envoy.filters.http.jwt_authn`)
and with other filters in the chain.

---

## `request_id` in the access log

`request_id` is already carried in the `x-request-id` header, which Envoy
sets before any filter runs (when `generate_request_id: true`). Reference it
directly in the access log format string:

```yaml
request_id: "%REQ(x-request-id)%"
```

There is no need to call `w.AddLogAttrs("request_id", ...)` — the value is
already available to the formatter without going through metadata.

---

## Multiple filters, multiple namespaces

When the listener has more than one Transit filter, each uses its own
namespace. The JSON access log can pull from all of them:

```yaml
json_format:
  request_id:    "%REQ(x-request-id)%"
  # from the auth filter
  user_id:       "%DYNAMIC_METADATA(com.example.auth:user_id)%"
  workspace_id:  "%DYNAMIC_METADATA(com.example.auth:workspace_id)%"
  # from the proxy filter
  model:         "%DYNAMIC_METADATA(com.example.proxy:model)%"
  provider:      "%DYNAMIC_METADATA(com.example.proxy:provider)%"
  # from the tokens filter
  input_tokens:  "%DYNAMIC_METADATA(com.example.tokens:input_tokens)%"
  output_tokens: "%DYNAMIC_METADATA(com.example.tokens:output_tokens)%"
```

---

## What does NOT appear in the access log

- **`w.Slog()` message text and inline args** — these go to the process log
  only. Access log entries are generated once per stream by the formatter,
  not once per log call.
- **`up.WithAttributes(...)` filter-level attrs** — these decorate the
  process log (`filter=name`) but are not written to dynamic metadata.
  If you need a static field (e.g. `region=us-west`) in the access log,
  write it from `OnRequestHeaders` via `w.AddLogAttrs`.
- **`w.SetFilterState(...)` values** — filter state is readable by Cluster
  Extensions but is invisible to the access log formatter. Use `SetMetadata`
  (or `AddLogAttrs` + `WithLogMetadata`) for access log fields.

---

## Pitfalls

| Pitfall | Symptom | Fix |
|---|---|---|
| `AddLogAttrs` without `WithLogMetadata` | Attrs stored but never emitted anywhere | Add `up.WithLogMetadata("com.example.ns")` to `Register` |
| Key missing from `json_format` | Field absent from JSON access log | Add `"key": "%DYNAMIC_METADATA(ns:key)%"` to the YAML |
| Namespace mismatch between Go and YAML | `%DYNAMIC_METADATA(...)%` renders `null` | Copy the namespace string exactly — it is case-sensitive |
| Calling `AddLogAttrs` in `OnStreamComplete` | Metadata write is a no-op | Move to any earlier callback; the stream is dead at `OnStreamComplete` |
| Using `SetFilterState` for access log fields | Access log shows `null` | `SetFilterState` is invisible to the formatter; use `AddLogAttrs` |
| No `generate_request_id: true` in listener | `request_id` field is empty | Add `generate_request_id: true` to the `HttpConnectionManager` config |
