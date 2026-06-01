# orange — LLM proxy (Phase 1)

`orange` is an Envoy-based proxy for LLM API traffic. Phase 1 covers OpenAI
and Anthropic chat endpoints with credential injection, host selection, and
usage observability. MCP, cross-provider schema translation, and Realtime/WS
are explicitly out of scope.

## Goals

- One entry point for clients; backend selection is a server-side concern.
- Provider credentials never leave the proxy; clients send no API keys.
- Per-request usage and latency metrics under the `orange.*` namespace.
- Static configuration, single YAML, reloadable on restart.

## Non-goals (Phase 1)

- MCP routing or any MCP-aware behavior.
- Cross-provider schema translation (OpenAI ↔ Anthropic body rewriting).
- OpenAI Responses API, Realtime API, WebSocket.
- Client authentication / per-tenant quotas / rate limiting.
- Dynamic config (xDS), retries across providers, automatic fallback.
- Caching, prompt rewriting, guardrails.

## Supported surface

| Client API | Method | Path                   | Streaming |
|------------|--------|------------------------|-----------|
| OpenAI     | POST   | `/v1/chat/completions` | JSON, SSE |
| Anthropic  | POST   | `/v1/messages`         | JSON, SSE |

Streaming is detected by request body field (`stream: true`) and by response
`content-type: text/event-stream`. Request bodies are buffered up to 1 MiB;
larger requests are rejected with 413.

## Architecture

```
                   ┌──────────────────── orange (Envoy) ────────────────────┐
                   │                                                        │
client ──HTTP──►   │ Listener                                               │
                   │  └─ HTTP CM                                            │
                   │      ├─ downstream filter: classify                     │
                   │      │    • parse body, extract `model`                │
                   │      │    • lookup provider in model table             │
                   │      │    • write filter metadata: provider, model      │
                   │      │    • route to logical cluster (per provider)    │
                   │      ├─ downstream filter: observe-usage                │
                   │      │    • parse JSON tail or SSE terminal frames     │
                   │      │    • emit orange.* metrics + log line           │
                   │      └─ (optional) downstream filter: tap               │
                   │                                                        │
                   │ Cluster (one per provider family)                      │
                   │  ├─ upstream filter: credential-inject                  │
                   │  │    • set Authorization / x-api-key / version hdrs   │
                   │  │    • rewrite :authority, path if needed             │
                   │  └─ cluster extension: host-pick                       │
                   │       • choose endpoint from filter metadata            │
                   │       • set SNI / TLS context per upstream             │
                   └────────────────────────────────────────────────────────┘
                                       │
                                       ▼
                         api.openai.com / api.anthropic.com /
                         *-aiplatform.googleapis.com (Vertex)
```

The three referenced examples already cover the building blocks:
`examples/cluster-router` (cluster extension + filter metadata), and
`examples/mcp-profile-gateway` / `examples/mcp-profile-router` (downstream
classification with buffered bodies).

## Request lifecycle

1. **Receive**. Listener accepts request. HCM buffers body up to limit.
2. **Classify** (downstream filter).
   - Parse JSON. Extract `model` (required). Read `stream` (default false).
   - Look up `model` in the static model table → `(provider, upstream)`.
   - On miss: 404 with structured error (see Errors).
   - Set dynamic metadata:
     `orange.provider`, `orange.upstream`, `orange.model`, `orange.stream`.
   - Set route based on provider family.
3. **Route**. Standard Envoy route → logical cluster for the provider family
   (`cluster: orange_openai`, `cluster: orange_anthropic`, …).
4. **Credential inject** (upstream filter).
   - Strip any client-supplied `Authorization`, `x-api-key`, `anthropic-*`
     headers.
   - Inject credentials from the upstream's secret ref.
   - For Anthropic: add `anthropic-version` (configurable, default
     `2023-06-01`).
   - For Vertex-hosted variants: rewrite `:authority` and `:path` to the
     Vertex shape; mint/refresh OAuth token from a service account.
5. **Host pick** (cluster extension).
   - Read `orange.upstream` from filter state.
   - Resolve to endpoint + TLS context from the upstream registry.
6. **Forward**. Envoy forwards. Response body streamed back unchanged.
7. **Observe-usage**.
   - Non-streaming: on response complete, parse JSON, extract usage.
   - SSE: tap chunks; for Anthropic accumulate `message_start.usage` +
     `message_delta.usage`; for OpenAI read the final chunk's `usage` (set
     when client sends `stream_options.include_usage: true`; otherwise mark
     usage as unknown).
   - Emit metric + log; do not buffer the response.
