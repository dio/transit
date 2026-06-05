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
   POST   /mcp[/profile|/s/x] │                                                               ┌─► kiwi
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
table, one TLS context, many possible upstream hostnames. Per-host TLS
SNI is derived automatically from each registered host's `Hostname`
field. No `transport_socket_matches`. No cluster per provider.

## Everything via Envoy

A guiding principle: **every byte in or out of Orange flows through
Envoy.** That includes the things it's tempting to short-circuit:

- **Ingress** — `/v1/*`, `/mcp/*`, the Responses WebSocket — is a normal
  Envoy listener with HCM, filters, access logs, and upgrade configs.
- **Egress** — provider HTTPS and MCP backend HTTPS — exits through
  Envoy clusters, even though Orange does its own DNS resolution and
  host selection in `pick`. The sidecars (`orange-mcp`,
  `orange-responsesws`) loop back through Envoy via a loopback cluster
  rather than punching out directly with a Go `http.Client`.
- **Metrics** — token usage isn't a parallel telemetry pipeline. The
  `orange-meter` filter emits `orange_input_tokens` /
  `orange_output_tokens` as Envoy counters and dynamic metadata, so
  they show up on `/stats`, `/clusters`, and any `stats_sinks` you wire
  up (statsd, OTLP, Prometheus) — alongside Envoy's own counters.
- **Logs** — request/response logs come out of Envoy access logs, with
  the `mcp_method` / `mcp_tool` / `orange_meter.*` fields populated
  through dynamic metadata that the filters write. No separate Orange
  log stream to ship.
- **Traces** — when Envoy tracing is configured, every Orange span is
  an Envoy span. The filters add tags via dynamic metadata; Envoy
  emits them through whatever tracer is wired up.

The payoff is operational uniformity: one binary to deploy, one
config surface for ingress/egress/observability, one set of dashboards.
The cost is that every new capability has to fit Envoy's filter and
cluster model — which is exactly why `pick` looks like an LB extension
and `match`/`adapt`/`meter` look like HTTP filters rather than some
bespoke Orange runtime.

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
the router has already locked SNI by the time the body arrives. See
[`internal/pipeline/pick/README.md`](internal/pipeline/pick/README.md)
for the gory phase-ordering details.

