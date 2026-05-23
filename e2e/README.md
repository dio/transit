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
├── testdata/
│   └── envoy.tmpl.yaml   text/template for the Envoy bootstrap config
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
| `body_test.go` | `BodySuite` / `MutableBodySuite` | streaming and buffered body handling; upstream-observed body and `content-length` after `SetRequestBody` |
| `compress_test.go` | `CompressSuite` | gzip decode via `RequestIdentity` + `SetResponseBody`; `RemoveResponseHeader` |
| `access_logger_test.go` | `AccessLoggerSuite` | `GetTimingInfo`, `GetBytesInfo`, `GetResponseCode`, `GetResponseFlags`; local-reply response code; non-empty flags on upstream error |
| `correlator_test.go` | `CorrelatorSuite` | HTTP filter ↔ access logger correlation via `sync.Map` |
| `otel_logs_test.go` | `OtelMetadataSuite` | `SetMetadata` → OTel access log body/attributes |
| `otel_metrics_test.go` | `OtelMetricsSuite` | Envoy stat-sink → OTLP metrics (Envoy plumbing, no Transit filter API) |
| `otel_traces_test.go` | `OtelTracesSuite` | `GetActiveSpan().SetTag` → OTLP span attributes |
| `als_test.go` | `AlsSuite` | dynamic metadata in ALS entries via `filter_metadata` |
| `upstream_filter_test.go` | `UpstreamFilterSuite` | dynamic module filter as upstream filter; auth injection; `RegisterWithGroup` |
| `lb_policy_test.go` | LB Policy | custom LB policy; host-selection assertion with two-host cluster |
| `cluster_test.go` | Cluster Extension | module-owned host discovery and host selection; TLS and mTLS |
| `cluster_scheduler_test.go` | Cluster Scheduler | `ClusterHandle.Schedule` delivers callbacks on the Envoy dispatcher thread |
| `async_callout_test.go` | `AsyncCalloutSuite` | `HTTPCallout` callback path; `Go`+`Do` fan-out; local response stops forwarding (negative assertion); request body after goroutine resume |

## Harness helpers

`main_test.go` provides reusable upstream helpers beyond the standard sinks:

| Helper | Returns | Use case |
|---|---|---|
| `startPlainUpstream()` | `int` port | Plain HTTP upstream; echoes `Authorization` header |
| `startForwardEchoUpstream()` | `int` port | Echoes every request header as `x-received-<name>` and reflects the request body |
| `startAsyncCalloutUpstream()` | `int` port | Returns the last path segment as body; used by callout filters |
| `startGzipUpstream()` | `int` port | Always returns `"hello compression"` gzip-compressed |
| `startIdentifiedUpstream(body)` | `int` port | Returns a fixed body string; used for LB policy host-selection assertions |
| `startRecorderUpstream()` | `*recorderUpstream` | Records every inbound request (method, path, headers, body, `ContentLength`); exposes `WaitFor`, `Len`, `Requests`, `Reset` |

`recorderUpstream` is the right primitive for:

- asserting what body and framing an upstream actually received (`WaitFor` + `Body`, `ContentLength`)
- asserting an upstream was **not** reached (`Len() == 0` after a local-response path completes)
