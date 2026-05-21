# e2e/otelsink

An in-memory OTLP gRPC receiver for e2e tests. A single `Sink` registers both
`LogsService` and `MetricsService` on one port.

```go
sink := otelsink.New()
port := sink.Start()   // returns the listening port

// block until a matching log record arrives (or ctx expires)
record, ok := sink.WaitForRecord(ctx, func(r *otlplogs.LogRecord) bool {
    return r.Body.GetStringValue() == "hello"
})

// block until a matching metric arrives (or ctx expires)
metric, ok := sink.WaitForMetric(ctx, func(m *otlpmetrics.Metric) bool {
    return m.Name == "server.uptime"
})
```

---

## Logs: dynamic metadata in log body / attributes

Envoy's `envoy.access_loggers.open_telemetry` renders `%DYNAMIC_METADATA(ns:key)%`
command operators in the `body` and `attributes` fields. A filter that calls
`w.SetMetadata("e2e", "custom_field", "hello")` will produce a log record whose
body is `"hello"` when the Envoy config contains:

```yaml
body:
  string_value: "%DYNAMIC_METADATA(e2e:custom_field)%"
```

---

## Metrics: Envoy tag extraction

Envoy's `envoy.stat_sinks.open_telemetry` applies its **tag extractor** before
exporting metrics. The extractor strips well-known prefixes (like the HTTP
connection manager's `stat_prefix`) from the metric name and re-encodes them as
OTLP data-point attributes.

**Concrete example** — after a request hits the `metadata-e2e` listener
(configured with `stat_prefix: metadata_e2e`):

| Envoy internal stat name                       | OTel metric name             | OTel attribute                                  |
|------------------------------------------------|------------------------------|-------------------------------------------------|
| `http.metadata_e2e.downstream_rq_total`        | `http.downstream_rq_total`   | `envoy.http_conn_manager_prefix=metadata_e2e`   |
| `http.metadata_e2e.downstream_rq_2xx`          | `http.downstream_rq_xx`      | `envoy.http_conn_manager_prefix=metadata_e2e`, `envoy.response_code_class=2` |
| `http.metadata_e2e.rq_direct_response`         | `http.rq_direct_response`    | `envoy.http_conn_manager_prefix=metadata_e2e`   |

**Implication:** predicates that search for per-listener metrics must match on
the attribute, not the metric name. Use the `metricHasAttr` helper defined in
`e2e/otel_metrics_test.go`:

```go
otelSink.WaitForMetric(ctx, func(m *otlpmetrics.Metric) bool {
    return strings.HasPrefix(m.Name, "http.") &&
        metricHasAttr(m, "envoy.http_conn_manager_prefix", "metadata_e2e")
})
```

Server-scope stats (`server.uptime`, `server.total_connections`, …) carry no
tag and are exported with their full name unchanged.

---

## Design note: dual-service conflict

Both `LogsServiceServer` and `MetricsServiceServer` define a method named
`Export`. Go does not allow two methods with the same name on one struct, so
`Sink` uses two thin adapter types (`logsSvc` / `metricsSvc`) that hold a
pointer to the shared `Sink` and each implement one of the two interfaces.