The DNS refresh loop is STRICT_DNS-shaped: it wakes at the earliest TTL
across all registered hostnames, re-resolves, and reconciles the host set in
place — keeping existing `HostPtr`s when the IP set is unchanged (so the
round-robin counter doesn't reset), only churning when an IP actually
appears or disappears. A transient DNS failure never evicts a healthy host.

### The Envoy patch this depends on

Stock Envoy *almost* makes this work, but two gaps prevent it from being
correct in production. Both are closed by a private patch maintained
alongside Orange:

[**`auto-host-sni-bounded-sni-session-cache.patch`**](https://gist.github.com/dio/965d1e555909c02013ca882a2b3caa78)

It does two things:

1. **Dynamic-module hostnames for `auto_host_sni`.** The legacy
   add-hosts ABI accepted only an address, so hosts created at runtime
   ended up with a synthesized hostname like `<cluster>+<addr>` — which
   `auto_host_sni` then dutifully used as the (wrong) SNI. The patch
   adds `envoy_dynamic_module_callback_cluster_add_hosts_with_hostnames`
   so `pick.AddHosts` can supply the *logical* hostname (e.g.
   `api.openai.com`) that `Upstream::HostDescription::hostname()`
   returns, and that `auto_host_sni` / `auto_sni_san_validation` read at
   handshake time. No `transport_socket_matches`, no xDS per host.
2. **Bounded, SNI-scoped upstream TLS session cache.** Envoy's default
   upstream session cache is keyed at `ClientContextImpl` scope. With a
   shared `UpstreamTlsContext` that talks to many SNI names, that means
   a session issued for `api.openai.com` could be offered when
   connecting to `api.anthropic.com` — incorrect, and in practice a
   handshake/verify failure waiting to happen. The patch replaces the
   single deque with an LRU keyed by *effective SNI* (bounded to 128
   distinct names, one most-recent session per name), so resumption
   never crosses an SNI boundary.

The patch also includes the router-side fix needed for async host
selection: `TransportSocketOptions` is rebuilt after `ChooseHost`
resolves, so filter-state-driven socket options written during body
processing still reach the upstream handshake. Without that fix the
`ClusterLBCompletion` pattern `pick` uses can lose its TSO between
header phase and connection-pool open.

It applies cleanly to Envoy `0d6e3c60aa55e434f28e581df1d25fcb83404b68`
and ships with unit coverage (`ClientSessionCacheIsScopedBySni`,
`ClientSessionCacheEvictsLeastRecentlyUsedSniAfterHardcodedBound`, plus
ABI tests for the new hostname entrypoint). The intended upstream shape
is to surface the cache bounds in the TLS `.proto` — this patch
hardcodes 128 to keep the diff small while validating the design.

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

Supported `auth.type` values: `bearer`, `x-api-key`, `anthropic`, `gemini`,
`aws`, `gcp`.
Supported `backend_schema` values (override `kind` for the wire translator):
`azureopenai`, `awsbedrock`, `awsanthropic`, `gcpvertexai`, `gcpanthropic`.

### Provider credentials

Each provider in `orange.yaml` declares how it obtains credentials. The table
below lists what you need to set before running Orange with that provider.

| Provider type | `auth.type` | What to supply | Notes |
|---|---|---|---|
| OpenAI, any bearer-token API | `bearer` | `secret_ref: env://OPENAI_API_KEY` | Any env var name; token injected as `Authorization: Bearer …`. |
| Anthropic direct | `anthropic` | `secret_ref: env://ANTHROPIC_API_KEY` | Injects `x-api-key` + `anthropic-version` header. |
| Google Generative Language API (`generativelanguage.googleapis.com`) | `gemini` | `secret_ref: env://GEMINI_API_KEY` | Injects `x-goog-api-key` header. No project/location needed. |
| GCP Vertex AI (Gemini or Anthropic via rawPredict) | `gcp` | `secret_ref: env://GCP_SERVICE_ACCOUNT_JSON` **or** `secret_ref: file:///path/to/key.json` **or** nothing | Three credential modes: (1) `env://` — env var holds the full service-account JSON string; (2) `file://` — absolute path to a key file, passed directly to the GCP SDK as `CredentialsFile`; (3) no `secret_ref` — [Application Default Credentials](https://cloud.google.com/docs/authentication/application-default-credentials) (`GOOGLE_APPLICATION_CREDENTIALS`, Workload Identity, gcloud user creds). |
| AWS Bedrock / Bedrock Anthropic | `aws` | `AWS_ACCESS_KEY_ID` + `AWS_SECRET_ACCESS_KEY` + `AWS_REGION` in environment | No `secret_ref`; credentials come from the AWS SDK default chain (env vars, `~/.aws/credentials`, instance role). `extra.aws_region` is still required in `orange.yaml` for SigV4 signing. |

#### Vertex AI provider example

```yaml
llm:
  providers:
    # Gemini via Google Generative Language API (API key, no project needed)
    gemini:
      kind: openai
      backend_schema: gcpvertexai
      endpoint: https://generativelanguage.googleapis.com
      auth:
        type: gemini
        secret_ref: env://GEMINI_API_KEY

    # Gemini via Vertex AI (service account or ADC)
    vertex:
      kind: openai
      backend_schema: gcpvertexai
      endpoint: https://us-central1-aiplatform.googleapis.com
      extra:
        gcp_project: env://GCP_PROJECT   # resolved at load time
        gcp_location: us-central1
      auth:
        type: gcp
        secret_ref: env://GCP_SERVICE_ACCOUNT_JSON   # env var holds SA JSON — or:
        # secret_ref: file:///path/to/key.json       # path passed directly to GCP SDK
        # (omit secret_ref entirely to fall back to ADC)

    # Anthropic claude-* via Vertex AI rawPredict
    vertex_anthropic:
      kind: anthropic
      backend_schema: gcpanthropic
      endpoint: https://us-east5-aiplatform.googleapis.com
      extra:
        anthropic_version: "vertex-2023-10-16"       # required by Vertex; differs from direct Anthropic
        gcp_project: env://GCP_PROJECT   # resolved at load time
        gcp_location: us-east5
      auth:
        type: gcp
        secret_ref: env://GCP_SERVICE_ACCOUNT_JSON   # or file:///path/to/key.json, or omit for ADC
  models:
    gemini-2.5-flash:
      provider: gemini
    vertex/gemini-2.5-flash:
      provider: vertex
      name: gemini-2.5-flash
    vertex/claude-opus-4:
      provider: vertex_anthropic
      name: claude-opus-4@20250514
```

> **Vertex `anthropic_version`** must be `"vertex-2023-10-16"`, not the
> `"2023-06-01"` header used by the direct Anthropic API. Orange does not
> override this automatically; the correct value must be in `extra`.

> **Vertex Anthropic via `/v1/messages`** is routed through the
> `gcpanthropic:messages` translator, which builds the `rawPredict` /
> `streamRawPredict` path, injects `anthropic_version`, and removes the
> `model` field from the body (Vertex rejects requests that include it —
> the model is encoded in the URL path only).

#### AWS Bedrock provider example

```yaml
llm:
  providers:
    bedrock:
      kind: openai
      backend_schema: awsbedrock
      endpoint: https://bedrock-runtime.us-east-1.amazonaws.com
      extra:
        aws_region: us-east-1      # must match the endpoint region; used for SigV4
      auth:
        type: aws                  # no secret_ref; reads AWS_* env vars / IAM role
  models:
    amazon.nova-lite-v1:0:
      provider: bedrock
    amazon.nova-micro-v1:0:
      provider: bedrock
```

Set before running:

```bash
export AWS_ACCESS_KEY_ID=AKIA…
export AWS_SECRET_ACCESS_KEY=…
export AWS_REGION=us-east-1        # or AWS_DEFAULT_REGION
```

Orange delegates entirely to the AWS SDK default credential chain — IAM
instance roles, `~/.aws/credentials`, and the env vars above all work.
`extra.aws_region` is still required so Orange can scope the SigV4
signature to the right region.

> **SigV4 service name**: Bedrock Runtime's signing service is `bedrock`
> (not `bedrock-runtime`); the hostname uses `bedrock-runtime` but the
> credential scope must say `bedrock`.

## Routing: fallback chains and traffic splits

Beyond the flat `model → provider` mapping in `llm.models`, Orange supports
per-key routing policies through the `keys:` block in `orange.yaml`. A key
identifies a caller (workspace + user), and any model it references can carry
a `routing:` override that replaces the global target.

Two routing modes are available:

### Fallback chain (`routing.chain`)

Tries targets left-to-right, retrying on failure. The `chain.retry` block is
translated by `orange-match` into `x-envoy-retry-on`,
`x-envoy-max-retries`, and `x-envoy-upstream-rq-per-try-timeout-ms` headers
before the request reaches Envoy's router. The route's `retry_policy` in
`envoy.tmpl.yaml` (and in EG, `BackendTrafficPolicy`) acts as the **floor**
that enables `RetryStateImpl`; orange drives the actual retry count and timeout
per-request via those injected headers.

```yaml
keys:
  demo/maya/sk-fallback:
    workspace: demo
    user: maya
    llm:
      models:
        claude-haiku-4-5:
          routing:
            chain:
              retry:
                retry_on: "connect-failure,reset,5xx,retriable-status-codes"
                per_try_timeout_ms: 10000
              children:
                - target: { provider: fallback_p1, name: claude-haiku-4-5 }
                - target: { provider: fallback_p2, name: claude-haiku-4-5 }
                - target: { provider: fallback_p3, name: claude-haiku-4-5 }
                - target: { provider: vertex_anthropic, name: claude-opus-4@20250514 }
```

`fallback_p1/p2/p3` are RFC 5737 TEST-NET addresses that time out cleanly;
every request rolls through them and lands on `vertex_anthropic`. In
production, replace the dead addresses with real primaries (other providers,
regions, or API key pools) and keep the last child as the proven fallback.

> **Retry floor in Envoy Gateway** — Without a base `retry_on` on the route,
> Envoy discards `x-envoy-retry-on` from the upstream filter even when the
> header is present. The `envoy.tmpl.yaml` route already has:
>
> ```yaml
> retry_policy:
>   retry_on: "5xx,gateway-error,reset,connect-failure,refused-stream,retriable-status-codes"
>   retriable_status_codes: [429]
>   num_retries: 7
>   per_try_timeout: 30s
> ```
>
> In EG, express this through `BackendTrafficPolicy` — see
> [Envoy Gateway deployment](#envoy-gateway-eg-deployment) below.

### Traffic split (`routing.split`)

Assigns each request to one child by weighted random selection. Weights are
integers; Orange normalises them. A split with `34/33/33` directs roughly a
third of traffic to each of three Vertex AI endpoints, useful for gradual
roll-outs, A/B model comparisons, or spreading load across quota pools.

```yaml
keys:
  demo/maya/sk-split:
    workspace: demo
    user: maya
    llm:
      models:
        claude-haiku-4-5:
          routing:
            split:
              children:
                - weight: 34
                  target: { provider: split_p1, name: claude-haiku-4-5 }
                - weight: 33
                  target: { provider: split_p2, name: claude-haiku-4-5 }
                - weight: 33
                  target: { provider: split_p3, name: claude-haiku-4-5 }
```

Split selection is stateless per-request — there is no session affinity and no
retry interaction. If one `split_p*` returns an error the request fails; wrap
a `chain` around the split (not shown here) if you need combined
split-then-fallback semantics.

### Composing chain and split

Setting both `routing.chain` and `routing.split` on the **same node** is a
config validation error — `Load` rejects it immediately with:

```
orange/config: keys[...].llm.models[claude-haiku-4-5].routing:
    must set exactly one of chain, target, or split
```

`RoutingNode` is a union, not a struct with two optional sub-fields. Each
node in the routing tree carries exactly one of the three. That invariant is
enforced by `validateRoutingNodeInner` (`config.go`) before the config snapshot
is accepted.

The interesting cases are **compositions across tree levels**, and those are
fully supported:

#### Split whose children are chains (split-then-fallback)

Each weighted arm is itself a fallback chain. The split fires first — it
samples one arm for the request — and if that arm's primary fails, the chain's
retry policy kicks in and walks down to the next target in that arm.

```yaml
claude-haiku-4-5:
  routing:
    split:
      children:
        - weight: 60
          chain:
            retry:
              retry_on: "connect-failure,reset,5xx,retriable-status-codes"
              per_try_timeout_ms: 10000
            children:
              - target: { provider: vertex_anthropic, name: claude-haiku-4-5-20251001 }
              - target: { provider: anthropic,        name: claude-haiku-4-5-20251001 }
        - weight: 40
          chain:
            retry:
              retry_on: "connect-failure,reset,5xx,retriable-status-codes"
              per_try_timeout_ms: 10000
            children:
              - target: { provider: split_p1, name: claude-haiku-4-5 }
              - target: { provider: split_p2, name: claude-haiku-4-5 }
```

60 % of requests go to the `vertex_anthropic → anthropic` chain; 40 % go
to the `split_p1 → split_p2` chain. Each chain can retry independently.

> **Nested split is blocked.** A split child cannot itself be a `split` node —
> `validateRoutingNodeInner` passes an `insideSplit=true` flag to child
> validation and returns an error if it finds another split. This prevents
> probabilistic trees whose depth and behaviour are hard to reason about.

#### Chain whose children are splits (each fallback slot draws from a pool)

Each position in the chain is a split. On the first attempt the split at
position 0 samples one provider; on retry 1 Envoy advances the attempt counter,
`pick` reads `orange.adapt.attempt`, and the split at position 1 samples one
provider from the next pool.

```yaml
claude-haiku-4-5:
  routing:
    chain:
      retry:
        retry_on: "connect-failure,reset,5xx,retriable-status-codes"
        per_try_timeout_ms: 10000
      children:
        - split:
            children:
              - weight: 50
                target: { provider: vertex_anthropic, name: claude-haiku-4-5-20251001 }
              - weight: 50
                target: { provider: split_p1, name: claude-haiku-4-5 }
        - split:
            children:
              - weight: 50
                target: { provider: split_p2, name: claude-haiku-4-5 }
              - weight: 50
                target: { provider: split_p3, name: claude-haiku-4-5 }
        - target: { provider: anthropic, name: claude-haiku-4-5-20251001 }
```

The first attempt draws from `{vertex_anthropic, split_p1}` with equal
weight. If it fails, the retry draws from `{split_p2, split_p3}`. If that
also fails, the last resort is `anthropic` directly. Each split selection is
independent — there is no affinity between attempts.

> **Chain-of-chain is blocked at runtime.** `resolveRouting` (`match.go`)
> checks each chain child and returns an error if any child is itself a chain.
> The validator does not catch this at load time (chain-inside-chain is a
> structural gap, not a schema error), so the request gets a 404
> `orange.model_not_found` at body-parse time. Don't nest chains; use a
> flat chain with more children instead.

### Using a key

Pass the key as the `Authorization: Bearer` value:

```bash
curl -s localhost:8080/v1/messages \
  -H 'content-type: application/json' \
  -H 'authorization: Bearer demo/maya/sk-fallback' \
  -d '{"model":"claude-haiku-4-5","max_tokens":32,
       "messages":[{"role":"user","content":"hi"}]}'
```

Orange resolves the key, applies the routing override for that
`(key, model)` pair, and the rest of the pipeline (adapt, meter, pick) runs
unchanged.

## Quickstart

```bash
# Required for the default orange.yaml providers
export OPENAI_API_KEY=sk-...
export ANTHROPIC_API_KEY=sk-ant-...
export GITHUB_TOKEN=$(gh auth token)   # MCP github backend

# GCP providers — pick one or both:
export GEMINI_API_KEY=AIza...          # Gemini via generativelanguage.googleapis.com
export GCP_SERVICE_ACCOUNT_JSON=$(cat my-sa-key.json)  # Vertex AI: env var holds SA JSON
# Alternatives for GCP credentials:
#   secret_ref: file:///abs/path/to/key.json  (path passed directly to GCP SDK — no env var needed)
#   omit secret_ref entirely and rely on ADC:
#     gcloud auth application-default login
#     export GOOGLE_APPLICATION_CREDENTIALS=/path/to/key.json

# AWS Bedrock
export AWS_ACCESS_KEY_ID=AKIA...
export AWS_SECRET_ACCESS_KEY=...
export AWS_REGION=us-east-1

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

# Gemini via Google Generative Language API (API key)
curl -s localhost:8080/v1/chat/completions -H 'content-type: application/json' \
  -d '{"model":"gemini-2.5-flash","messages":[{"role":"user","content":"hi"}]}'

# Gemini via Vertex AI (service account / ADC)
curl -s localhost:8080/v1/chat/completions -H 'content-type: application/json' \
  -d '{"model":"vertex/gemini-2.5-flash","messages":[{"role":"user","content":"hi"}]}'

# Anthropic claude via Vertex AI rawPredict — OpenAI Chat Completions path
curl -s localhost:8080/v1/chat/completions -H 'content-type: application/json' \
  -d '{"model":"vertex/claude-opus-4","messages":[{"role":"user","content":"hi"}]}'

# Anthropic claude via Vertex AI rawPredict — native Anthropic Messages path
curl -s localhost:8080/v1/messages -H 'content-type: application/json' \
  -d '{"model":"vertex/claude-opus-4","max_tokens":64,
       "messages":[{"role":"user","content":"hi"}]}'

# AWS Bedrock — Amazon Nova Lite
curl -s localhost:8080/v1/chat/completions -H 'content-type: application/json' \
  -d '{"model":"amazon.nova-lite-v1:0","messages":[{"role":"user","content":"hi"}]}'

# AWS Bedrock — streaming
curl -N -s localhost:8080/v1/chat/completions -H 'content-type: application/json' \
  -d '{"model":"amazon.nova-lite-v1:0","stream":true,
       "messages":[{"role":"user","content":"count 1 to 5"}]}'
```

### LLM demos

`demos/llm` is a curl wrapper for all three LLM API paths.

```bash
# Chat Completions (default, gpt-4o-mini)
./demos/llm
./demos/llm "count 1 to 5"
./demos/llm --stream "count 1 to 5"

# Responses API
./demos/llm --api responses "hi"
./demos/llm --api responses --stream "count 1 to 5"

# Anthropic Messages
./demos/llm --api messages "hi"
./demos/llm --api messages --stream "count 1 to 5"

# Different model
./demos/llm --model amazon.nova-lite-v1:0 "hi"
./demos/llm --model gemini-2.5-flash --stream "count 1 to 5"

# List models served by Orange
./demos/llm models
```

Useful overrides:

```bash
ORANGE_LLM_BASE_URL=http://localhost:8080
ORANGE_LLM_SHOW_HEADERS=1   # or pass --headers
```

### Image generation demos

`demos/images` is a curl wrapper for `/v1/images/generations`.

```bash
# Default: DALL-E 3, one image, default prompt
./demos/images

# Custom prompt
./demos/images "a sunset over the ocean"

# DALL-E 3, HD quality, landscape size
./demos/images --model dall-e-3 --quality hd --size 1792x1024 "a mountain landscape"

# GPT Image 1 — save to file
./demos/images --model gpt-image-1 --output /tmp/out.png "a neon city at night"

# GPT Image 1 — save all images to a directory
./demos/images --model gpt-image-1 --save /tmp "a robot reading a book"

# Gemini image generation
./demos/images --model gemini-2.5-flash-image "a cartoon cat"

# DALL-E 2, multiple images
./demos/images --model dall-e-2 --n 2 "a red apple"

# Extra parameters (e.g. transparent background)
./demos/images --model gpt-image-1 --param background=transparent "a logo"

# List image-capable models
./demos/images models
```

Useful overrides:

```bash
ORANGE_LLM_BASE_URL=http://localhost:8080
ORANGE_LLM_SHOW_HEADERS=1   # or pass --headers
```

### MCP traffic

`demos/mcp` is a curl wrapper for the Orange MCP endpoint. It captures the
public `mcp-session-id` from `initialize` and then drives `tools/list`,
`tools/call`, SSE stream, and `DELETE`.

```bash
# Initialize the default profile and print the export line for ORANGE_MCP_SESSION_ID.
./demos/mcp profile=default
export ORANGE_MCP_SESSION_ID='<session printed by initialize>'

./demos/mcp profile=default list
./demos/mcp profile=default call kiwi__search-flight \
    '{"flyFrom":"SFO","flyTo":"JFK","departureDate":"10/06/2026"}'
./demos/mcp profile=default stream
./demos/mcp profile=default delete

# Target a single MCP server directly with the s/<name> shorthand.
./demos/mcp server=github
./demos/mcp server=github list

# See all request/response headers for debugging.
./demos/mcp --headers profile=default
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
./demos/codex                              # interactive, HTTP
./demos/codex --ws                         # interactive, Responses WebSocket
./demos/codex exec "write a hello-world HTTP server in Go"      # one-shot
./demos/codex --ws exec "write a hello-world HTTP server in Go" # one-shot, WS
```

`demos/codex` runs Codex with a clean `CODEX_HOME` under `$TMPDIR`, points
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
ORANGE_CODEX_HOME="$(mktemp -d)" ./demos/codex debug models \
  | jq '.models[] | select(.slug == "gpt-4o-mini") |
        {slug, context_window, max_context_window, supports_parallel_tool_calls}'
```

That command should print the `gpt-4o-mini` entry from
`codex-model-catalog.json` and should not print the fallback metadata
warning. If you still see skill/plugin budget warnings, make sure you are
using `./demos/codex`; running `codex …` directly loads your normal
`$CODEX_HOME`.

Validate the Codex WebSocket path with Codex-side WS tracing enabled:

```bash
ORANGE_CODEX_HOME="$(mktemp -d)" ORANGE_CODEX_TRACE=1 \
  ./demos/codex --ws exec --json "reply with exactly: orange responsesws ok"
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

## Claude Code CLI support

Orange serves the Anthropic Messages API at `/v1/messages` and routes to the
configured Anthropic (or Vertex Anthropic) backend based on the `model` field.
`demos/claude` sets `ANTHROPIC_BASE_URL` to point Claude Code at Orange and
supplies a placeholder `ANTHROPIC_API_KEY` so `--bare` auth does not bail out
before the first request — Orange does not validate the client-side key.

**Terminal 1** — start the proxy:

```bash
make demo
```

**Terminal 2** — run Claude Code through Orange:

```bash
./demos/claude                         # interactive
./demos/claude -p "write hello world in Go"   # one-shot (--print)
```

Useful overrides:

```bash
ORANGE_CLAUDE_BASE_URL=http://localhost:8080   # default
ORANGE_CLAUDE_MODEL=haiku                     # default alias; must resolve to a model in orange.yaml
```

## Goose AI agent support

Orange serves an OpenAI-compatible Chat Completions endpoint at
`/v1/chat/completions`. `demos/goose` sets `GOOSE_PROVIDER=openai`,
`OPENAI_BASE_URL`, and a placeholder `OPENAI_API_KEY` — Goose requires the
key to be non-empty even though Orange ignores it on inbound requests.

**Terminal 1** — start the proxy:

```bash
make demo
```

**Terminal 2** — run Goose through Orange:

```bash
./demos/goose session                                              # interactive
./demos/goose run --no-session --text "write hello world in Go"   # one-shot
```

Useful overrides:

```bash
ORANGE_GOOSE_BASE_URL=http://localhost:8080   # default
ORANGE_GOOSE_MODEL=gpt-4o-mini               # default; must be in orange.yaml
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

### Fallback and split routing

```bash
# Fallback chain: sk-fallback key → three dead TEST-NET primaries → vertex_anthropic.
# Envoy access log shows response_flags: "UF,URX" on the first three attempts,
# then a 200 from vertex_anthropic on the fourth.
curl -s localhost:8080/v1/messages \
  -H 'content-type: application/json' \
  -H 'authorization: Bearer demo/maya/sk-fallback' \
  -d '{"model":"claude-haiku-4-5","max_tokens":32,
       "messages":[{"role":"user","content":"hi"}]}'

# Traffic split: sk-split key → 34/33/33 across split_p1/p2/p3.
# Run several times; the access log's provider_backend field rotates across the three.
for i in $(seq 6); do
  curl -s localhost:8080/v1/messages \
    -H 'content-type: application/json' \
    -H 'authorization: Bearer demo/maya/sk-split' \
    -d '{"model":"claude-haiku-4-5","max_tokens":8,
         "messages":[{"role":"user","content":"hi"}}' | jq -r '.content[0].text'
done
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

## Envoy Gateway (EG) deployment

When running orange behind [Envoy Gateway](https://gateway.envoyproxy.io/) instead of a
hand-rolled `envoy.yaml`, the timeout and buffer settings above must be expressed
through EG's policy CRDs. The raw Envoy equivalents are:

| Envoy field | Raw value | EG CRD |
|---|---|---|
| route `timeout` (default `"/"` route) | `900s` (Envoy proto requires `s` suffix) | `BackendTrafficPolicy` → `timeout.http.requestTimeout` |
| listener `per_connection_buffer_limit_bytes` | `104857600` (100 MiB) | `ClientTrafficPolicy` → `connection.bufferLimit` |
| cluster `per_connection_buffer_limit_bytes` | `104857600` (100 MiB) | `BackendTrafficPolicy` → `connection.bufferLimit` |

### BackendTrafficPolicy — timeout + upstream buffer

Apply once per `HTTPRoute` that carries LLM traffic (the catch-all `"/"` route in
`envoy.tmpl.yaml`). WS and MCP routes already use `timeout: 0s` and are unaffected.

```yaml
apiVersion: gateway.envoyproxy.io/v1alpha1
kind: BackendTrafficPolicy
metadata:
  name: orange-llm
  namespace: <your-namespace>
spec:
  targetRefs:
    - group: gateway.networking.k8s.io
      kind: HTTPRoute
      name: orange-llm          # adjust to your HTTPRoute name
  timeout:
    http:
      requestTimeout: 15m       # EG accepts Go duration strings; raw Envoy uses 900s
  connection:
    bufferLimit: 100Mi          # matches per_connection_buffer_limit_bytes on clusters
```

### BackendTrafficPolicy — retry floor for fallback chains

`orange-match` injects `x-envoy-retry-on`, `x-envoy-max-retries`, and
`x-envoy-upstream-rq-per-try-timeout-ms` per-request when the key's routing
policy includes a `chain.retry` block. Envoy only honours these
`x-envoy-*` retry headers when a base `retry_on` is already present on the
route — without it `RetryStateImpl` is never initialised and the injected
headers are silently ignored.

Add a second `BackendTrafficPolicy` (or extend the existing one) to establish
the retry floor for the fallback-chain route:

```yaml
apiVersion: gateway.envoyproxy.io/v1alpha1
kind: BackendTrafficPolicy
metadata:
  name: orange-llm-retry
  namespace: <your-namespace>
spec:
  targetRefs:
    - group: gateway.networking.k8s.io
      kind: HTTPRoute
      name: orange-llm          # adjust to your HTTPRoute name
  retry:
    retryOn:
      triggers:
        - connect-failure
        - reset
        - retriable-5xx
        - retriable-status-codes
    retriableStatusCodes: [429]
    numRetries: 7               # ceiling; orange-match overrides per-request
    perRetry:
      timeout: 30s              # ceiling; orange-match overrides per-request via
                                #   x-envoy-upstream-rq-per-try-timeout-ms
      backOff:
        baseInterval: 100ms
        maxInterval: 1s
```

> `numRetries` and `perRetry.timeout` here are ceilings, not the actual
> per-chain values — orange-match overwrites them with the chain's
> `per_try_timeout_ms` and the child count via `x-envoy-*` headers. The floor
> only needs to be high enough to accommodate the longest chain you expect to
> configure; the demo `sk-fallback` key has four children so `numRetries: 7`
> is comfortably above the three retries that chain requires.

The split routing mode (`routing.split`) does not interact with Envoy's retry
machinery at all — child selection happens inside `orange-match` before a
single upstream attempt, so no retry policy changes are needed for split.

### ClientTrafficPolicy — downstream buffer

Apply once per `Gateway` listener to raise the per-connection downstream buffer from
Envoy's 1 MiB default to 100 MiB (needed for large image-generation responses and
long streaming completions).

```yaml
apiVersion: gateway.envoyproxy.io/v1alpha1
kind: ClientTrafficPolicy
metadata:
  name: orange
  namespace: <your-namespace>
spec:
  targetRefs:
    - group: gateway.networking.k8s.io
      kind: Gateway
      name: orange              # adjust to your Gateway name
  connection:
    bufferLimit: 100Mi          # matches per_connection_buffer_limit_bytes on listeners
```

> **Why these defaults aren't in EG out of the box** — Envoy's built-in route timeout
> is 15 seconds and its per-connection buffer is 1 MiB. Both are too small for LLM
> workloads: a single gpt-image-1 response can exceed 5 MiB, and reasoning models
> regularly take longer than 15 s to produce a first token.

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
demos/
  llm            curl wrapper for /v1/chat/completions, /v1/responses, /v1/messages
  images         curl wrapper for /v1/images/generations
  mcp            curl wrapper for the /mcp endpoint
  codex          Codex CLI wrapper that targets Orange
  claude         Claude Code CLI wrapper that targets Orange
  goose          Goose AI agent wrapper that targets Orange
  fallback/      isolated config for the fallback-chain demo (orange.yaml + envoy.tmpl.yaml)
  split/         isolated config for the traffic-split demo (orange.yaml + envoy.tmpl.yaml)
  tracing/
    validate     OpenInference span validator (unit + live modes)
```
