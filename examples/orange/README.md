# orange — multi-provider LLM & MCP proxy

Orange is a single Envoy listener that fronts:

- **LLM APIs** — OpenAI Chat Completions, OpenAI Responses (HTTP and WebSocket),
  Anthropic Messages — and routes each request to the right backend based on
  the `model` field in the body. It translates between schemas, injects the
  right provider credentials, and tracks token usage.
- **MCP backends** — one or more upstream MCP (Model Context Protocol)
  servers, multiplexed under a single client-facing session. Clients see one
  `/mcp` endpoint; Orange fans out `initialize`, merges `tools/list`,
  routes `tools/call` by tool prefix, and proxies the SSE stream.

Everything ships as **one** Go-built dynamic module (`liborange.so`) loaded by
Envoy.

```
                                                                                              ┌─► api.openai.com
   POST /v1/chat/completions ─┐                                                               │
   POST /v1/responses         │                                            orange-adapt       ├─► api.anthropic.com
   GET  /v1/responses  (WS)   ├─► orange-match ─► orange-pick ─────────►  + orange-meter ─────┤
   POST /v1/messages          │   (body parses    (ChooseHost waits         (translate,       └─► (Azure / Bedrock /
                              │    model →         on match's promise,       inject auth,         Vertex / Groq / …)
                              │    provider)       picks resolved IP)        count tokens)
                              │
   POST   /mcp[/profile|/s/x] │                                                                ┌─► kiwi
   GET    /mcp[/...]   (SSE)  ├─► orange-mcp ─────────────────────────►  orange-mcp-egress ───┼─► github
   DELETE /mcp[/...]          │   (sealed public session,                 (sets backend →     └─► aws-knowledge
                              ┘    fans out to backends,                   pick host, applies     (any MCP server)
                                   merges /tools/list,                     per-backend auth)
                                   tool-prefix routes /tools/call)
```

All three egress paths (`orange_default`, `orange-mcp-egress`,
`orange-responsesws-default`) are *separate cluster entries* in
`envoy.yaml`, but they all delegate host selection to the same `pick`
extension and share the same DNS/SNI machinery — one resolver, one host
table, one TLS context, many possible upstream hostnames.

All upstream traffic — LLM providers *and* MCP backends — exits through one
shared dynamic cluster (`orange_default` / `orange-mcp-egress` /
`orange-responsesws-default`). Per-host TLS SNI is derived automatically from
each registered host's `Hostname` field. No `transport_socket_matches`. No
cluster per provider.

## Modules at a glance

| Module                     | Phase                                 | Job |
|----------------------------|---------------------------------------|-----|
| `orange-match`             | downstream HTTP filter                | Parses `model` from the JSON body, looks up the provider, rewrites `:authority` to the provider host (so per-host SNI selects the right cert), writes routing filter state, and resolves the per-request `StreamPromise` so `pick` can complete host selection. |
| `orange-pick`              | dynamic-modules cluster               | `ChooseHost` waits on the `StreamPromise` set by `match`. A background goroutine re-resolves every provider hostname on TTL expiry and reconciles the cluster's host set in place — the same model as Envoy's built-in `STRICT_DNS`. |
| `orange-adapt`             | upstream HTTP filter                  | Drives a four-phase translator pipeline (request headers → request body → response headers → response body chunks). Translates between client schema and backend wire format (OpenAI, Anthropic, Azure, Bedrock, Vertex, Groq, DeepInfra). Injects provider auth. |
| `orange-meter`             | upstream HTTP filter                  | Extracts token usage from both streaming (SSE ring-buffer) and non-streaming responses. Emits `orange_input_tokens` / `orange_output_tokens` Envoy counters and dynamic metadata. |
| `orange-responsesws`       | filter + WebSocket sidecar            | Tunnels OpenAI Responses-API WebSocket traffic. Sits in `upgrade_configs`; the HTTP filter is a no-op whose `WithGroup` boots the sidecar that bridges client WS ↔ upstream WS. |
| `orange-responsesws-meter` | upgrade-path HTTP filter              | Token metering for the Responses WebSocket path. |
| `orange-mcp`               | filter + HTTP sidecar                 | Public-facing MCP endpoint. Owns session encryption, profile fan-out, list merging, tool-prefix routing, SSE multiplexing. |
| `orange-mcp-egress-match`  | downstream HTTP filter (egress chain) | Translates the `x-orange-mcp-backend` internal header into a `match.StateUpstream` filter-state entry so `pick` selects the right upstream IP for the MCP backend. |

