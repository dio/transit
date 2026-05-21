# e2e/sinks/alssink

An in-process gRPC server that implements
`envoy.service.accesslog.v3.AccessLogService` so Envoy can stream HTTP access
log entries directly to it via the built-in `envoy.access_loggers.http_grpc`
access logger — no custom filter required.

```go
sink := alssink.New()
port := sink.Start()   // returns the listening port

// block until a matching HTTP log entry arrives (or ctx expires)
entry, ok := sink.WaitForHTTPEntry(ctx, func(e *accesslogdatav3.HTTPAccessLogEntry) bool {
    return e.GetResponse().GetResponseCode().GetValue() == 200
})

// reset between test cases
sink.Reset()
```

## Envoy config

Wire the sink as an access logger on any HCM listener:

```yaml
access_log:
  - name: envoy.access_loggers.http_grpc
    typed_config:
      "@type": type.googleapis.com/envoy.extensions.access_loggers.grpc.v3.HttpGrpcAccessLogConfig
      common_config:
        log_name: als-e2e
        grpc_service:
          envoy_grpc:
            cluster_name: als-sink
      additional_request_headers_to_log:
        - x-request-id
```

The `als-sink` cluster must point at `127.0.0.1:<port>` with `http2_protocol_options: {}`.

## Difference from accessloggersink and otelsink

| Sink | Transport | Protocol | Needs custom filter? |
|---|---|---|---|
| `accessloggersink` | HTTP/1.1 | Custom JSON | Yes (`e2e-logger`) |
| `otelsink` | gRPC | OTLP logs/metrics/traces | No (Envoy built-ins) |
| `alssink` | gRPC | Envoy ALS proto | No (Envoy built-in) |
