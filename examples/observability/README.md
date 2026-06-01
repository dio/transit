# Observability Example

This example demonstrates how to instrument an Envoy dynamic module filter to drive observability signals that **Envoy exports** to an OTLP backend: **distributed tracing (spans), metrics (counters), and structured logs**.

## Key Principle: Envoy Exports, Filter Drives

⚠️ **Nothing is emitted directly from Go.** The filter only:
- Tags the active span (set by Envoy)
- Increments counters (collected by Envoy)
- Writes dynamic metadata (exported by Envoy)

**Envoy** is responsible for:
- Creating and exporting spans via the tracing sink
- Collecting and flushing counters via the stats sink
- Emitting structured logs via the access logger

## Scenario

You're building a proxy or gateway that needs to observe all HTTP traffic flowing through it. Your observability backend (e.g., Datadog, Honeycomb, New Relic) expects to receive three types of telemetry:

1. **Traces** — per-request span with metadata (operation name, HTTP method, path, status code, model identifier)
2. **Metrics** — counters tracking request and response volumes
3. **Logs** — structured records with request metadata (status code, model name if present)

The filter **observability** demonstrates how to:
- Instrument the request/response lifecycle via the active span (Envoy exports it)
- Increment counters at key phases (Envoy collects and exports them)
- Write dynamic metadata that Envoy exports via access logs

## Architecture

```
┌──────────┐
│  Client  │
└────┬─────┘
     │ HTTP request
     ▼
┌─────────────────────────────────────────────────┐
│  ENVOY PROXY (does all exporting)               │
├─────────────────────────────────────────────────┤
│                                                 │
│  Filter: observability (Go code)                │
│  ├─ SetOperation("http.request")                │ ──┐
│  ├─ SetTag("http.method", "GET")                │   │ Go filter
│  ├─ SetTag("http.path", "/api/v1/users")        │   │ only DRIVES
│  ├─ SetTag("llm.model", "claude-sonnet")        │   │ signals, does
│  ├─ SetTag("http.status_code", "200")           │   │ NOT export
│  ├─ IncrementCounter(requests, 1)               │ ──┘
│  ├─ IncrementCounter(responses, 1)              │
│  └─ SetMetadata("observability:model", "...")   │
│                                                 │
│  ↓ (Envoy framework handles export)             │
│                                                 │
│  Tracing Sink (envoy.tracers.opentelemetry)     │
│  ├─ Export span with tags to OTLP               │
│                                                 │
│  Stats Sink (envoy.stat_sinks.open_telemetry)   │
│  ├─ Export counters to OTLP                     │
│                                                 │
│  Access Logger (envoy.access_loggers.otel)      │
│  ├─ Export log with metadata attributes to OTLP │
│                                                 │
└────┬────────────────────────────────────────────┘
     │ gRPC OTLP (Envoy exports all signals)
     ▼
┌───────────────────────────┐
│  OTLP Collector / Backend │
│  (Datadog, Honeycomb,     │
│   or in-memory sink in    │
│   e2e tests)              │
└───────────────────────────┘

Legend:
------
Go filter (this example): Sets metadata, tags, counters
Envoy: Exports ALL telemetry via OTLP
```

## What the Filter Does

⚠️ **The filter only DRIVES signals. Envoy EXPORTS them.**

The filter manipulates signals that Envoy's configured sinks will export:

### On Request Headers

```go
func onRequest(w *up.Writer, r *up.Request) {
    // 1. Get the active span (created by Envoy, exported by Envoy's tracer)
    span := w.GetActiveSpan()
    if span != nil {
        // Filter only DRIVES the span via tags
        // Envoy exports it to otel-collector cluster
        span.SetOperation("http.request")
        span.SetTag("http.method", r.Method)
        span.SetTag("http.path", r.Path)
    }

    // 2. If x-model header present, tag it and write metadata
    // (metadata will be exported by Envoy's access logger)
    if model, ok := ModelFromHeader(r.Header("x-model")); ok {
        if span != nil {
            span.SetTag("llm.model", model)  // → Envoy exports on span
        }
        w.SetMetadata("observability", "model", model)  // → Envoy exports in log
    }

    // 3. Increment counter (Envoy collects and exports to stats sink)
    w.IncrementCounter(requestsID, 1)
}
```

### On Response (Status + EndStream)

```go
func onResponse(w *up.Writer, chunk *up.ResponseChunk) {
    if chunk.StatusCode != 0 {
        // Response headers phase: tag status code on the span
        // (Envoy will export this span to the tracer)
        span := w.GetActiveSpan()
        if span != nil {
            code, _ := w.GetAttributeString(up.AttributeIDResponseCode)
            span.SetTag("http.status_code", code.String())
        }
        // Metadata will be exported by Envoy's access logger
        w.SetMetadata("observability", "status_code", chunk.StatusCode)
    }

    if chunk.EndStream {
        // Response body complete: increment counter
        // (Envoy collects and flushes to stats sink per stats_flush_interval)
        w.IncrementCounter(responsesID, 1)
    }
}
```