The same `pick` cluster extension serves all three egress clusters — LLM,
MCP, and Responses WS — because they share the same DNS/SNI machinery.

> **Config delivery is pluggable.** The demo ships a single `orange.yaml`
> on disk, polled and `fsnotify`-watched — deliberately the simplest thing
> that works. Providers and MCP servers are added and removed *inside
> Orange*, not by reshaping Envoy's static config or going through xDS;
> the dynamic cluster's host set is reconciled by `pick` as the config
> snapshot changes. The pipeline only ever calls `config.Get()`, so the
> file loader can be replaced by a control-plane gRPC stream, an HTTP
> fetch with ETag, Consul/etcd, a signed S3 object, or Vault for secrets
> — without touching filters, `pick`, or `envoy.yaml`:
>
> ```go
> // internal/config/config.go (sketch)
> func Get() *Config { return pipeline().Snapshot() }
>
> // Today: file loader polls ORANGE_CONFIG + fsnotify watch.
> // Tomorrow: point pipeline() at any source that produces *Config snapshots —
> //   a gRPC long-poll, a KV watch, a signed object fetched periodically, …
> //   The pipeline, filters, and pick.cluster don't change.
> ```
>
> Treat `orange.yaml` as a demo convenience, not the contract.

## Why this isn't just header-based routing

OpenAI/Anthropic SDK clients send the model name in the request **body**, not
a header. Envoy's router opens the upstream connection during
`router.decodeHeaders` — before the body filter has seen the body — so the
naive "write filter state from the body handler, read it in `ChooseHost`"
design races and loses.

`match` stores a `*up.StreamPromise[Decision]` in the per-stream object bag
at headers phase. `pick` returns a `ClusterLBCompletion` and waits on that
promise via `up.AsyncHostSelector`. When the body arrives, `match.bodyHandler`
parses the model, resolves the promise, and `pick` hops back to the cluster
main thread to complete the selection. No per-request goroutine is parked.
See `.agents/skills/transit-body-driven-cluster-routing/SKILL.md` for the
full phase-ordering rationale.

Per-stream cleanup is owned by `match`'s `OnStreamComplete` hook: it resolves
the promise with `orange.stream_terminated` (a CAS, so a real body-resolved
result always wins). The promise and its `OnResolve` callback drain even when
the body handler never runs (downstream disconnect, idle timeout, foreign
local reply).

## One cluster, per-host SNI (`pick`)

Both LLM providers and every MCP backend share one dynamic cluster. The
cluster has **one** `transport_socket` with `auto_host_sni: true` and
`auto_sni_san_validation: true`.

How does that work when the hosts have different certificates?

`pick.Init` and `pick.applyResolved` register each resolved IP with the
provider's hostname:

```go
h.AddHosts([]up.HostSpec{{
    Address:  "104.18.6.192",          // resolved IP
    Hostname: "api.openai.com",        // ← becomes SNI + cert validation name
    Metadata: map[string]string{"sni": "api.openai.com"},
}})
```

At connect time Envoy reads the `Hostname` on the selected `HostPtr` and uses
it as both the SNI value and the SAN to validate against the server
certificate. The cluster doesn't care that `api.openai.com` and
`api.anthropic.com` need different certs — each *host* already carries the
right name.

