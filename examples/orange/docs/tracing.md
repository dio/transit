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

## Envoy tracing configuration

To have Envoy emit its own spans (and inject `traceparent` so orange can create child spans), add three blocks to `envoy.tmpl.yaml`:

### 1. OTLP collector cluster

```yaml
static_resources:
  clusters:
    - name: otel_collector
      type: LOGICAL_DNS
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
                    socket_address:
                      address: "${OTEL_COLLECTOR_HOST:-localhost}"
                      port_value: "${OTEL_COLLECTOR_PORT:-4317}"
```

### 2. Bootstrap tracing block

```yaml
tracing:
  http:
    name: envoy.tracers.opentelemetry
    typed_config:
      "@type": type.googleapis.com/envoy.config.trace.v3.OpenTelemetryConfig
      grpc_service:
        envoy_grpc:
          cluster_name: otel_collector
      service_name: orange
```

### 3. HCM tracing section

Inside the `http_connection_manager` filter config, add:

```yaml
generate_request_id: true
tracing:
  random_sampling:
    value: 100.0
  custom_tags:
    - tag: llm.model_name
      metadata:
        kind: { request: {} }
        metadata_key:
          key: orange
          path: [{ key: model }]
    - tag: llm.system
      metadata:
        kind: { request: {} }
        metadata_key:
          key: orange
          path: [{ key: provider_kind }]
    - tag: llm.provider_backend
      metadata:
        kind: { request: {} }
        metadata_key:
          key: orange
          path: [{ key: provider_backend }]
    - tag: llm.token_count.prompt
      metadata:
        kind: { request: {} }
        metadata_key:
          key: orange_meter
          path: [{ key: input_tokens }]
    - tag: llm.token_count.completion
      metadata:
        kind: { request: {} }
        metadata_key:
          key: orange_meter
          path: [{ key: output_tokens }]
```

`generate_request_id: true` causes Envoy to create a `traceparent` header on each request, which orange reads as the parent for its child spans. Both Envoy and orange export to the same `OTEL_EXPORTER_OTLP_ENDPOINT`.

## Viewing traces

Point `OTEL_EXPORTER_OTLP_ENDPOINT` at any OTLP-compatible collector:

- **Jaeger** (all-in-one): `docker run -p 4317:4317 -p 16686:16686 jaegertracing/all-in-one`
- **Phoenix / Arize**: set `OTEL_EXPORTER_OTLP_ENDPOINT=https://app.phoenix.arize.com`
- **Grafana Tempo**: configure the OTLP receiver in your Tempo config