**Flow:**
1. Filter calls `SetTag()` / `SetMetadata()` / `IncrementCounter()`
2. Envoy's frameworks capture these calls
3. Envoy's configured sinks export them via OTLP

## Observability Signals

### Traces (exported by Envoy's tracer)

The filter drives a **span** that **Envoy exports** with:
- **Operation name:** `"http.request"` (set by filter)
- **Attributes** (set by filter, exported by Envoy):
  - `http.method` — e.g., "GET", "POST"
  - `http.path` — e.g., "/api/v1/users"
  - `http.status_code` — e.g., "200"
  - `llm.model` — (optional, only if `x-model` header present) — e.g., "claude-sonnet-4-6"

**Exported to:** OTLP tracing sink (configured in `tracing:` block)
**Example span query:** "All requests to `/api/v1/users` with status 200 and model claude-sonnet"

### Metrics (exported by Envoy's stats sink)

The filter increments **counters** that **Envoy collects and exports**:
- `observability_requests_total` — incremented once per request (at headers phase)
- `observability_responses_total` — incremented once per response (at body end)

**Exported to:** OTLP stats sink (configured in `stats_sinks:` block)
**Export interval:** Every `stats_flush_interval` (1s in this example)
**Example query:** "Rate of requests and responses per minute"

### Logs (exported by Envoy's access logger)

The filter writes **dynamic metadata** that **Envoy's access logger exports** as structured log records:
- `status_code` — HTTP response code (always present, set by filter)
- `model` — Model name (present only if `x-model` header was sent, set by filter)

**Exported to:** OTLP access logger (configured in `access_log:` block)
**Example query:** "All requests with status 200 where model='claude-opus-4-7'"

## Running the Example

### Build

```bash
make -C examples/observability build
```

Outputs: `examples/observability/libobservability.so`

### Unit Tests

```bash
make -C examples/observability test
```

Tests the filter logic in isolation (no Envoy):
- `ModelFromHeader` parsing

### E2E Tests

```bash
make -C examples/observability e2e
```

Requires:
- Envoy binary at `.bin/envoy` (run `make download-envoy` if missing)
- macOS (tested on macOS 14+)

Starts a real Envoy instance with the filter loaded, an in-memory OTLP sink, and runs 14 tests.

### Run Manually

```bash
make -C examples/observability run
```

Starts Envoy listening on `localhost:10000`. Forward requests:

```bash
# Plain request
curl http://localhost:10000/

# Request with model header
curl -H 'x-model: claude-sonnet-4-6' http://localhost:10000/api/v1/chat
```

Logs are printed to stdout in JSON format.

## E2E Test Coverage

⚠️ **Tests verify Envoy's exports, not Go code exports.**

The e2e test suite:
1. Starts a real Envoy with the filter loaded
2. Starts an in-memory OTLP sink
3. Sends HTTP requests through Envoy
4. Asserts that **Envoy exported** correct signals to the sink

All 14 tests verify across the observability signals:

### Baseline Tests (3)

| Test | Purpose |
|------|---------|
| `TestGet_noModel_responds200` | Request without x-model header returns 200 |
| `TestGet_withModel_responds200` | Request with x-model header returns 200 |
| `TestPost_responds200` | POST request returns 200 |

### Trace Tests (6)

| Test | Verifies |
|------|----------|
| `TestTrace_spanOperation` | Span operation name is "http.request" |
| `TestTrace_httpMethodTag` | Span has `http.method="GET"` attribute |
| `TestTrace_httpPathTag` | Span has `http.path="/test/path"` attribute |
| `TestTrace_llmModelTag_present` | Span has `llm.model="claude-opus-4-1"` when header sent |
| `TestTrace_llmModelTag_absent` | Span has no `llm.model` attribute when header not sent |
| `TestTrace_httpStatusCodeTag` | Span has `http.status_code="200"` attribute |

**Key insight:** Traces prove that request metadata flows through the filter and into OTEL span attributes.

### Metric Tests (2)

| Test | Verifies |
|------|----------|
| `TestMetric_requestsCounterIncremented` | Metrics exported via OTLP stats sink |
| `TestMetric_responsesCounterIncremented` | Metrics exported via OTLP stats sink |

**Key insight:** Metrics prove that Envoy's stats sink is connected to the OTLP collector and receiving counter increments.

### Log Tests (3)

| Test | Verifies |
|------|----------|
| `TestLog_statusCodeInMetadata` | Log record has `status_code="200"` attribute |
| `TestLog_modelPresentWhenHeaderSet` | Log record has `model="claude-sonnet-4-1"` when header sent |
| `TestLog_modelAbsentWhenNoHeader` | Log record created (model empty/absent when no header) |

