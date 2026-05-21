# e2e

Integration tests for `github.com/dio/transit`. Each test spins up a real
Envoy binary loaded with `libe2e.so` (compiled from `e2e/filters/`) and drives
it over loopback.

## Prerequisites

```bash
make download-envoy          # fetches .bin/envoy
```

## Running

```bash
make e2e                     # full suite
ENVOY_BIN=.bin/envoy go test ./... -v -timeout=90s    # manual
TRANSIT_SKIP_BUILD=1 ...     # reuse previously built libe2e.so
```

Tests skip automatically when `ENVOY_BIN` is not set.

## Directory layout

```
e2e/
├── filters/          dynamic-module filters compiled into libe2e.so
├── sinks/            in-process servers that receive data from Envoy
│   ├── accessloggersink/   HTTP sink for custom JSON access log entries
│   ├── otelsink/           gRPC OTLP sink (logs + metrics + traces)
│   └── alssink/            gRPC ALS sink (envoy.service.accesslog.v3)
├── cmd/              helper binaries (e.g. upstream echo server)
├── *_test.go         one file per feature / filter under test
├── main_test.go      TestMain: builds .so, starts Envoy, wires all sinks
├── go.mod            separate module (isolates heavyweight gRPC/OTel deps)
└── go.sum
```

## Sinks

All three sinks live under [`sinks/`](sinks/) and expose a consistent pattern:
`New() → Start() → Wait*(ctx, predicate) → Reset()`.

| Package | Protocol | Use case |
|---|---|---|
| [`sinks/accessloggersink`](sinks/accessloggersink/) | HTTP/1.1 JSON | custom `e2e-logger` access logger entries |
| [`sinks/otelsink`](sinks/otelsink/) | gRPC OTLP | OTel logs, metrics, and traces from Envoy built-ins |
| [`sinks/alssink`](sinks/alssink/) | gRPC ALS proto | standard `envoy.access_loggers.http_grpc` entries |

## Test suites

| File | Suite | What it covers |
|---|---|---|
| `echo_test.go` | `EchoSuite` | pass-through filter, basic request routing |
| `guard_test.go` | `GuardSuite` | `SendLocalResponse`, `HeadersStatusStop` |
| `body_test.go` | `BodySuite` / `MutableBodySuite` | streaming and buffered body handling |
| `codec_test.go` | `CodecSuite` | gzip decode via `NegotiateIdentity` + `SetResponseBody` |
| `access_logger_test.go` | `AccessLoggerSuite` | dynamic module access logger, `GetTimingInfo` |
| `correlator_test.go` | `CorrelatorSuite` | HTTP filter ↔ access logger correlation via `sync.Map` |
| `otel_logs_test.go` | `OtelMetadataSuite` | `SetMetadata` → OTel access log body/attributes |
| `otel_metrics_test.go` | `OtelMetricsSuite` | Envoy stat-sink → OTLP metrics, tag extraction |
| `otel_traces_test.go` | `OtelTracesSuite` | `GetActiveSpan().SetTag` → OTLP span attributes |
