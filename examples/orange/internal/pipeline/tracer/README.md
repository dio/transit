# tracer

OpenInference-conformant OTLP tracing for orange LLM and MCP calls. Runs as the `orange-tracer` Envoy dynamic-module filter and participates in the existing trace started by Envoy — it reads the incoming `traceparent` header as the parent context, creates a child span with OpenInference attributes, and re-injects `traceparent` into the upstream request so the chain continues downstream.

Spans are exported to whatever `OTEL_EXPORTER_OTLP_ENDPOINT` is configured — the same endpoint Envoy uses. No separate exporter address is needed.

## Span lifecycle

```
request headers  → create span (endpoint from path, parent from traceparent header)
                   inject traceparent into upstream request headers
response headers → read model/provider from orange dynamic metadata (written by match)
                   set llm.model_name, llm.system on span
response body    → accumulate bytes (non-streaming) or ring buffer (streaming)
response end     → parse body → set output attrs + token counts → span.End()
```

## Traced paths

| HTTP path | `openinference.span.kind` | Span name | Key attributes |
|-----------|--------------------------|-----------|----------------|
| `POST /v1/chat/completions` | `LLM` | `ChatCompletion` | `llm.model_name`, `llm.system`, `llm.output_messages.*`, `llm.token_count.*`, `llm.finish_reason` |
| `POST /v1/messages` | `LLM` | `Messages` | same + `llm.system=anthropic` |
| `POST /v1/embeddings` | `EMBEDDING` | `Embeddings` | `embedding.model_name`, `llm.token_count.prompt` |
| `POST /v1/images/generations` | `LLM` | `ImageGeneration` | `llm.model_name`, `output.value` |
| `POST /v1/responses` | `LLM` | `Responses` | same as ChatCompletion |
| MCP `tools/call` | `TOOL` | `{toolName}` | `tool.name`, `output.value` |
| MCP `initialize` / list ops | `CHAIN` | `{method}` | — |

## Files

| File | Purpose |
|------|---------|
| `tracer.go` | `up.Register`, TracerProvider init, request/response callbacks, carrier adapters |
| `semconv.go` | Inline OpenInference semantic convention constants |
| `response.go` | Response body parsers: OpenAI (JSON + SSE), Anthropic (JSON + SSE), Embeddings, Images |
| `mcp.go` | `StartMCPSpan`, `InjectMCPTrace`, `RecordMCPResult` — used by `mcp/handlers.go` |
| `tracer_test.go` | Unit tests using `tracetest.NewInMemoryExporter` |

## Configuration

| Env var | Default | Description |
|---------|---------|-------------|
| `OTEL_EXPORTER_OTLP_ENDPOINT` | *(unset → noop)* | OTLP gRPC endpoint, e.g. `http://localhost:4317` |
| `OTEL_TRACES_EXPORTER` | `otlp` | `console` for pretty-printed stdout; `none` to disable |
| `OTEL_SDK_DISABLED` | `false` | `true` disables all tracing |
| `OTEL_SERVICE_NAME` | `orange` | Service name in resource attributes |
| `OTEL_PROPAGATORS` | `tracecontext,baggage` | W3C propagators (via autoprop) |

## Validation

```bash
make trace-validate          # runs unit tests (no Envoy required)
./demos/tracing/validate     # same
./demos/tracing/validate --live  # sends a live request; checks span output
```
