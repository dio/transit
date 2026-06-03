# orange — multi-provider LLM proxy

A single Envoy listener that routes OpenAI and Anthropic API traffic based on
the `model` field of the request body, translates the wire format to each
backend's native schema, injects the right provider credential, and performs
TLS per provider off a single dynamic cluster.

```
client ──► :8080/v1/chat/completions ──► match (body: model=…)
                                          │  rewrites :authority → provider host
                                          ▼
                                       pick.ChooseHost
                                          │  (waits on match promise)
                                          ▼
                                       orange_default cluster
                                          │  (auto_host_sni derives SNI
                                          │   from :authority rewritten by match)
                              ┌───────────┴────────────┐
                              ▼                        ▼
                       api.openai.com            api.anthropic.com
                       (Bearer ...)              (x-api-key + anthropic-version)
```

Four dynamic modules ship in one `.so`:

| Module         | Phase                    | Job |
|----------------|--------------------------|-----|
| `orange-match` | downstream HTTP filter   | Parses `model` from the JSON body, looks up the provider, rewrites `:authority` to the provider host (so `auto_host_sni` picks the right SNI), writes routing filter state, and resolves the per-request `StreamPromise` so `pick` can complete. |
| `orange-pick`  | dynamic-modules cluster  | `ChooseHost` waits on the `StreamPromise` set by `match`. On `ServerInitialized` starts a STRICT_DNS-style background loop that re-resolves provider hostnames on TTL expiry and reconciles the host set. |
| `orange-adapt` | upstream HTTP filter     | Drives a four-phase translator pipeline (request headers → request body → response headers → response body chunks). Translates between client schema and backend wire format (OpenAI, Anthropic, Azure, Bedrock, Vertex, Groq, DeepInfra). Injects provider auth. |
| `orange-meter` | upstream HTTP filter     | Extracts LLM token usage from both streaming (SSE ring-buffer) and non-streaming (full JSON) responses. Emits `orange_input_tokens` / `orange_output_tokens` Envoy counters and dynamic metadata. |

## Why this is not just "header-based routing"

OpenAI/Anthropic SDK clients send the model name in the request **body**, not a
header. Envoy's router opens the upstream connection during
`router.decodeHeaders` — before the body filter has seen the body — so the
naive "write filter state from the body handler, read it in `ChooseHost`"
design races and loses.

`match` stores a `*up.StreamPromise[Decision]` in the per-stream object bag at
headers phase. `pick` returns a `ClusterLBCompletion` and waits on that
promise via `up.AsyncHostSelector`. When the body arrives, `match.bodyHandler`
parses the model, resolves the promise, and `pick` hops back to the cluster
main thread to complete the selection. No per-request goroutine is parked. See
`.agents/skills/transit-body-driven-cluster-routing/SKILL.md` for the full
phase-ordering rationale.

Per-stream cleanup is owned by `match`'s `OnStreamComplete` hook: it resolves
the promise with `orange.stream_terminated` (a CAS, so a real body-resolved
result always wins), guaranteeing the promise and the `OnResolve` callback
drain even when the body handler never runs (downstream disconnect, idle
timeout, foreign local reply).

## Per-provider TLS without a cluster explosion

Both providers live in **one** dynamic cluster (`orange_default`). `match`
rewrites the `:authority` pseudo-header to the provider hostname (e.g.
`api.openai.com` or `api.anthropic.com`) before the upstream connection is
established. The cluster's `auto_host_sni: true` and `auto_sni_san_validation:
true` then derive SNI and peer-cert validation from that `:authority` value
automatically — no `transport_socket_matches` required.

## Config

`orange.yaml` is the single source of truth for providers and model routing.
Loaded lazily at first request via the `ORANGE_CONFIG` env var (set by `make
run`/`make demo`).

```yaml
providers:
  openai:
    kind: openai
    endpoint: https://api.openai.com
    auth:
      type: bearer
      secret_ref: env://OPENAI_API_KEY

  anthropic:
    kind: anthropic
    endpoint: https://api.anthropic.com
    extra:
      anthropic_version: "2023-06-01"
    auth:
      type: anthropic
      secret_ref: env://ANTHROPIC_API_KEY

