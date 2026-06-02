# Orange Pipeline SDK Research and Development Background

Status: working background for research and development, not an accepted API
specification.

Purpose: collect the constraints learned from `examples/orange`,
`examples/request-ui`, `examples/sse-tap`, `examples/ws-proxy`, and the Envoy
Gateway integrations so SDK research and implementation can proceed from one
shared model.

The core thesis is:

> Orange is a pipeline, not one filter.

The SDK should make that pipeline explicit: one shared configuration snapshot,
one typed request decision, one complete exchange record, explicit response
modes, bounded transport/TLS coordination, and protocol sidecars when HTTP
callbacks are not enough.

## How To Use This Document

Use this document as:

- **background** for SDK API discussions;
- **research backlog** for unresolved Envoy / Envoy Gateway / TLS questions;
- **development map** for turning repeated example-local patterns into SDK
  primitives;
- **review checklist** when new orange-like examples add another extension
  point or lifecycle hook.

Do not treat API sketches here as final names or signatures. They are meant to
show ownership boundaries and semantics first. Exact Go API shape should be
decided when implementing each development track.

## Research And Development Tracks

The work splits into parallel tracks.

| Track | Goal | First proof |
| --- | --- | --- |
| Pipeline config | One shared immutable config snapshot for all extension points. | Orange reads classify/hostpick/translate config from `PipelineConfig[T]`. |
| Dynamic provider resolution | Determine how far provider host and TLS handling can be data-plane dynamic. | Local e2e for dynamic hostnames with generic TLS / `auto_host_sni` if viable. |
| Rendezvous | Type-safe cross-phase request decisions. | Replace orange `pending` package with `StreamPromise[T]` + typed stream key. |
| Async host selection | SDK-owned `ClusterLBCompletion` scheduling/cancel pattern. | Replace orange hostpick cancellation map with `AsyncHostSelector[T]`. |
| Exchange observer | Request/response/finalized accumulator as reusable SDK pattern. | Port the `request-ui` accumulator shape into `up`. |
| Response modes | Make streaming observation vs buffered mutation explicit. | Validate with `sse-tap` and a buffered response mutation example. |
| Sidecars | Lifecycle helper for embedded protocol servers. | Rebuild `ws-proxy` registration/lifecycle on the helper. |
| EG transport integration | Bounded TLS/backend config coordinated with Envoy Gateway. | Compare `transport_socket_matches` EPP vs Backend + BackendTLSPolicy. |

Each track should produce:

- a small design note or update to this document;
- one focused SDK primitive or explicit decision not to add one;
- unit tests for SDK behavior;
- one example or integration proving the behavior against real Envoy when the
  behavior crosses the ABI or xDS boundary.

`examples/orange` routes OpenAI-compatible requests by the `model` field in
the request body. That makes it a useful stress test for the SDK because the
decision arrives after Envoy's router has already asked the cluster to pick an
upstream host.

Orange does not stop at host selection. A production LLM gateway also needs to
inspect and mutate request/response objects, gate requests by user headers or
policy, produce complete request-response records, tap streaming responses for
usage, and proxy protocols that Envoy's HTTP filter callbacks cannot observe
directly, such as WebSocket frames or MCP sessions. It also needs one
pipeline-wide configuration source shared by all of those extension points,
without forcing multi-megabyte business config through Envoy/xDS.

The current implementation works, but it forces application developers to
stitch together several SDK primitives mentally:

- downstream filter headers/body lifecycle
- per-stream object storage
- one-shot async result delivery
- `ClusterLBCompletion`
- cluster main-thread scheduling
- host-selection cancellation
- request metadata for later upstream filters

This document describes the current orange routing flow, then widens the SDK
proposal to cover the larger composition problem without baking orange-specific
logic into Transit.

## Current Flow

There are three modules in the orange shared library:

| Module | Extension point | Role |
| --- | --- | --- |
| `classify` | downstream HTTP filter | Parse the body, map `model` to a provider, publish the selected upstream. |
| `hostpick` | dynamic module cluster | Wait for classification, then complete async host selection. |
| `translate` | upstream HTTP filter | Read classification metadata and inject provider-specific auth/path headers. |

The important timing is:

```text
client
  -> classify.decodeHeaders
       creates a Pending and stores it in the stream-object bag
  -> router.decodeHeaders
       calls hostpick.ChooseHost before the request body is available
       ChooseHost returns a ClusterLBCompletion and waits on the Pending
  -> classify.decodeData / mutable body callback
       parses model, resolves Pending
  -> hostpick completion callback
       schedules onto the cluster main thread and completes host selection
  -> upstream HTTP filter chain
       translate reads dynamic metadata written by classify
```

### Headers Phase: Publish a Rendezvous Object

`classify.requestHandler` creates `*pending.Pending`, stores it in the filter
stream context for its own body callback, and publishes the same object to the
SDK stream-object bag:

```go
p := pending.New()
if r.Context != nil {
    *r.Context = &streamState{p: p}
}
w.SetStreamObject(StreamObjectKey, p)
```

The stream-object bag is the critical SDK primitive here. It lets an HTTP
filter attach a typed Go object to a stream and lets the cluster LB read the
same object later through `ClusterLBContext.GetStreamObject`.

This replaced the older token plus process-wide registry design. There is no
orange-owned global pending registry in the current code path.

### Host Selection: Wait Asynchronously

`hostpick.ChooseHost` reads the object from the stream bag:

```go
v, ok := ctx.GetStreamObject(classify.StreamObjectKey)
if !ok {
    return nil, nil
}
p := v.(*pending.Pending)
```