`match` plays the other half of this trick: it rewrites `:authority` to the
provider hostname from its **downstream headers handler**, before the
upstream connection pool opens. Doing it in the body handler is too late —
the router has already locked SNI by the time the body arrives. (`pick`'s
README has the gory phase-ordering details.)

The DNS refresh loop is STRICT_DNS-shaped: it wakes at the earliest TTL
across all registered hostnames, re-resolves, and reconciles the host set in
place — keeping existing `HostPtr`s when the IP set is unchanged (so the
round-robin counter doesn't reset), only churning when an IP actually
appears or disappears. A transient DNS failure never evicts a healthy host.

## MCP support

Orange speaks the [MCP streamable-HTTP](https://modelcontextprotocol.io)
transport. Clients see one URL — `http://localhost:8080/mcp[/<profile>]` —
and behind it Orange can multiplex many backends.

### Profiles vs. single-server shorthand

`orange.yaml` defines:

- **servers** — concrete MCP backends (endpoint, namespace, allowed tools,
  per-server auth).
- **profiles** — named bundles of servers and per-profile tool filters /
  auth overrides. A profile groups multiple backends under a single public
  session.

```yaml
mcp:
  profiles:
    default:
      tools:
        kiwi:
          include: ["search-flight"]
        aws-knowledge:
          include: ["read_documentation", "search_documentation"]
          optional: true   # profile init still succeeds if this backend is down
        github:
          include: ["search_repositories"]
          optional: true
      auth:
        github:
          type: bearer
          secret_ref: env://GITHUB_TOKEN
    kiwi-only:
      tools:
        kiwi: {}
  servers:
    kiwi:
      endpoint: https://mcp.kiwi.com
      namespace: kiwi
      tools: { include: ["search-flight"] }
    aws-knowledge:
      endpoint: https://knowledge-mcp.global.api.aws
      namespace: aws
      tools: { include: ["read_documentation", "search_documentation"] }
    github:
      endpoint: https://api.githubcopilot.com/mcp/
      namespace: github
      auth:
        type: bearer
        secret_ref: env://GITHUB_TOKEN
      tools: { include: ["search_repositories", "get_file_contents"] }
```

Clients address Orange in one of two ways:

| URL                          | Resolves to                                      |
|------------------------------|--------------------------------------------------|
| `POST /mcp/<profile>`        | The named profile — fan-out across all members.  |
| `POST /mcp/s/<server>`       | A single server — same machinery, route of one.  |
| `POST /mcp` (no segment)     | The `default` profile.                           |

### Sessions

`initialize` is **profile-atomic**: every required backend in the profile
must complete `initialize` successfully. Optional backends (`optional:
true`) may fail without sinking the session. On success, Orange:

1. Collects each backend's private `mcp-session-id`.
2. Encrypts the whole envelope (`{route, subject, backends:[{name,
   session_id, caps}…]}`) with AEAD into a single opaque token.
3. Returns it as the public `mcp-session-id` header.

The client never sees a backend session ID. Subsequent requests carry the
public token; Orange decrypts it, picks the right backend session per call,
and routes accordingly. Same trick for SSE `Last-Event-Id`: Orange wraps
`{backend, event_id}` so resume after a disconnect knows which backend to
re-attach to.

Key rotation works with multiple keys:

```bash
ORANGE_MCP_SESSION_KEYS=new-key,old-key   # new encrypts, both decrypt
ORANGE_MCP_SESSION_KEYS=orange-generated  # ephemeral; invalid after restart
ORANGE_MCP_SESSION_KEYS=                  # unset → dev key + loud warning
```

### Per-method handling

| Method                          | What Orange does |
|---------------------------------|------------------|
| `initialize`                    | Fan out to every backend in the profile. Required-backend failure → `502`, no public session issued. Returns the first successful backend's response body, plus an `x-orange-mcp-backend-status` header summarising each backend's outcome. |
| `tools/list` / `prompts/list` / `resources/list` | Fan out to all session backends. Merge results, prefixing tool/prompt/resource names with `<backend>__` so call routing is unambiguous. |
| `tools/call`                    | Split the prefixed name (`<backend>__<tool>`), route to that single backend's session. |
| Server-initiated request response (from client) | Decode the synthetic `{"backend":..., "id":...}` envelope Orange injected on the way out, forward to the right backend. Heartbeat pings get acked locally. |
| Other methods                   | Broadcast to all backends; return the first 2xx response. |
| `GET /mcp/...` (SSE)            | Multiplex SSE streams from every backend, rewriting event IDs and server-initiated request IDs through the same envelope scheme so the client can correlate responses on the way back. |
| `DELETE /mcp/...`               | Tear down each backend's session. |

### Response headers worth knowing

Orange sets these on its responses so clients (and access logs) can see what
actually happened:

- `mcp-session-id` — the opaque public session token.
- `x-orange-mcp-method` — the JSON-RPC method that was processed.
- `x-orange-mcp-tool` — for `tools/call`, the fully prefixed tool name.
- `x-orange-mcp-backend-status` — per-backend status summary, e.g.
  `kiwi=ok,github=optional-failed,aws-knowledge=ok`.

## Config

`orange.yaml` is the single source of truth for providers, models, MCP
servers, and MCP profiles. It is loaded lazily at first request via the
`ORANGE_CONFIG` env var (set by `make run` / `make demo`) and reloaded on
file change.

```yaml
llm:
  providers:
    openai:
      kind: openai
      endpoint: https://api.openai.com
      auth: { type: bearer, secret_ref: env://OPENAI_API_KEY }
    anthropic:
      kind: anthropic
      endpoint: https://api.anthropic.com
      extra: { anthropic_version: "2023-06-01" }
      auth: { type: anthropic, secret_ref: env://ANTHROPIC_API_KEY }
  models:
    gpt-4o-mini:
      provider: openai
      metadata:
        description: "GPT-4o mini via OpenAI."
        context_length: 128000
        max_tokens: 16384
        tags: ["chat", "responses", "fast", "vision"]
    claude-haiku-4-5:
      provider: anthropic

mcp:
  servers: { … }
  profiles: { … }
```

`secret_ref: env://VAR` is resolved at config load; missing env vars fail
boot loudly — secrets are never silently empty.

Supported `auth.type` values: `bearer`, `x-api-key`, `anthropic`, `aws`,
`gcp`.
Supported `backend_schema` values (override `kind` for the wire translator):
`azureopenai`, `awsbedrock`, `awsanthropic`, `gcpvertexai`, `gcpanthropic`.

## Quickstart

```bash
export OPENAI_API_KEY=sk-...
export ANTHROPIC_API_KEY=sk-ant-...
export GITHUB_TOKEN=ghp-...        # only needed if you use the github MCP backend

make demo     # builds the .so, renders envoy.yaml, prints curl examples,
              # then runs envoy in the foreground.
```

Then from another shell.

### LLM traffic

```bash
# OpenAI Chat Completions
curl -s localhost:8080/v1/chat/completions -H 'content-type: application/json' \
  -d '{"model":"gpt-4o-mini","messages":[{"role":"user","content":"hi"}]}'

# OpenAI Responses API — non-streaming
curl -s localhost:8080/v1/responses -H 'content-type: application/json' \
  -d '{"model":"gpt-4o-mini","input":"hi"}'

# OpenAI Chat Completions — streaming (SSE)
curl -N -s localhost:8080/v1/chat/completions -H 'content-type: application/json' \
  -d '{"model":"gpt-4o-mini","stream":true,
       "messages":[{"role":"user","content":"count 1 to 5"}]}'

# OpenAI Responses API — streaming (SSE)
curl -N -s localhost:8080/v1/responses -H 'content-type: application/json' \
  -d '{"model":"gpt-4o-mini","stream":true,"input":"count 1 to 5"}'

# Anthropic Messages
curl -s localhost:8080/v1/messages -H 'content-type: application/json' \
  -d '{"model":"claude-haiku-4-5","max_tokens":32,
       "messages":[{"role":"user","content":"hi"}]}'

# Anthropic Messages — streaming (SSE)
curl -N -s localhost:8080/v1/messages -H 'content-type: application/json' \
  -d '{"model":"claude-haiku-4-5","max_tokens":64,"stream":true,
       "messages":[{"role":"user","content":"count 1 to 5"}]}'
```

### MCP traffic

`mcp-demo` is a curl wrapper for the Orange MCP endpoint. It captures the
public `mcp-session-id` from `initialize` and then drives `tools/list`,
`tools/call`, SSE stream, and `DELETE`.

```bash
# Initialize the default profile and print the export line for ORANGE_MCP_SESSION_ID.
./mcp-demo profile=default
export ORANGE_MCP_SESSION_ID='<session printed by initialize>'

./mcp-demo profile=default list
./mcp-demo profile=default call kiwi__search-flight \
    '{"flyFrom":"SFO","flyTo":"JFK","departureDate":"10/06/2026"}'
./mcp-demo profile=default stream
./mcp-demo profile=default delete

# Target a single MCP server directly with the s/<name> shorthand.
./mcp-demo server=github
./mcp-demo server=github list

# See all request/response headers for debugging.
./mcp-demo --headers profile=default
```

Useful overrides:

```bash
ORANGE_MCP_BASE_URL=http://localhost:8080/mcp
ORANGE_MCP_SHOW_HEADERS=1
```

## Codex CLI support

Orange handles all three endpoints Codex targets:

- `POST /v1/responses` — HTTP non-streaming and SSE (Responses API passthrough)
- `GET /v1/responses` — WebSocket upgrades through `orange-responsesws`
- `POST /v1/chat/completions` — Chat Completions fallback

Run Codex against Orange without a Codex-side provider key — Orange injects
the real upstream credentials from `orange.yaml`.

**Terminal 1** — start the proxy:

```bash
make demo
```

**Terminal 2** — run Codex through Orange:

```bash
./codex-demo                              # interactive, HTTP
./codex-demo --ws                         # interactive, Responses WebSocket
./codex-demo exec "write a hello-world HTTP server in Go"      # one-shot
./codex-demo --ws exec "write a hello-world HTTP server in Go" # one-shot, WS
```

`codex-demo` runs Codex with a clean `CODEX_HOME` under `$TMPDIR`, points
Codex at `codex-model-catalog.json`, and disables Codex-side OpenAI auth for
the Orange provider. That avoids inheriting local skills/plugins that can
trigger startup budget warnings and avoids the fallback model metadata
warning. It also disables Codex shell snapshots so local shell startup files
cannot fail the transport demo before a model request is sent. Passing
`--ws` also enables Codex's `responses_websockets` feature and sets
`model_providers.orange.supports_websockets=true`.

In interactive mode, Codex may prewarm the WebSocket before it has a turn
to send; the Orange log will show `waiting for first client frame` until the
first prompt is submitted. Codex may also send a `generate:false`
`response.create` warmup before the real user turn; Orange completes that
warmup locally and keeps the WebSocket open for the next turn instead of
forwarding it upstream. Orange waits up to 10 minutes for Codex's first
non-warmup `response.create` frame; override with
`ORANGE_RESPONSESWS_FIRST_FRAME_TIMEOUT` if needed. Orange still emits model
metadata from `orange.yaml` through `GET /v1/models` for clients that read
the OpenAI-compatible catalogue.

After the first prompt, `curl -s localhost:9901/stats | grep orange_` should
show non-zero `orange_input_tokens` and `orange_output_tokens`, confirming
`orange-meter` saw the response.

### Codex demo troubleshooting

Validate that Codex is reading the Orange demo model catalogue without
starting an interactive session:

```bash
ORANGE_CODEX_HOME="$(mktemp -d)" ./codex-demo debug models \
  | jq '.models[] | select(.slug == "gpt-4o-mini") |
        {slug, context_window, max_context_window, supports_parallel_tool_calls}'
```

That command should print the `gpt-4o-mini` entry from
`codex-model-catalog.json` and should not print the fallback metadata
warning. If you still see skill/plugin budget warnings, make sure you are
using `./codex-demo`; running `codex …` directly loads your normal
`$CODEX_HOME`.

Validate the Codex WebSocket path with Codex-side WS tracing enabled:

```bash
ORANGE_CODEX_HOME="$(mktemp -d)" ORANGE_CODEX_TRACE=1 \
  ./codex-demo --ws exec --json "reply with exactly: orange responsesws ok"
```

The Orange server log should show these checkpoints for the same
`orange-responsesws` session:

```text
orange-responsesws: client accepted
orange-responsesws: reading first client frame
orange-responsesws: first client frame received
orange-responsesws: model provider resolved
orange-responsesws: egress websocket connected
orange-responsesws: first client frame forwarded
orange-responsesws: pump egress->client read frame
```

## Diagnostics

```bash
# Per-host TLS handshakes — both providers should tick after their first curl;
# cx_connect_fail should stay 0.
curl -s localhost:9901/clusters | grep orange_default:: | grep -E 'cx_(total|connect_fail)'

# Token usage counters emitted by orange-meter.
curl -s localhost:9901/stats | grep orange_

# OpenAI-compatible model list served by Orange.
curl -s localhost:8080/v1/models | jq '.data[] | select(.id == "gpt-4o-mini")'

# Upstream conns opened through the dynamic cluster.
curl -s localhost:9901/clusters | grep orange_default:: | grep cx_total
```

Error paths handled by `match` (no upstream contacted):

```bash
# Missing model field → 400 orange.model_required
curl -i localhost:8080/v1/chat/completions -H 'content-type: application/json' -d '{}'
curl -i localhost:8080/v1/responses        -H 'content-type: application/json' -d '{}'

# Unknown model → 404 orange.model_not_found
curl -i localhost:8080/v1/chat/completions -H 'content-type: application/json' \
  -d '{"model":"gemini-1.5"}'
```

## Tests

```bash
make test     # unit tests (config, match, adapt, pick, meter, mcp, responsesws, translator)
make build    # cgo c-shared build of liborange.so
make e2e      # end-to-end tests against live providers (requires API keys)
```

## Layout

```
internal/
  config/        orange.yaml loader (secret_ref resolution, polling, MCP)
  pipeline/
    match/       downstream filter: body parse + StreamPromise resolve
    pick/        cluster extension: async ChooseHost + STRICT_DNS host refresh
    adapt/       upstream filter: translator pipeline + auth injection
    meter/       upstream filter: token counting → Envoy counters + metadata
    mcp/         /mcp sidecar: session encryption, profile fan-out, SSE mux,
                 egress filter that maps backend → pick host
    responsesws/ /v1/responses WebSocket sidecar + meter bridge
  translator/    wire-format translators (openai↔openai/azure/bedrock/vertex/groq/deepinfra)
  apischema/     API schema types (openai, anthropic, gcp, awsbedrock)
  send/          JSON error-response helpers
  observability/ slog wiring + Envoy access-log shaping
  debug/         embedded admin HTTP server
codemod/         source-to-source migration tool (OpenAI SDK → orange endpoints)
cmd/             c-shared entrypoint (blank-imports all pipeline packages)
envoy.tmpl.yaml  Envoy config; ${ORANGE_TRUSTED_CA} substituted by make
orange.yaml      runtime config (providers, models, MCP servers/profiles)
codex-demo       Codex CLI wrapper that targets Orange
mcp-demo         curl wrapper for the /mcp endpoint
```