models:
  gpt-4o-mini:
    provider: openai
  claude-3-5-haiku-latest:
    provider: anthropic
```

`secret_ref: env://VAR` is resolved at config load; missing env vars fail boot
loudly — secrets are never silently empty.

Supported `auth.type` values: `bearer`, `x-api-key`, `anthropic`, `aws`, `gcp`.
Supported `backend_schema` values (override `kind` for the wire translator):
`azureopenai`, `awsbedrock`, `awsanthropic`, `gcpvertexai`, `gcpanthropic`.

## Quickstart

```bash
export OPENAI_API_KEY=sk-...
export ANTHROPIC_API_KEY=sk-ant-...

make demo     # builds the .so, renders envoy.yaml, prints curl examples,
              # then runs envoy in the foreground.
```

Then from another shell:

```bash
# OpenAI
curl -s :8080/v1/chat/completions -H 'content-type: application/json' \
  -d '{"model":"gpt-4o-mini","messages":[{"role":"user","content":"hi"}]}'

# Anthropic
curl -s :8080/v1/messages -H 'content-type: application/json' \
  -d '{"model":"claude-3-5-haiku-latest","max_tokens":32,
       "messages":[{"role":"user","content":"hi"}]}'

# Streaming (SSE) — add -N to curl, "stream": true to the body.
curl -N -s :8080/v1/chat/completions -H 'content-type: application/json' \
  -d '{"model":"gpt-4o-mini","stream":true,
       "messages":[{"role":"user","content":"count 1 to 5"}]}'
```

Error paths (handled by `match`, no upstream contacted):

```bash
# Missing model field → 400 orange.model_required
curl -i :8080/v1/chat/completions -H 'content-type: application/json' -d '{}'

# Unknown model → 404 orange.model_not_found
curl -i :8080/v1/chat/completions -H 'content-type: application/json' \
  -d '{"model":"gemini-1.5"}'
```

## Diagnostics

```bash
# Per-host TLS handshakes — both providers should tick after their first
# curl; cx_connect_fail should stay 0.
curl -s :9901/clusters | grep orange_default:: | grep -E 'cx_(total|connect_fail)'

# Token usage counters emitted by orange-meter.
curl -s :9901/stats | grep orange_

# Routing decisions are logged by match / pick / adapt at info level.
# Watch the `make demo` foreground; look for slog lines from each module.
```

## Tests

```bash
make test     # unit tests (config, match, adapt, pick, meter, translator)
make build    # cgo c-shared build of liborange.so
make e2e      # end-to-end tests against live providers (requires API keys)
```

## Layout

```
internal/
  config/        orange.yaml loader (secret_ref resolution, polling)
  pipeline/
    match/       downstream filter: body parse + StreamPromise resolve
    pick/        cluster extension: async ChooseHost + STRICT_DNS host refresh
    adapt/       upstream filter: translator pipeline + auth injection
    meter/       upstream filter: token counting → Envoy counters + metadata
  translator/    wire-format translators (openai↔openai/azure/bedrock/vertex/groq/deepinfra)
  apischema/     API schema types (openai, anthropic, gcp, awsbedrock)
  send/          JSON error-response helpers
  debug/         embedded admin HTTP server
codemod/         source-to-source migration tool (OpenAI SDK → orange endpoints)
cmd/             c-shared entrypoint (blank-imports all pipeline packages)
envoy.tmpl.yaml  Envoy config, ${ORANGE_TRUSTED_CA} substituted by make
orange.yaml      runtime config (providers, models)
```

## Status

| Stage | Status |
|-------|--------|
| M0 — skeleton + plumbing | done |
| M1 — match (body → upstream routing) | done |
| M2 — pick (cluster extension) | done |
| M3a — adapt: OpenAI Bearer | done |
| M3b — adapt: Anthropic x-api-key + version | done |
| M3c — body-driven routing via `ClusterLBCompletion` | done |
| M3d — per-provider TLS via `auto_host_sni` + `:authority` rewrite | done |
| M4 — streaming (SSE) passthrough | done |
| M5 — multi-backend translators (Azure, Bedrock, Vertex, Groq, DeepInfra) | done |
| M6 — meter: token counting via Envoy counters | done |
| M7 — `make demo` + this README | done |