It cannot synchronously pick a host yet, because the body has not been parsed.
So it creates an async completion, registers a callback on the pending, and
returns the completion to Envoy:

```go
completion := ctx.NewCompletion()
p.OnResolve(func(res pending.Result) {
    l.owner.handle.Schedule(func() { l.complete(completion, res) })
})
return nil, completion
```

The `Schedule` call matters. The pending callback can fire from the downstream
worker path, from stream teardown, or inline when `ChooseHost` observes an
already-resolved pending. `ClusterLBCompletion.Complete` must happen from the
cluster main thread.

### Body Phase: Resolve the Decision

`classify.bodyHandler` parses the configured model field, looks up the provider,
updates request state, and resolves the pending:

```go
model := gjson.GetBytes(chunk.Data, field).String()
upstream := cfg.LookupModel(model)

w.SetFilterState(StateModel, model)
w.SetFilterState(StateUpstream, upstream)
w.SetMetadata(MetadataNamespace, MetadataKeyUpstream, upstream)
w.SetMetadata(MetadataNamespace, MetadataKeyModel, model)

st.p.Resolve(pending.Result{
    Provider: upstream,
    Kind:     prov.Kind,
    Model:    model,
})
```

Error paths resolve the pending with an error code before sending a local
response. `OnStreamComplete` also resolves the pending with
`orange.stream_terminated` if the stream ends before the body callback runs.
Because `Pending.Resolve` is first-writer-wins, body success, local errors, and
stream teardown can race safely.

### Upstream Filter: Read Metadata, Not the Pending

`translate` does not use the pending or talk to `hostpick`. It reads dynamic
metadata written by `classify`:

```go
name, ok := w.GetMetadataString(
    up.MetadataSourceDynamic,
    classify.MetadataNamespace,
    classify.MetadataKeyUpstream,
)
```

That split is useful: `pending.Pending` is a routing-phase rendezvous between
`classify` and `hostpick`; dynamic metadata is the stable communication channel
from downstream classification to later HTTP filters.

## Orange Is a Pipeline, Not One Filter

The body-driven routing problem is only one member of a growing set of
cross-phase workflows. The SDK should make those workflows composable instead
of forcing each example to build its own local conventions.

### Pipeline Configuration Plane

Orange needs one global configuration client for the pipeline, not separate
config objects per dynamic module extension point. "Global" here means
pipeline-wide and explicitly owned by the pipeline runtime, not an accidental
package global. The downstream filter, cluster extension, upstream filter,
response tap, exchange logger, and protocol sidecars should all read the same
active configuration snapshot.

That configuration may be large. Model catalogs, tenant policy, provider
profiles, BYOK placement, routing weights, tool/server catalogs, and audit
settings can easily exceed several megabytes. Encoding that into Envoy
`filter_config`, `cluster_config`, route metadata, or xDS resources is the
wrong operational boundary:

- Envoy config updates become large and noisy.
- Independent business config churn forces Envoy config churn.
- Every extension point gets its own partial view unless the user manually
  duplicates the same blob into multiple config locations.
- Rollback and last-good behavior become tied to Envoy config delivery instead
  of the gateway application's own control-plane semantics.

The SDK should support a pipeline-scoped config provider with an immutable
snapshot store:

```text
configuration API server
      |
      | poll / stream / file watch
      v
PipelineConfigProvider
      |
      | validate, normalize, index, eager-cache
      v
PipelineConfigSnapshot
      |
      +--> classify / gating
      +--> hostpick
      +--> translate
      +--> response tap
      +--> exchange logger
      +--> WebSocket / MCP sidecars
```

Request-path code should never fetch remote config. It should read the latest
published immutable snapshot and optional warmed indexes/caches. Refresh work
belongs in a background provider tied to pipeline lifetime.

Provider types should be pluggable:

- file-based for local development and deterministic tests
- HTTP polling for production operational sanity
- gRPC streaming for environments that want push delivery
- custom providers for embedded or test-controlled snapshots

Polling is likely the production default. It is debuggable, naturally bounded,
works through ordinary load balancers, and has clear failure semantics: keep
serving the last-good snapshot when the API server is slow, unavailable, or
publishes invalid data.

The provider should also support eager stashing. For example, a large config
document may reference many provider profiles, tenant policies, model matchers,
or tool definitions. The refresh path can precompute indexes and warm a bounded
cache, such as an LRU, so request callbacks do cheap lookups instead of parsing
or joining large structures on every stream.

The key rule is whole-snapshot publication: a request should see either the old
config or the new config, never a half-updated mix.

### Bounded Transport/TLS Configuration

For a standalone, orange-free version of this question, see
[Envoy Dynamic Modules And Dynamic Upstream TLS](dynamic-module-transport-tls-question.md).

The pipeline config plane should not be confused with Envoy's transport
configuration plane. Orange can keep the large provider/model/tenant catalog in
its own config snapshot, but upstream TLS origination still has to be expressed
in Envoy xDS somehow. The dynamic module can choose a host at request time; it
cannot synthesize arbitrary new `UpstreamTlsContext` resources out of band from
Envoy Gateway's controller.

That creates a second bounded-config problem:

- business config may contain thousands of logical models, profiles, tenants,
  tools, or routing rules;
- transport config should contain a bounded number of upstream TLS
  personalities that Envoy can own, validate, and rotate;
- the pipeline config maps many logical destinations onto those bounded
  transport personalities.

