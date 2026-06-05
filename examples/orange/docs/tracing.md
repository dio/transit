# OpenInference Tracing

Orange generates [OpenInference](https://github.com/Arize-ai/openinference/blob/main/spec/semantic_conventions.md)-conformant OTLP spans for every LLM and MCP call it proxies.

## How it works

Orange **participates** in the existing trace rather than owning it:

1. Envoy injects a `traceparent` header into each downstream request (requires Envoy tracing enabled).
2. The `orange-tracer` dynamic module filter reads that header, creates a child span with OpenInference attributes, and re-injects `traceparent` into the upstream request so providers can continue the chain.
3. Spans are exported to the same `OTEL_EXPORTER_OTLP_ENDPOINT` that Envoy uses.

## Configuration

| Env var | Default | Description |
|---------|---------|-------------|
| `OTEL_EXPORTER_OTLP_ENDPOINT` | *(unset → noop)* | OTLP gRPC endpoint, e.g. `http://localhost:4317` |
| `OTEL_TRACES_EXPORTER` | `otlp` | Set to `console` for pretty-printed stdout, `none` to disable |
| `OTEL_SDK_DISABLED` | `false` | Set to `true` to disable all tracing |
| `OTEL_SERVICE_NAME` | `orange` | Service name in resource attributes |
| `OTEL_PROPAGATORS` | `tracecontext,baggage` | W3C propagators (via autoprop) |

## Traced paths

| HTTP path | OI span kind | Span name | Key attributes |
|-----------|-------------|-----------|----------------|
| `POST /v1/chat/completions` | `LLM` | `ChatCompletion` | `llm.model_name`, `llm.system`, `llm.output_messages.*`, `llm.token_count.*`, `llm.finish_reason` |
| `POST /v1/messages` | `LLM` | `Messages` | same + `llm.system=anthropic` |
| `POST /v1/embeddings` | `EMBEDDING` | `Embeddings` | `embedding.model_name`, `llm.token_count.prompt` |
| `POST /v1/images/generations` | `LLM` | `ImageGeneration` | `llm.model_name`, `output.value` |
| `POST /v1/responses` | `LLM` | `Responses` | same as ChatCompletion |
| MCP `tools/call` | `TOOL` | `{toolName}` | `tool.name`, `output.value` |
| MCP `initialize` / list ops | `CHAIN` | `{method}` | — |

All spans also carry `openinference.span.kind`.

## Validation

```bash
# Quick smoke-test: start orange with console exporter, send one request,
# check that the span appears with the expected attributes.
make trace-validate
```

The script sets `OTEL_TRACES_EXPORTER=console`, sends a request to `/v1/chat/completions`, and `grep`s the stdout output for `"openinference.span.kind": "LLM"` and `llm.model_name`.

## Viewing traces

Point `OTEL_EXPORTER_OTLP_ENDPOINT` at any OTLP-compatible collector:

- **Jaeger** (all-in-one): `docker run -p 4317:4317 -p 16686:16686 jaegertracing/all-in-one`
- **Phoenix / Arize**: set `OTEL_EXPORTER_OTLP_ENDPOINT=https://app.phoenix.arize.com`
- **Grafana Tempo**: configure the OTLP receiver in your Tempo config
