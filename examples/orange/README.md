# orange — multi-provider LLM proxy

A single Envoy listener that routes OpenAI and Anthropic API traffic based on
the `model` field of the request body, injects the right provider credential
on the upstream side, and handshakes each provider with its own SNI off a
single dynamic cluster.

```
client ──► :8080/v1/chat/completions ──► classify (body: model=…)
                                          │
                                          ▼
                                       hostpick.ChooseHost
                                          │  (waits on classify body)
                                          ▼
                                       orange_default cluster
                                          │  (per-host TLS via
                                          │   transport_socket_matches)
                              ┌───────────┴────────────┐
                              ▼                        ▼
                       api.openai.com            api.anthropic.com
                       (Bearer ...)              (x-api-key + anthropic-version)
```

Three dynamic modules ship in one `.so`:

| Module       | Phase                    | Job |
|--------------|--------------------------|-----|
| `classify`   | downstream HTTP filter   | Parse `model` from the JSON body, look up the upstream, rewrite `:authority`, resolve the per-request pending so `hostpick` can complete. |
| `hostpick`   | dynamic-modules cluster  | `ChooseHost` returns a `ClusterLBCompletion`; a goroutine waits on the pending from `classify` and completes with the matching host. |
| `credinject` | upstream HTTP filter     | Strip client-supplied auth, inject the provider credential. OpenAI → `Authorization: Bearer …`. Anthropic → `x-api-key` + `anthropic-version`. |

## Why this is not just "header-based routing"

OpenAI/Anthropic SDK clients send the model name in the request **body**, not a
header. Envoy's router opens the upstream connection during
`router.decodeHeaders` — before the body filter has seen the body — so the
naive "write filter state from the body handler, read it in `ChooseHost`"
design races and loses.

`classify` mints a per-request token at headers phase, registers a
`pending.Pending` under it, and writes the token to filter state. `hostpick`
returns a `ClusterLBCompletion` and a goroutine waits on that pending. When
the body arrives, `classify.bodyHandler` parses the model and resolves the
pending; the waiter hops back to the cluster's main thread and completes the
selection. See `.agents/skills/transit-body-driven-cluster-routing/SKILL.md`
for the full phase-ordering rationale.

## Per-provider TLS without a cluster explosion

Both providers live in **one** dynamic cluster (`orange_default`). Each host
is registered with `HostSpec.Metadata{"sni": <provider hostname>}`; the
cluster's `transport_socket_matches` selects the matching `UpstreamTlsContext`
per connection. No `auto_sni` (which gets sampled before `ChooseHost` runs
and so can't see body-driven decisions) — see SKILL.md *"Auto-SNI sampling
time"* for why.

## Config

`orange.yaml` is the single source of truth. Each module reads its section
lazily via the `ORANGE_CONFIG` env var (set by `make run`/`make demo`).

```yaml
upstreams:
  openai_direct:
    kind: openai
    endpoint: https://api.openai.com
    auth: { type: bearer, secret: env://OPENAI_API_KEY }
  anthropic_direct:
    kind: anthropic
    endpoint: https://api.anthropic.com
    anthropic_version: "2023-06-01"
    auth: { type: x-api-key, secret: env://ANTHROPIC_API_KEY }

models:
  - { match: "gpt-4o*",  upstream: openai_direct }
  - { match: "gpt-4.1*", upstream: openai_direct }
  - { match: "claude-*", upstream: anthropic_direct }
```

Missing env vars / unreadable files fail Envoy boot loudly — secrets aren't
silently empty.

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

Error paths (handled by `classify`, no upstream contacted):

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

# Routing decisions are logged by classify / hostpick / credinject at info.
# Watch the `make demo` foreground; look for:
#   orange-classify body: resolved model=… upstream=… host=… kind=…
#   orange-hostpick: token=… completing with host upstream=…
#   orange-credinject: upstream=… authority=… kind=…
```

## Tests

```bash
make test     # unit tests (config + classify + credinject + hostpick + pending)
make build    # cgo c-shared build of liborange.so
```

## Layout

```
classify/    downstream filter: body parse + pending resolve
credinject/  upstream filter: strip + inject per provider
hostpick/    cluster extension: async ChooseHost via ClusterLBCompletion
pending/     process-wide token → Pending registry
config/      orange.yaml loader (env:// secret resolution)
cmd/         c-shared entrypoint (registers all three modules)
envoy.tmpl.yaml  Envoy config, ${ORANGE_TRUSTED_CA} substituted by make
orange.yaml      runtime config (upstreams, models, classify, credinject)
```

## Status

| Stage | Status |
|-------|--------|
| M0 — skeleton + plumbing | done |
| M1 — classify (body → upstream) | done |
| M2 — hostpick (cluster extension) | done |
| M3a — credinject OpenAI Bearer | done |
| M3b — credinject Anthropic x-api-key + version | done |
| M3c — body-driven routing via `ClusterLBCompletion` | done |
| M3d — per-provider TLS via `transport_socket_matches` | done |
| M4 — streaming (SSE) passthrough | done |
| M5 — `make demo` + this README | done |