The ideal steady state is one logical Envoy route and, if possible, one
dynamic-module cluster. The cluster contains Go-owned hosts and request-time
selection stays inside Transit. The bounded Envoy Gateway config carries only
the transport identities that cannot be purely dynamic: SNI, SAN validation,
trust roots, client certs, and related TLS policy.

#### Option A: `transport_socket_matches` via EnvoyPatchPolicy

`integrations/cluster-async-router-eg` demonstrates this shape. The
`EnvoyPatchPolicy` replaces the EG-generated backend cluster with a
`CLUSTER_PROVIDED` dynamic-modules cluster and adds
`transport_socket_matches` entries:

```text
HostSpec.Metadata{"sni": "api.openai.com"}
      |
      v
transport_socket_matches[].match.sni
      |
      v
UpstreamTlsContext{sni, SAN validation, CA}
```

This is powerful because it keeps request-time host selection in Go and can
keep the number of Envoy clusters bounded, often to one. The cost is that the
TLS personalities are not fully dynamic: every distinct transport socket match
must exist in the cluster xDS resource. In Envoy Gateway today, that means the
Transit controller or operator has to coordinate with the EG controller through
`EnvoyPatchPolicy` or an equivalent xDS-producing integration.

Use this shape when:

- the pipeline strongly benefits from a single dynamic cluster;
- provider count is small or can be grouped into a bounded set of TLS
  personalities;
- per-host SNI/SAN/trust differs, but the transport variants are known at
  deployment time or change rarely;
- Transit needs direct control of `ClusterLB.ChooseHost`.

Avoid letting this grow into "one `transport_socket_match` per model" or "one
patch entry per customer". If the bounded set is actually unbounded, the design
has leaked business config back into xDS.

#### Option B: Envoy Gateway `Backend` + `BackendTLSPolicy`

If the number of upstream provider families is naturally bounded, Envoy
Gateway's `Backend` plus `BackendTLSPolicy` model is more natural. In that
shape, Envoy Gateway owns the upstream transport resources for each provider
family, and TLS policy lives in Gateway API resources instead of JSON patches.

Conceptually:

```text
Backend/openai      + BackendTLSPolicy/openai
Backend/anthropic   + BackendTLSPolicy/anthropic
Backend/bedrock     + BackendTLSPolicy/bedrock
```

The pipeline config snapshot still does not list every provider in Envoy. It
maps request decisions to one of the bounded backend families:

```text
model / tenant / policy decision
      |
      v
provider profile in pipeline snapshot
      |
      v
backend family key: openai | anthropic | bedrock | internal
```

This is appealing when:

- provider families are bounded and operationally meaningful;
- platform teams want TLS policy expressed as Kubernetes/Gateway API objects;
- certificate references, trust roots, SNI, and validation policy should be
  reconciled by Envoy Gateway rather than patched manually;
- cluster count stays acceptably bounded, even if it is more than one.

The tradeoff is that Transit must route to the right bounded backend/cluster
family. If Envoy Gateway materializes one cluster per `Backend`, then the
pipeline may need a bounded set of clusters instead of a single cluster. That
can still be correct. The goal is not dogmatic single-cluster purity; the goal
is to avoid unbounded cluster/config growth driven by business objects.

There is also an operational cost that applies to both this option and
`transport_socket_matches`: they are still Envoy configuration. Creating or
changing a `Backend`, `BackendTLSPolicy`, or transport socket match means the
pipeline controller must coordinate with the controller that produces xDS. That
is a second control channel next to the pipeline config provider.

That second channel may be acceptable for small, stable transport families, but
it is not ideal for provider catalog churn. The preferred design, if research
proves it viable, is:

- Envoy config carries one stable generic egress/TLS shape.
- Pipeline config carries provider hostnames and routing policy.
- Transit resolves provider hosts dynamically.
- Envoy derives SNI/SAN validation from the selected host or a request/stream
  value at connection time.
- No provider add/remove requires a Gateway/xDS update.

If that cannot be made safe, then Backend + BackendTLSPolicy or
`transport_socket_matches` should be treated as bounded fallback mechanisms,
not the primary dynamic-provider channel.

Concrete example: adding Cohere should ideally be a pipeline config update:

```text
providers:
  cohere:
    endpoint: https://api.cohere.com
    auth: ...
models:
  - match: command-*
    provider: cohere
```

If adding that provider also requires creating a Gateway API `Backend`, adding
a `BackendTLSPolicy`, or patching `transport_socket_matches`, then the operator
has to coordinate two control paths:

1. the orange pipeline config provider, so classify/hostpick/translate know
   Cohere exists; and
2. the Envoy Gateway/xDS path, so Envoy knows the Cohere transport/TLS
   personality.

Provider-family additions may be rare, but this is still hard to maintain:
rollout order matters, rollback spans two systems, debug state lives in two
places, and a partial rollout can classify traffic to a provider whose transport
identity Envoy cannot yet originate. The research goal is to make this
pipeline-config-only when the provider uses ordinary public TLS.

#### Why Bounded Matters

Unbounded Envoy config has bad operational properties:

- every model, tenant, or BYOK profile update becomes an xDS update;
- every provider add/remove may require coordination with the xDS-producing
  controller if provider TLS identity is represented as Backend/TLS policy or
  transport socket config;
- config dumps become huge and hard to debug;
- Envoy Gateway reconciliation gets coupled to application catalog churn;
- rollout and rollback blast radius grows with business config size;
- TLS policy ownership becomes unclear between the pipeline controller and
  platform controller.

Bounded transport config keeps the split clean:

- Envoy Gateway owns listener, route, backend, and TLS transport resources;
- Transit owns request classification, host selection, response observation,
  protocol sidecars, and the large business snapshot;
- the pipeline config references bounded transport identities rather than
  embedding transport resources.

#### Practical Guidance

Prefer the most native bounded transport model available:

1. If there are only a few provider families and EG `Backend` +
   `BackendTLSPolicy` can express their TLS requirements, use that. It is a
   better platform API than hand-authored `transport_socket_matches`, but it is
   still an xDS-coordinated channel.
2. If Transit needs a single dynamic cluster with per-host TLS personalities,
   use `transport_socket_matches` through `EnvoyPatchPolicy` or a controller
   that produces the same xDS shape. Treat this as bounded transport config,
   not dynamic provider catalog config.
3. If every provider requires unique TLS config and provider count is large or
   tenant-defined, do not push all of it into xDS. Introduce a bounded
   indirection: provider family, egress pool, trust bundle class, or regional
   backend group.
4. First research whether a stable generic TLS config plus dynamic host
   resolution can avoid provider-level xDS updates entirely.

The pipeline config snapshot can be huge and fast-moving. The Envoy Gateway
transport config should be small, bounded, and owned by the controller that is
responsible for producing xDS.

#### Research: Fully Dynamic Provider Host Resolution

We should still pursue whether provider host resolution can become entirely
dynamic, or at least more dynamic than the current
`transport_socket_matches` approach.

There are several different meanings of "dynamic" here:

1. **Dynamic provider catalog** — the pipeline config snapshot can add, remove,
   or change provider profiles without Envoy config changes. This is feasible
   with `PipelineConfig[T]`.
2. **Dynamic DNS/address resolution** — the cluster extension can resolve
   provider hostnames and call `ClusterHandle.AddHosts` / `RemoveHosts` as the
   provider set changes. This is also feasible in Transit, using the same
   snapshot/host-refresh shape as other Cluster Extension examples.
3. **Dynamic TLS personality** — the hard part. Envoy must know what TLS
   context to use before the upstream connection handshake: SNI, validation
   mode, trust roots, SAN policy, and optional client certs. Today our proven
   path selects among predeclared `transport_socket_matches`; it does not
   synthesize arbitrary new `UpstreamTlsContext` values from Go at request time.

The research question is whether a single or bounded TLS context can safely
cover many dynamic provider hostnames. Candidate paths:

- **Generic public-web TLS context**: one cluster TLS context using system roots
  plus automatic SNI/SAN validation derived from the upstream host or a header.
  This may work for public providers with ordinary WebPKI certificates if Envoy
  can derive SNI from the selected host at the right time.
- **`auto_sni` / `auto_san_validation`**: Envoy can derive SNI and SAN
  validation from `:authority` or another configured header as seen by the
  router. This needs careful testing with body-driven routing because our
  earlier orange work found that body-phase `:authority` rewrites are too late
  for `auto_sni` in that path.
- **`auto_host_sni` / host-derived SNI**: if Envoy can derive SNI from the
  selected upstream endpoint hostname rather than from request headers, this is
  more promising for async host selection. It would require the Cluster
  Extension to preserve hostnames as host identity, not only resolved IPs.
- **SDS / dynamic validation context**: SDS can make secrets and validation
  material dynamic, but it still needs to be evaluated against whether it can
  express arbitrary per-provider SNI/SAN policy without generating unbounded
  cluster transport config.
- **Controller-generated bounded xDS**: if fully dynamic data-plane TLS is not
  viable, the production path may be a controller that watches the pipeline
  provider catalog and reconciles only a bounded set of Gateway API
  `Backend`/`BackendTLSPolicy` resources or `transport_socket_matches`. This
  is a fallback because it introduces another control channel to coordinate.
- **Sidecar direct dial**: a protocol sidecar can use Go's TLS stack and be
  fully dynamic from the pipeline snapshot. That solves transport dynamism but
  moves TLS/auth ownership out of Envoy unless the sidecar dials an Envoy egress
  listener, which brings us back to bounded Envoy transport config.

Research should produce a matrix:

| Path | Single cluster | Dynamic hostnames | Dynamic SNI/SAN | Envoy owns TLS | Notes |
| --- | --- | --- | --- | --- | --- |
| `transport_socket_matches` | yes | yes | bounded/predeclared | yes | Proven in `cluster-async-router-eg`; not fully dynamic for TLS personalities. |
| Backend + BackendTLSPolicy | bounded clusters | bounded backends | bounded resources | yes | Natural EG model when provider families are bounded. |
| generic TLS + `auto_host_sni` | maybe | maybe | maybe | yes | Needs e2e proof with Cluster Extension hosts. |
| generic TLS + `auto_sni` header | maybe | maybe | timing-sensitive | yes | Body-driven routing likely hits the known timing trap. |
| sidecar direct dial | n/a | yes | yes | no | Useful escape hatch, weaker Envoy ownership. |

The immediate experiment should be small:

1. Add a local e2e variant with one dynamic-module cluster, one generic TLS
   transport socket, and two HTTPS upstream hostnames that present different
   certificates.
2. Have the Cluster Extension add hosts by hostname, not just resolved IP,
   if the ABI can preserve that through `HostSpec`.
3. Test `auto_host_sni` and automatic SAN validation after async
   `ClusterLBCompletion` selects the host.