**Key insight:** Logs prove that dynamic metadata set by the filter reaches the OTLP access logger.

## Test Architecture

```
TestMain
├─ Create OTLP sink (in-memory gRPC receiver)
├─ Start Envoy with:
│  ├─ observability filter loaded
│  ├─ tracing: OpenTelemetry → otel-collector cluster
│  ├─ stats_sinks: OpenTelemetry → otel-collector cluster
│  ├─ access_log: OpenTelemetry → otel-collector cluster
│  └─ stats_flush_interval: 1s
├─ Run 14 test cases
└─ Stop Envoy

Each test:
├─ Send HTTP request (with/without x-model header)
├─ Wait for signal in OTLP sink (span, metric, or log record)
└─ Assert signal has expected attributes
```

### OTLP Sink

An embedded in-memory gRPC receiver that:
- Listens on a random loopback port
- Implements LogsService, MetricsService, and TraceService
- Stores spans, metrics, and log records in memory
- Provides `WaitForSpan()`, `WaitForMetric()`, `WaitForRecord()` helpers for tests

## Key Files

| File | Purpose |
|------|---------|
| `observability.go` | Filter implementation (request/response handlers) |
| `observability_test.go` | Unit tests (filter logic only) |
| `cmd/main.go` | Dummy main (required for CGO module compilation) |
| `e2e/e2e_test.go` | E2E tests with real Envoy and OTLP sink |
| `e2e/testdata/envoy.tmpl.yaml` | Envoy bootstrap config template |
| `Makefile` | Build, test, and e2e targets |
| `../internal/otelsink/sink.go` | Embedded OTLP gRPC receiver for e2e tests |

## Envoy Configuration Highlights

### Tracing

```yaml
tracing:
  http:
    name: envoy.tracers.opentelemetry
    typed_config:
      "@type": type.googleapis.com/envoy.config.trace.v3.OpenTelemetryConfig
      grpc_service:
        envoy_grpc:
          cluster_name: otel-collector
      service_name: "observability-e2e"
```

→ Exports all spans from `GetActiveSpan()` to the OTLP collector.

### Stats Flush

```yaml
stats_flush_interval: 1s
```

→ Counters are flushed to the OTLP sink every 1 second.

### Metrics Export

```yaml
stats_sinks:
  - name: envoy.stat_sinks.open_telemetry
    typed_config:
      "@type": type.googleapis.com/envoy.extensions.stat_sinks.open_telemetry.v3.SinkConfig
      grpc_service:
        envoy_grpc:
          cluster_name: otel-collector
      report_counters_as_deltas: false
```

→ Envoy's counters are exported as OTLP metrics.

### Access Logging

```yaml
access_log:
  - name: envoy.access_loggers.open_telemetry
    typed_config:
      "@type": type.googleapis.com/envoy.extensions.access_loggers.open_telemetry.v3.OpenTelemetryAccessLogConfig
      grpc_service:
        envoy_grpc:
          cluster_name: otel-collector
      attributes:
        values:
          - key: status_code
            value:
              string_value: "%DYNAMIC_METADATA(observability:status_code)%"
          - key: model
            value:
              string_value: "%DYNAMIC_METADATA(observability:model)%"
```

→ Dynamic metadata set by the filter is exported as log record attributes.

## What This Example Proves

✅ **Traces work via Envoy:** Filter drives spans → Envoy exports to OTLP tracer sink
✅ **Metrics work via Envoy:** Filter increments counters → Envoy collects and exports to OTLP stats sink
✅ **Logs work via Envoy:** Filter writes metadata → Envoy's access logger exports to OTLP sink
✅ **Zero direct Go exports:** Filter only DRIVES signals; **Envoy** handles ALL exports
✅ **Integration:** Filter (instrumentation) + Envoy (export) → OTLP collector → Backend observability system

## Next Steps

1. **Deploy to your observability backend** (Datadog, Honeycomb, etc.) by:
   - Replacing the in-memory OTLP sink with your real collector endpoint
   - Updating Envoy config to point to your backend
   - Adding authentication (mTLS, API keys) as needed

2. **Customize the filter** to:
   - Extract additional request/response metadata
   - Emit more granular metrics (latency histograms, error rates)
   - Sample traces selectively (e.g., only errors or slow requests)

3. **Monitor in your observability platform:**
   - Create dashboards for request volume, latency, error rates
   - Set up alerts on metric anomalies
   - Correlate logs, traces, and metrics using trace IDs

## See Also

- [Envoy Dynamic Modules](https://www.envoyproxy.io/docs/envoy/latest/api-v3/api/v3/service/ext_proc/v3/external_processor.proto)
- [OpenTelemetry Protocol (OTLP)](https://opentelemetry.io/docs/specs/otel/protocol/)
- [Transit](https://github.com/dio/transit) — Dynamic module SDK for Go
