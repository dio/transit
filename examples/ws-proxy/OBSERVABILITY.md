# ws-proxy Observability

`ws-proxy` has two observability planes:

1. **Envoy edge plane**: transit filter callbacks, Envoy access logs, dynamic
   metadata, and Envoy-native stats.
2. **Embedded actor plane**: the Go `http.Server` started by `up.RegisterWithGroup`,
   including WebSocket sessions, frame taps, and local background goroutines.

These planes can share names, dimensions, and the same OpenTelemetry backend,
but they should not share one API. `w.IncrementCounter` and `w.RecordHistogram`
(via a `Writer`) are only valid while Envoy is calling the HTTP filter.
`WSProxy.ServeHTTP` is normal Go code running behind a local Envoy cluster, so
it uses ordinary Go observability APIs.

## What to Emit Where

Use Envoy/transit for edge facts that Envoy owns:

- downstream method, path, route, authority, response code
- request rejection or mutation done in a transit filter
- dynamic metadata for Envoy access logs
- counters and histograms that should live in Envoy's stats store

Use actor-side Go observability for facts the embedded process owns:

- WebSocket session start/end
- frame-tap outcomes: model name, provider, token counts
- background actor lifecycle events
- actor-side latency histograms, error counters
- session close reasons

Keep dimensions consistent across both planes. Useful low-cardinality labels:
`component`, `route`, `provider`, `model`, `result`, `close_code`.
Avoid labels like user IDs, full prompts, or full URLs.

## Current Example

The example emits structured actor logs from `WSProxy.ServeHTTP` via `log/slog`:

```text
level=INFO msg="ws-proxy: session ended" path=/v1/responses model=gpt-4.1 input_tokens=21 output_tokens=8 duration=42ms result=ok reason=""
```

Implemented in `observability.go` via `recordActorSession`. The slog output is
visible in Envoy stderr (both `envoyCmd.Stdout` and `envoyCmd.Stderr` are wired
to os.Stderr in the e2e harness).

## OTLP Actor Metrics (Future Extension)

When `otel_endpoint` is set in filter config, `observability.go` should also emit:

| Metric | Type | Labels |
|--------|------|--------|
| `ws_proxy_sessions_total` | Counter | `model`, `result` |
| `ws_proxy_session_duration_ms` | Distribution | `model`, `result` |

Buckets for duration: `[5, 10, 25, 50, 100, 250, 500, 1000, 2500, 5000]` ms.

The wiring pattern (requires `github.com/dio/logging` + `github.com/tetratelabs/telemetry`):

```go
func WireOTelMetrics(ctx context.Context, endpoint string) (func(context.Context) error, error) {
    exp, err := otlpmetricgrpc.New(ctx,
        otlpmetricgrpc.WithEndpoint(endpoint),
        otlpmetricgrpc.WithInsecure(),
    )
    if err != nil {
        return nil, err
    }
    reader := metric.NewPeriodicReader(exp, metric.WithInterval(2*time.Second))
    mp := metric.NewMeterProvider(metric.WithReader(reader))
    sink := logging.NewOTelSink(mp, "ws-proxy")
    telemetry.SetGlobalMetricSink(sink)
    scope.UseLogger(logging.New(slog.Default()))
    return func(ctx context.Context) error {
        _ = sink.Shutdown(ctx)
        return mp.Shutdown(ctx)
    }, nil
}
```

Enable in filter config:

```yaml
filter_config:
  "@type": type.googleapis.com/google.protobuf.StringValue
  value: '{"listen_addr":"127.0.0.1:10001","otel_endpoint":"127.0.0.1:4317"}'
```

## Envoy OTel Stat Sink

For edge metrics recorded through transit, configure Envoy's OpenTelemetry stat
sink and point it at an OTLP/gRPC receiver:

```yaml
stats_flush_interval: 1s
stats_sinks:
  - name: envoy.stat_sinks.open_telemetry
    typed_config:
      "@type": type.googleapis.com/envoy.extensions.stat_sinks.open_telemetry.v3.SinkConfig
      grpc_service:
        envoy_grpc:
          cluster_name: otel_collector
      report_counters_as_deltas: true
      emit_tags_as_attributes: true

static_resources:
  clusters:
    - name: otel_collector
      type: STRICT_DNS
      typed_extension_protocol_options:
        envoy.extensions.upstreams.http.v3.HttpProtocolOptions:
          "@type": type.googleapis.com/envoy.extensions.upstreams.http.v3.HttpProtocolOptions
          explicit_http_config:
            http2_protocol_options: {}
      load_assignment:
        cluster_name: otel_collector
        endpoints:
          - lb_endpoints:
              - endpoint:
                  address:
                    socket_address: { address: 127.0.0.1, port_value: 4317 }
```

## Local Collector

Run a local OpenTelemetry Collector with OTLP/gRPC on port 4317:

```sh
docker run --rm -p 4317:4317 -p 4318:4318 \
  otel/opentelemetry-collector-contrib:latest
```

Point both Envoy's stat sink and `otel_endpoint` at `127.0.0.1:4317`.

## Upstream Auth Filter (ws-auth)

`ws-auth` is an upstream HTTP filter on the `upstream` cluster (not on the
listener). It has no observable surface of its own: it strips client credentials
and injects provider credentials before the upstream connection is made. Any
credential injection errors are logged at debug level.

For production: add a counter in the transit `up.Register` handler to track
injection failures, using `w.IncrementCounter` with a `MetricID` defined via
`up.RegisterWithConfig`.