4. Repeat with body-driven selection to verify the timing is still correct.
5. If that works, evaluate how Envoy Gateway can express the generic TLS
   context through `BackendTLSPolicy`, `Backend`, `BackendTrafficPolicy`, or an
   `EnvoyPatchPolicy` without listing every provider.

Until that experiment passes, the conservative design remains: dynamic provider
catalog and dynamic host resolution in Transit; bounded TLS transport
personalities in Envoy Gateway.

The research priority is to move as much of provider host resolution as
possible into the pipeline data plane, while keeping Envoy's TLS ownership. If
new providers can be added by the pipeline config provider without touching
Gateway resources or xDS, that is operationally cleaner than any design that
requires controller-to-controller coordination for provider churn.

### Request Object Inspection and Gating

Orange needs to inspect the request object before routing:

- headers for tenant, user, virtual key, auth context, and feature gates
- method, path, authority, and protocol for endpoint-specific behavior
- body fields such as `model`, `tools`, `stream`, or MCP method names
- active span and request id for correlation

Some decisions are read-only, such as logging and classification. Others are
gates: reject missing credentials, deny a model for a tenant, rate-limit a user,
or fail closed before any upstream is contacted.

Orange also needs to mutate the request object once the decision is known:

- rewrite `:authority` before upstream connection setup
- strip client-supplied auth headers
- inject internal routing or audit headers
- normalize paths for provider-specific APIs
- attach dynamic metadata and filter state for later extension points

Today a developer has to choose between request headers callbacks, mutable-body
callbacks, local responses, dynamic metadata, filter state, and stream objects.
Those are the right low-level pieces, but orange wants a higher-level way to
say "derive a request decision, publish it to the rest of the stream, and make
it available to request mutation, routing, logging, and upstream filters."

### Complete Request-Response Records

`examples/request-ui` shows the current complete-log pattern:

1. Request handler initializes per-stream state with method, path, host,
   request id, trace ids, and request headers.
2. Response handler enriches that state with status, response headers, and
   optionally response body.
3. `WithOnStreamFinalized` merges in Envoy finalization data: byte counts,
   durations, response flags, upstream timing, upstream addresses, response
   code details, and local reply body.
4. The finalized callback emits one complete record for success, local reply,
   upstream failure, and downstream disconnect.

That pattern should be available as a reusable SDK-level "exchange observer"
shape. Orange should not need to manually tie request state, response state,
and finalized stream metadata every time it wants audit logs, billing records,
or UI inspection.

### Response Object Inspection and Mutation

Orange also needs response-side hooks:

- inspect headers and trailers
- inspect buffered response bodies for non-streaming APIs
- tap streaming responses without buffering the whole stream
- mutate response headers or bodies when policy requires it
- emit usage metadata and counters
- attach response-derived fields to the final request-response record

`examples/sse-tap` is the streaming case. It observes response chunks, keeps
only a head/tail buffer, extracts token usage near the start and end of SSE
streams, and emits counters and dynamic metadata. That is a different shape
from full-body mutation: it is a streaming observer with bounded memory.

The SDK should distinguish these modes clearly:

- **observe streaming response**: no added latency, bounded buffer, cannot
  rewrite already-forwarded bytes
- **buffer and mutate response**: added latency/memory, can replace body
- **finalize exchange**: no mutation, complete post-stream record

### Protocol Sidecars

Some protocols cannot be handled cleanly by HTTP request/response callbacks.
`examples/ws-proxy` shows the pattern for WebSocket `/v1/responses`: after
`101 Switching Protocols`, Envoy forwards frames as a tunnel, so the dynamic
module starts an embedded `net/http` server via `RegisterWithGroup` and routes
the upgraded connection to that loopback server.

The same shape is likely for MCP protocol handling when the gateway needs to
inspect session-level messages, fan out, aggregate, or maintain protocol state.
There may be multiple variants:

- direct sidecar dial, where Go owns upstream TLS/auth
- egress-via-Envoy, where the sidecar loops back into an Envoy egress listener
  so Envoy still owns TLS, auth injection, policy, and observability
- protocol-specific taps, where only selected message types are parsed

This is still part of the same SDK ergonomics problem. A developer should not
have to rediscover how to bind a background server, route loopback traffic,
shut it down with filter config lifetime, and produce a session record.

## Pain Points

The current implementation is correct but too easy to get subtly wrong.

1. **String keys and type assertions**

   `classify` and `hostpick` must share `StreamObjectKey`, and `hostpick` must
   assert `v.(*pending.Pending)`. A typo or wrong type is a runtime failure.

2. **Every app writes its own one-shot promise**

   `pending.Pending` is generic in behavior but lives under `examples/orange`.
   The CAS, already-resolved callback path, first-writer-wins semantics, and
   teardown behavior are not orange-specific.

3. **Every async host selector repeats the same completion dance**

   `ChooseHost` must create a completion, register a callback, schedule back to
   the cluster main thread, check cancellation, map the result to a host, and
   complete exactly once.

4. **Cancellation is manual and inverted**

   Orange tracks canceled completions in a map. The happy path is empty; the
   map is only populated by `CancelHostSelection`. That is a reasonable local
   implementation detail, but it is not business logic.

5. **Cleanup is split across concepts**

   The SDK owns stream-object bag lifetime. Orange owns terminal resolution of
   the pending in `OnStreamComplete`. Developers must understand both or they
   risk a host selection that never completes.

6. **The phase-ordering model is non-obvious**

   The reason this exists is not visible at the call site: `ChooseHost` runs
   during `router.decodeHeaders`, before mutable-body classification. The SDK
   can encode the safe pattern more clearly than documentation alone.