8. **Tap** (optional). If enabled for the route, write raw request/response
   to the configured sink. Off by default.

## Config schema (sketch)

Single YAML, loaded at boot. Bootstrap wires this into Envoy via an EDS-less
static cluster set plus filter config.

```yaml
orange:
  listen: 0.0.0.0:8080

  upstreams:
    openai_direct:
      kind: openai
      endpoint: https://api.openai.com
      auth:
        type: bearer
        secret_ref: env:OPENAI_API_KEY

    anthropic_direct:
      kind: anthropic
      endpoint: https://api.anthropic.com
      anthropic_version: "2023-06-01"
      auth:
        type: x-api-key
        secret_ref: env:ANTHROPIC_API_KEY

    anthropic_vertex:
      kind: anthropic-on-vertex
      endpoint: https://us-east5-aiplatform.googleapis.com
      project: my-gcp-project
      region: us-east5
      auth:
        type: gcp-sa
        secret_ref: file:/etc/orange/sa.json

  # model -> upstream. First match wins. Globs allowed.
  models:
    - match: "gpt-4o*"
      upstream: openai_direct
    - match: "claude-sonnet-4*"
      upstream: anthropic_direct
    - match: "claude-opus-*"
      upstream: anthropic_vertex

  observability:
    metrics:
      enabled: true
      # OTel orange.* attributes always included:
      #   orange.system, orange.request.model, orange.response.model
      extra_attributes: []
    tap:
      enabled: false
      sink: file:/var/log/orange/tap.jsonl
```

Validation at boot: every `models[*].upstream` must exist; every upstream's
`kind` must match a known adapter; every `secret_ref` must resolve.

## Observability

Metrics (OTel, orange semconv where defined):

- `orange.client.token.usage` (histogram) — tagged `orange.token.type` =
  `input` | `output`.
- `orange.server.request.duration` (histogram).
- `orange.requests` (counter) — tagged `provider`, `model`, `stream`,
  `status_class`.

Trace attributes per request: `orange.system`, `orange.request.model`,
`orange.response.model`, `orange.response.id`, `http.response.status_code`,
`orange.upstream`.

Logs: one structured line per request with the above plus `usage.input`,
`usage.output`, `duration_ms`. SSE requests log on stream close.

Wiring follows `examples/observability` — same OTel exporters.

## Errors

All proxy-generated errors are JSON in the shape of the downstream provider
(OpenAI-style for `/v1/chat/completions`, Anthropic-style for
`/v1/messages`). Provider-originated errors pass through unchanged.

| Condition                  | Status | Code                         |
|----------------------------|--------|------------------------------|
| Body > 1 MiB               | 413    | `orange.body_too_large`      |
| Missing/invalid JSON       | 400    | `orange.invalid_request`     |
| Missing `model`            | 400    | `orange.model_required`      |
| Unknown `model`            | 404    | `orange.model_not_found`     |
| Secret unresolved          | 500    | `orange.config_error`        |
| Upstream connect failure   | 502    | `orange.upstream_unavailable`|

## Security

- Client auth is **not** in Phase 1. Deploy behind a trusted ingress.
- Credentials live in env vars or files referenced by `secret_ref`. They are
  never logged, never appear in tap output (tap redacts `Authorization`,
  `x-api-key`, and any `*-token` headers).
- TLS to upstream is required; certificate verification is on by default.

## Repo layout

```
cmd/orange/                 # binary that wraps Envoy + dynamic modules
modules/orange/
  classify/                 # downstream filter
  credinject/               # upstream filter
  hostpick/                 # cluster extension
  usage/                    # observe-usage filter
  tap/                      # optional tap filter
config/orange.yaml          # example static config
e2e/orange/                 # end-to-end tests (mirror existing examples)
```

## Test plan

- Unit tests per module (table-driven, mirroring `mcp-profile-router`).
- e2e suite with fake upstreams that assert:
  - correct credential header per provider,
  - SSE passthrough with usage extraction,
  - non-streaming usage extraction,
  - error mapping table,
  - tap redaction.
- Load test: 1k rps SSE, p99 added latency < 5 ms over passthrough baseline.

## Phase 2 (preview, not committed)

- MCP routing tier (`/mcp/*`), session affinity.
- OpenAI Responses API + Realtime/WS.
- Cross-provider schema translation as an opt-in filter.
- Client auth, per-tenant quotas, retries with fallback upstreams.
- Dynamic config via xDS / control plane.
