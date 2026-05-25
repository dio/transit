# Trace Propagation Example

This example demonstrates end-to-end W3C trace context propagation through a
full path where every participant exports its own span to an OTLP collector:

```
Client
  └─► Envoy inbound listener          [span: trace-propagation.ingress]
        │  dynamic module sets operation name + http.method / http.path tags
        ↓
      Embedded Go HTTP server          [span: trace-propagation.embedded]
        │  Go OTLP SDK — extracts parent context, creates child span,
        │  injects updated traceparent into the egress request
        ↓
      Envoy egress listener            [span: trace-propagation.egress]
        │  dynamic module sets operation name + http.method / http.path tags
        ↓
      backend cluster ← upstream HTTP dynamic module filter
        │  stamps x-upstream-filter: ran on every request;
        │  tags the active span with upstream.filter=ran if available
        ↓
      Backend sink
```

It demonstrates:

- `up.RegisterWithGroup` — starts an embedded HTTP server alongside the filter
- `up.Register` — registers `trace-propagation-egress` (listener filter) and
  `trace-propagation-upstream` (cluster upstream filter) from the same `.so`
- `w.GetActiveSpan()` / `span.SetOperation` / `span.SetTag` — enriches
  Envoy-native spans from both listener and upstream filter context
- Go OTLP SDK (`otlptracegrpc`) — instruments the embedded server as a proper
  trace participant with its own child span
- W3C `propagation.TraceContext` — extracts and injects `traceparent` across
  hop boundaries so spans are correctly linked parent → child

## Build

```sh
make -C examples/trace-propagation build
```

The shared library is written to `examples/trace-propagation/libtrace-propagation.so`.

## Run

Start any local HTTP backend on port `8080`, run an OTLP collector on `4317`,
then start Envoy:

```sh
make -C examples/trace-propagation run
```

Requests to `http://127.0.0.1:10000/` produce three linked spans in your
collector: ingress (Envoy), embedded (Go server), and egress (Envoy).

## Test

Unit tests cover `CopyTraceHeaders`:

```sh
make -C examples/trace-propagation test
```

The e2e test builds the shared library, starts an in-memory OTLP gRPC sink,
starts Envoy, and asserts all span names and the upstream filter header:

```sh
make -C examples/trace-propagation e2e
```

## Runtime environment variables

| Variable | Default | Purpose |
|---|---|---|
| `TRACE_PROPAGATION_LISTEN_ADDR` | `127.0.0.1:9192` | Embedded server listen address |
| `TRACE_PROPAGATION_EGRESS_URL` | `http://127.0.0.1:9193` | Egress Envoy listener URL |
| `TRACE_PROPAGATION_OTEL_ENDPOINT` | `127.0.0.1:4317` | OTLP gRPC endpoint for embedded server spans |