7. **Request/response records are hand-assembled**

   `request-ui` proves the SDK has enough callbacks to build complete exchange
   logs, but the developer still owns the accumulator, callback ordering,
   finalization merge, body truncation, and delivery semantics.

8. **Streaming observation and body mutation are easy to confuse**

   `sse-tap` is intentionally a bounded streaming observer. A response body
   mutator has different latency and memory semantics. The SDK should make the
   difference explicit so policy code picks the right mode.

9. **Protocol sidecars require too much infrastructure code**

   `ws-proxy` has to combine `Group` lifecycle, embedded serving, loopback
   routing, frame pumps, optional Envoy egress, and session records. MCP-style
   sidecars will repeat the same scaffolding unless the SDK grows a protocol
   sidecar helper.

10. **Configuration is pipeline-scoped but implemented per module**

   Orange config is read by classify, hostpick, translate, response taps, and
   sidecars. Treating each extension point as its own config island forces
   developers to coordinate reloads by convention. Large config blobs also do
   not belong in Envoy config just to make them visible to Go code.

## Proposed SDK Surface

The SDK should provide generic building blocks, not an orange router. The
building blocks fall into four groups:

- stream rendezvous for cross-phase decisions
- pipeline-scoped configuration snapshots
- exchange observation for complete request-response records
- explicit response observation/mutation modes
- protocol sidecar lifecycle helpers

The rendezvous pieces solve the immediate body-driven host-selection pain. The
other pieces keep orange from becoming a pile of unrelated local mini-SDKs.

### Pipeline Config Provider

Introduce a pipeline-owned config abstraction that is shared by all extension
points in the same dynamic module:

```go
type PipelineConfig[T any] struct { ... }

func NewPipelineConfig[T any](initial T, opts ...PipelineConfigOption[T]) *PipelineConfig[T]

func (c *PipelineConfig[T]) Snapshot() T
func (c *PipelineConfig[T]) Version() string
func (c *PipelineConfig[T]) Start(g *Group)
func (c *PipelineConfig[T]) Stop()
```

The SDK should not dictate the config schema. Orange would define its own
normalized shape:

```go
type OrangeConfig struct {
    Version   string
    Providers ProviderIndex
    Models    ModelMatcher
    Tenants   TenantPolicyIndex
    Tools     ToolCatalogIndex
    Cache     *ProviderCache
}
```

The provider interface should separate transport from normalization:

```go
type RawConfig []byte

type ConfigSource interface {
    Fetch(context.Context) (RawConfig, error)
}

type ConfigDecoder[T any] interface {
    DecodeAndIndex(RawConfig) (T, error)
}
```

Provider implementations can include:

```go
func FileConfigSource(path string) ConfigSource
func PollingConfigSource(url string, opts PollOptions) ConfigSource
func GRPCStreamConfigSource(target string, opts StreamOptions) ConfigSource
func StaticConfigProvider[T any](snapshot T) *PipelineConfig[T]
```

`PipelineConfig` would own the generic lifecycle:

- run provider refreshes in an `up.Group`
- apply interval, timeout, jitter, and backoff
- validate and decode off the request path
- publish immutable snapshots atomically
- keep last-good on fetch, validation, or decode errors
- expose version/age/error metrics
- optionally call observer hooks on refresh success/failure
- stop cleanly when the dynamic module config is destroyed

The request path stays simple:

```go
cfg := orangePipeline.Config.Snapshot()
decision := cfg.Models.Lookup(model, tenant)
```

For large configs, the decoded snapshot should include precomputed indexes and
bounded caches:

```go
type ProviderCache interface {
    GetProvider(name string) (ProviderProfile, bool)
    PutProvider(name string, p ProviderProfile)
}
```

The SDK does not need to own a specific cache algorithm in the first version,
but it should make eager stashing a first-class extension point. A default LRU
helper would be useful for provider profiles, credential references, MCP server
descriptors, and precompiled matchers.

This is deliberately independent of Envoy/xDS for large business state. Envoy
config should still carry the stable wiring: listeners, routes, dynamic module
names, bounded clusters/backends, transport sockets/TLS policy, and small
bootstrap pointers such as "config API URL". The pipeline config API carries
large and fast-moving application state.

### Pipeline Runtime

Once config is pipeline-scoped, there should be a small object that bundles
shared services:

```go
type Pipeline[TConfig any] struct {
    Config *PipelineConfig[TConfig]
    Group  *Group
}
```

Orange modules would import or receive the same pipeline runtime:

```go
var orangePipeline = up.NewPipeline[OrangeConfig](...)

func init() {
    up.Register("orange-classify", classify.Handler(orangePipeline), ...)
    up.RegisterCluster("orange-hostpick", hostpick.Factory(orangePipeline))
    up.Register("orange-translate", translate.Handler(orangePipeline))
}
```

The goal is not global mutable state for everything. The goal is one explicit
pipeline runtime whose lifetime, config, caches, metrics, and sidecars are
shared intentionally by all extension points that participate in the same
gateway pipeline.

### Typed Stream Keys

Replace stringly-typed stream objects with typed keys:

```go
type StreamKey[T any] struct {
    name string
}

func NewStreamKey[T any](name string) StreamKey[T]

func SetStreamValue[T any](w *Writer, key StreamKey[T], value T)
func GetStreamValue[T any](w *Writer, key StreamKey[T]) (T, bool)
func GetClusterStreamValue[T any](ctx ClusterLBContext, key StreamKey[T]) (T, bool)
```

Orange would define:

```go
var ClassificationKey = up.NewStreamKey[*up.StreamPromise[Decision]]("orange.classification")
```

Then headers and host selection become type-safe:

```go
p := up.NewStreamPromise[Decision]()
up.SetStreamValue(w, ClassificationKey, p)

p, ok := up.GetClusterStreamValue(ctx, ClassificationKey)
```

This removes duplicate type assertions and makes key ownership explicit.

### Stream Promise

Move the orange `pending.Pending` pattern into the SDK:

```go
type StreamPromise[T any] struct { ... }

func NewStreamPromise[T any]() *StreamPromise[T]
func (p *StreamPromise[T]) Resolve(T) bool
func (p *StreamPromise[T]) OnResolve(func(T)) CancelFunc
func (p *StreamPromise[T]) Result() (T, bool)
```

Semantics:

- first `Resolve` wins
- callback fires at most once
- callback fires inline if already resolved
- callback registration can be canceled
- no goroutine is parked by default

The SDK should document that callbacks may run on whichever thread resolves
the promise. Code that touches Envoy cluster state still needs a cluster
scheduler, or should use the host-selection adapter below.

### Async Host Selection Adapter

The biggest ergonomics win is a helper that owns the repetitive, dangerous
`ClusterLBCompletion` logic:

```go
type AsyncHostSelector[T any] struct { ... }

func NewAsyncHostSelector[T any](handle ClusterHandle) *AsyncHostSelector[T]

func (s *AsyncHostSelector[T]) Choose(
    ctx ClusterLBContext,
    key StreamKey[*StreamPromise[T]],
    decide func(T) (HostPtr, string),
) (HostPtr, *ClusterLBCompletion)

func (s *AsyncHostSelector[T]) Cancel(completion *ClusterLBCompletion)
```

`Choose` would:

- read the typed promise from the stream-object bag
- return immediate failure if the promise is missing
- create `ctx.NewCompletion()`
- register a promise callback
- schedule completion on the cluster main thread
- guard against `CancelHostSelection`
- call `completion.Complete(host, errDetail)` exactly once

Orange hostpick would shrink to the application-specific mapping:

```go
func (l *lb) ChooseHost(_ up.ClusterLBHandle, ctx up.ClusterLBContext) (up.HostPtr, *up.ClusterLBCompletion) {
    return l.selector.Choose(ctx, classify.ClassificationKey, func(d classify.Decision) (up.HostPtr, string) {
        if d.Err != "" {
            return nil, d.Err
        }
        host := l.hostByProvider(d.Provider)
        if host == nil {
            return nil, "orange.unknown_upstream"
        }
        return host, ""
    })
}

func (l *lb) CancelHostSelection(c *up.ClusterLBCompletion) {
    l.selector.Cancel(c)
}
```

The app still owns provider lookup, host maps, and error strings. The SDK owns
the concurrency and Envoy-threading contract.

### Exchange Observer

Provide a reusable way to accumulate request/response/finalized information for
one stream:

```go
type ExchangeObserver[T any] struct { ... }

type ExchangeHooks[T any] struct {
    OnRequest  func(*Writer, *Request, *T)
    OnResponse func(*Writer, *ResponseChunk, *T)
    OnFinal    func(*T, FinalizedInfo)
}

func WithExchangeObserver[T any](hooks ExchangeHooks[T]) FilterOption
```

The SDK would own:

- allocating and storing per-stream accumulator state
- passing the same state to request, response, and finalization callbacks
- finalization ordering for success, local replies, upstream failures, and
  disconnects
- optional body truncation helpers
- safe return of pooled state after finalization

Application code would own the record schema and sink:

```go
type GatewayRecord struct {
    Tenant string
    Model  string
    Status int
    Usage  Usage
}

up.WithExchangeObserver(up.ExchangeHooks[GatewayRecord]{
    OnRequest: func(w *up.Writer, r *up.Request, rec *GatewayRecord) {
        rec.Tenant = r.Header("x-tenant-id")
    },
    OnResponse: func(w *up.Writer, chunk *up.ResponseChunk, rec *GatewayRecord) {
        if chunk.StatusCode != 0 {
            rec.Status = chunk.StatusCode
        }
    },
    OnFinal: func(rec *GatewayRecord, info up.FinalizedInfo) {
        sink.Send(rec, info)
    },
})
```

This is the generalized `request-ui` pattern.

### Request Decision and Gating

Request classification should produce a typed decision that can be used by
routing, policy, translation, and final logging:

```go
type RequestDecision struct {
    Tenant   string
    User     string
    Model    string
    Provider string
    Deny     *DenyDecision
}
```

The SDK does not need to know those fields, but it can provide a convention for
publishing the decision once:

```go
var DecisionKey = up.NewStreamKey[*up.StreamPromise[RequestDecision]]("orange.decision")
var DecisionMetadata = up.NewMetadataSchema("orange",
    "tenant", "user", "model", "provider",
)
```

The desired ergonomics:

- request headers/body code reads the active pipeline config and resolves one
  typed decision
- host selection consumes the same decision through a promise
- upstream filters read stable metadata derived from the decision
- final logging reads the typed decision or finalized metadata
- deny decisions map to local responses and terminal host-selection errors

That keeps "check user headers" and "pick provider by body" in one decision
pipeline instead of scattering it across independent filters.

### Response Modes

Expose response behavior as separate, named modes:

```go
func WithStreamingResponseObserver(fn ResponseObserverFunc, opts ...ResponseObserveOption) FilterOption
func WithBufferedResponseMutator(fn ResponseMutatorFunc, opts ...ResponseMutateOption) FilterOption
func WithResponseFinalizer(fn ResponseFinalizerFunc) FilterOption
```

The point is not necessarily these exact names. The point is that SDK users
should have to choose one of three semantics:

- observe chunks as they pass through
- buffer then mutate
- finalize after Envoy knows the whole stream outcome

Orange usage examples:

- SSE usage extraction: streaming observer with a head/tail buffer
- non-streaming JSON policy rewrite: buffered mutator
- audit/billing record: finalizer or exchange observer

### Protocol Sidecar

Wrap the `RegisterWithGroup` plus embedded server pattern:

```go
type SidecarConfig struct {
    Name       string
    ListenAddr string
    Routes     []SidecarRoute
}

func RegisterSidecar(name string, cfg SidecarConfig, h http.Handler) *Group
```

A useful version would own:

- background server lifecycle tied to filter config lifetime
- listener startup readiness
- graceful shutdown
- session context and deadline helpers
- optional session record hooks
- documented Envoy loopback cluster wiring
- optional egress-via-Envoy dial helper

Orange can then use sidecars for:

- WebSocket `/v1/responses`
- MCP protocol sessions
- future protocol bridges where HTTP callbacks are not enough

Sidecars should remain explicit. They are heavier than filters and should be
chosen only when Envoy's callback surface cannot observe or mutate the protocol
objects directly.

## Non-Goals

- Do not add an SDK concept of "model", "provider", or "LLM".
- Do not add a new control-plane protocol as the only supported answer.
  Pipeline config providers should be transport-pluggable.
- Do not make dynamic metadata implicit. Upstream HTTP filters should still
  read explicit metadata written by downstream filters.
- Do not hide Envoy's phase model entirely. The helper should make the safe
  route easy, but docs should still explain why body-driven routing needs async
  host selection.
- Do not replace simple header-driven routing. If routing data is available at
  headers phase, `SetFilterState`, `GetFilterState`, or direct header reads are
  still simpler.
- Do not force every gateway feature through one mega-filter. The goal is a
  shared stream/exchange substrate that multiple filters and sidecars can use.
- Do not treat sidecars as the default. Prefer native HTTP filter callbacks
  when they can inspect or mutate the protocol object safely.
- Do not use xDS as the large business-config transport just because it is
  already present. Envoy config should wire the pipeline; pipeline config should
  feed the application.

## Development Sequence

1. Add `PipelineConfig[T]` with static and file providers.
2. Add polling provider support with interval, timeout, jitter, backoff,
   last-good semantics, version/age metrics, and refresh observers.
3. Research dynamic provider host resolution and TLS personality handling:
   generic TLS context, `auto_host_sni`, `auto_sni`, SDS, Backend +
   BackendTLSPolicy, and controller-generated bounded xDS.
4. Add optional eager-cache helpers, starting with a small bounded LRU.
5. Update orange config loading so classify, hostpick, translate, taps, and
   sidecars read one shared pipeline snapshot.
6. Add typed stream keys as wrappers over the existing stream-object bag.
7. Add `StreamPromise[T]` and port `examples/orange/pending` tests to SDK tests.
8. Add `AsyncHostSelector[T]` and port orange's cancellation tests onto it.
9. Add an exchange observer helper and port the request/response/finalized
   accumulator from `examples/request-ui`.
10. Add named response observer/mutator modes and validate them against
   `examples/sse-tap` plus a buffered response mutation example.
11. Add a sidecar helper and validate it against `examples/ws-proxy`, then the
   MCP protocol path.
12. Update `examples/orange` to define a typed `Decision` and remove its local
   `pending` package.
13. Update `docs/orange-token-correlation-risks.md` and `examples/orange/README.md`
   to reflect the current no-global-registry implementation and the new SDK
   primitive.

## Open Questions

1. Should `StreamPromise.OnResolve` allow multiple callbacks, or should it stay
   single-consumer like orange's current `Pending`?
2. Should missing promise in `AsyncHostSelector.Choose` return `(nil, nil)` or
   complete with a configured error detail?
3. Should the SDK expose a direct `ResolveOnStreamComplete` option so terminal
   promise resolution is less manual?
4. Should typed stream keys live in `up` only, or should `down.ClusterLBContext`
   also expose typed helpers?
5. Should exchange observation own body capture/truncation, or should it only
   coordinate callbacks and leave body policy entirely to user code?
6. Should response mutation be limited to buffered bodies, or should the SDK
   support streaming transforms with explicit backpressure semantics?
7. Should protocol sidecars be an SDK package, an example helper, or both?
8. How should a typed request decision map to dynamic metadata without making
   metadata implicit or lossy?
9. Should the first production config provider be HTTP polling only, with gRPC
   streaming left as a later provider, or should both land together behind the
   same interface?
10. What should the minimum config snapshot contract include: version, fetched
   time, source, checksum, last error, and age?
11. Should eager stashing/caching be owned by the decoder, by `PipelineConfig`,
   or by a separate SDK cache package?
12. How should huge snapshots be redacted and exposed for debug endpoints
   without leaking secrets or dumping megabytes into local replies?

The smallest useful version is typed stream keys plus `StreamPromise[T]`. The
highest-value routing version is the async host-selection adapter, because that
is where most of the correctness hazards live. The larger orange SDK direction
is a composable pipeline substrate: one shared config snapshot, one typed
request decision, one complete exchange record, explicit response modes, and
explicit sidecars when HTTP callbacks are not enough.
