# Orange Pipeline SDK

Status: working research and development packet.

This directory collects the SDK background, execution plan, and supporting
design notes for turning orange-style examples into pipeline-level SDK
primitives.

The core thesis:

> Orange is a pipeline, not one filter — and not one protocol.

"Orange" names a *shape* of gateway, not a product. The same pipeline shape
appears in the LLM proxy (`examples/orange`) and in the MCP profile fan-out
topology (`examples/mcp-*`, `integrations/mcp-profile-tiered-router-eg`).
Both need shared configuration, cross-phase request decisions, async host
selection, request-response records, response taps/mutators, protocol
sidecars, host refresh loops, and bounded Envoy Gateway transport/TLS
coordination.

Three goals sit alongside the pipeline thesis, in priority order:

1. **Everything goes through Envoy.** This is the highest-priority
   principle. Data-path egress, ingress, TLS, retries, timeouts, load
   balancing, observability emission, and access logging all flow through
   Envoy. Sidecars exist to *shape* protocol traffic, not to bypass Envoy.
   When the obvious implementation would skip Envoy (e.g. a sidecar
   dialing an upstream directly, an extension shipping its own metrics,
   a filter writing its own access log), the design must instead route
   that traffic back through Envoy — even when that requires a dance.
   The reference dances:
   - **Egress via Envoy from a sidecar.** See
     [`integrations/tiered-ws-proxy-eg`](../../integrations/tiered-ws-proxy-eg):
     the WS sidecar terminates the client connection but dials its
     upstream through Envoy so TLS, retries, and access logging remain
     Envoy-owned.
   - **Observability via Envoy.** See [`examples/observability`](../../examples/observability):
     extensions emit signals through Envoy's stats/access-log/trace
     surfaces rather than parallel pipelines.
   - **Trace propagation across sidecars.** See
     [`examples/trace-propagation`](../../examples/trace-propagation):
     sidecars participate in the same trace context Envoy carries, not a
     separate one.

   **Break-glass / escape hatch.** Any deviation from "via Envoy" requires
   written rationale: what Envoy capability is missing, what was attempted,
   what the operational cost of the escape hatch is, and what would let us
   close it. Sidecar direct dial (the WS-B fallback) is the canonical
   example — it loses Envoy TLS ownership and must be documented as such.

2. **Protocol-pluggable.** LLM and MCP are the first two consumers; neither
   gets to bake its concepts into `up/`. New protocols (gRPC fan-out,
   webhooks, custom L7) should drop in by composing the same primitives.

3. **Play nice with Envoy Gateway.** Integrate cleanly with EG — native
   Gateway API resources where they fit, bounded EPP/xDS where they
   don't, a clear story for which dynamic surface (pipeline config,
   Gateway resource, xDS) owns which kind of change. The runnable EG
   integrations are the proving ground:
   [`tiered-router-eg`](../../integrations/tiered-router-eg),
   [`tiered-ws-proxy-eg`](../../integrations/tiered-ws-proxy-eg),
   [`cluster-async-router-eg`](../../integrations/cluster-async-router-eg),
   [`mcp-profile-tiered-router-eg`](../../integrations/mcp-profile-tiered-router-eg).
   See [WS-H](plan.md#workstream-h).

## Reference Pipelines

Two real consumers, same SDK surface. Every primitive must serve both before
it lands.

| Pipeline | Phases | Example |
| --- | --- | --- |
| **LLM proxy** | classify (body → provider) → async hostpick → translate (auth/headers) → response tap (token usage) | [`examples/orange`](../../examples/orange) |
| **MCP L1 fan-out** | session decode → profile lookup → fan-out to L2 members → response merge (tools/list, initialize) → session encode | [`examples/mcp-profile-gateway`](../../examples/mcp-profile-gateway), [`examples/mcp-catalog-router`](../../examples/mcp-catalog-router), [`examples/mcp-profile-router`](../../examples/mcp-profile-router) |
| **MCP tiered EG** | L1 (profile gateway, dynamic module) + L2 (catalog router, cluster-router host selection) over Envoy Gateway | [`integrations/mcp-profile-tiered-router-eg`](../../integrations/mcp-profile-tiered-router-eg) |
| **Tiered router EG** | Generic L1/L2 tiered routing topology over EG; proving ground for bounded EG integration shapes | [`integrations/tiered-router-eg`](../../integrations/tiered-router-eg) |
| **Tiered WS proxy EG** | WebSocket sidecar with egress-via-Envoy; reference dance for "everything through Envoy" when the natural path would skip it | [`integrations/tiered-ws-proxy-eg`](../../integrations/tiered-ws-proxy-eg) |
| **Async cluster router EG** | Async host selection (`ClusterLBCompletion`) integrated with EG xDS | [`integrations/cluster-async-router-eg`](../../integrations/cluster-async-router-eg) |

If a proposed primitive only serves one of the rows above, it does not
belong in `up/` yet — keep it example-local until a second consumer needs it.

See [`mcp-fit-notes.md`](mcp-fit-notes.md) for the per-workstream audit of
how the MCP examples map onto each proposed `up/` primitive (and where they
don't).

## The Three Phases: Classify, Translate, Observe

Both reference pipelines decompose into the same three phases. This is
borrowed wholesale from the ai-gateway ExtProc model (see
`/Users/dio/src/dio/llm-spike/AI_GATEWAY_EXTPROC_PHASES.md`) and becomes
the shared pipeline vocabulary across LLM and MCP. Workstreams map onto
phases, not the other way round.

| Phase | When | LLM (orange) | MCP (profile gateway) |
| --- | --- | --- | --- |
| **Classify** | Router filter, request body complete | Parse body → pick model/provider; write typed decision to `StreamKey` | Decode `mcp-session-id` envelope; resolve `{prefix}__{tool}` → owning server; write typed decision |
| **Translate** | Upstream filter, headers + body | 2a select translator from backend schema (`GetTranslator(schema, override)`); 2b apply translator on full body; run auth handler (SigV4 signs over mutated body) | Inject credentials and per-leg headers; rewrite path/`:authority` for L2 cluster; for fan-out, prepare per-leg request bodies |
| **Observe** | Upstream filter, response body | Apply response translator (provider → OpenAI); extract token usage; emit metrics/spans | Merge fan-out leg responses (`tools/list`, `initialize`); re-encode session envelope; emit per-leg + aggregate signals |

Two notes on Translate:

- **It is two sub-stages.** 2a runs at request headers (translator
  selected from backend schema + endpoint spec; routing committed).
  2b runs once the request body is complete (translator applied to the
  full body). The split exists because streaming bodies arrive in
  pieces — see `/Users/dio/src/dio/llm-spike/TWO_TRANSLATE_STAGES_EXPLAINED.md`.
- **Auth runs after the translator**, not in parallel. AWS SigV4 must
  sign over the *mutated* body, so the auth handler is sequenced after
  2b. Four auth patterns exist today (API key, AWS SigV4, GCP Workload
  Identity, Azure OAuth2/Entra) — see
  `/Users/dio/src/dio/llm-spike/AUTHENTICATION_PATTERNS.md`.

Observe is not just record accumulation. It does real work: response-side
translation, token-usage extraction, span finalization. WS-E and WS-F sit
inside this phase.

## What the SDK Should Look Like

Aspirational end-state. None of these primitives exist in `up/` yet; track
status in the [Track Map](#track-map). LLM-flavored snippet below; the MCP
fan-out builds from the same primitives — `StreamKey` carries a profile id,
`AsyncHostSelector` resolves L2 members, an exchange observer merges
fan-out responses, and a sidecar handles long-lived session state.

```go
// One immutable config snapshot shared by every extension point.
cfg := up.NewPipelineConfig[orange.Catalog](
    up.PollingSource("https://config.internal/orange.json",
        up.PollInterval(30*time.Second), up.LastGood()),
    orange.DecodeCatalog,
)

// Typed cross-phase rendezvous: classify writes, hostpick reads.
var decisionKey = up.NewStreamKey[orange.Decision]("orange.decision")

// SDK owns ClusterLBCompletion lifecycle; app only maps decision -> host.
selector := up.NewAsyncHostSelector[orange.Decision](decisionKey,
    func(d orange.Decision) (up.HostPtr, string) {
        return cfg.Snapshot().HostFor(d.Provider), d.Provider.SNI
    })

pipeline := up.NewPipeline(cfg,
    up.WithRequestFilter("classify", orange.Classify(decisionKey)),
    up.WithClusterExtension("hostpick", selector),
    up.WithRequestFilter("translate", orange.Translate(decisionKey)),
    up.WithExchangeObserver(requestui.Hooks()),         // WS-E
    up.WithResponseObserver(ssetap.Observer()),         // WS-F streaming
    up.WithSidecar("ws-proxy", wsproxy.New()),          // WS-G
)
```

The point of the packet is to justify, design, and verify every line above.

## Non-Goals

These are load-bearing constraints. Re-read before proposing any abstraction.

- Do not bake protocol-specific concepts (LLM, MCP, anything else) into the
  SDK. Protocol logic lives in examples and consumers; `up/` stays neutral.
- Do not make xDS the transport for large business configuration.
- Do not hide Envoy phase ordering from SDK users.
- Do not force every feature into one mega-filter.
- Do not replace native Gateway API resources when they are the better
  bounded platform API.
- Do not fork the SDK per consumer: if LLM and MCP need different shapes of
  the same primitive, the primitive is wrong.
- Do not invent parallel data paths that bypass Envoy (sidecar direct dial,
  out-of-band telemetry, side-channel auth). If Envoy is missing a
  capability, fix that first; escape hatches require written rationale per
  the principle above.
- Do not model the translator layer as an interface with one method per
  direction. Translators are **pure functions** with concrete request and
  response types. No `Translator[ReqT, SpanT]` vtable, no mocks, no
  generic indirection — direct calls dispatched by a `Route` value that
  carries function pointers plus upstream metadata (cluster, path). See
  `/Users/dio/src/dio/llm-spike/ALT_PATH_PURE_FUNCTIONS.md`. The trade
  is documented in
  `/Users/dio/src/dio/llm-spike/CODE_COMPARISON.md`: pure wins on
  testability, clarity, and ~5–7% throughput; the interface path wins
  only on shipping speed for a v1.

## Track Map

Status legend: `spike` (research-gated) · `todo` · `wip` · `landed` ·
`blocked`.

| Track | Status | Primary Doc | Example | Purpose |
| --- | --- | --- | --- | --- |
| Pipeline config | `todo` | [Plan A](plan.md#workstream-a-pipeline-configuration), [Background](background.md#pipeline-configuration-plane) | LLM: [`examples/orange/config`](../../examples/orange/config) · MCP: [`examples/mcp-profile-router`](../../examples/mcp-profile-router) | One immutable config snapshot shared by all extension points. |
| Dynamic provider host/TLS | `spike` | [Standalone question](dynamic-module-transport-tls-question.md), [Plan B](plan.md#workstream-b-dynamic-provider-host-resolution-and-tls), [Background](background.md#research-fully-dynamic-provider-host-resolution) | LLM: [`examples/orange/envoy.yaml`](../../examples/orange/envoy.yaml) | Research whether provider/backend add/remove can avoid Gateway/xDS updates while Envoy still owns TLS. |
| Typed rendezvous | `todo` | [Plan C](plan.md#workstream-c-typed-rendezvous), [Background](background.md#typed-stream-keys) | LLM: [`examples/orange/pending`](../../examples/orange/pending) · MCP: [`examples/mcp-profile-gateway`](../../examples/mcp-profile-gateway) | Typed stream keys and `StreamPromise[T]` for cross-phase decisions (model id, profile id, session id). |
| Async host selection | `todo` | [Plan D](plan.md#workstream-d-async-host-selector), [Background](background.md#async-host-selection-adapter) | LLM: [`examples/orange/hostpick`](../../examples/orange/hostpick) · MCP: [`examples/mcp-catalog-router`](../../examples/mcp-catalog-router) | SDK-owned `ClusterLBCompletion` scheduling and cancellation. |
| Exchange observer | `todo` | [Plan E](plan.md#workstream-e-exchange-observer), [Background](background.md#exchange-observer) | LLM: [`examples/request-ui`](../../examples/request-ui) · MCP: fan-out merge in [`examples/mcp-profile-gateway`](../../examples/mcp-profile-gateway) | Reusable request/response/finalized accumulator. |
| Response modes | `todo` | [Plan F](plan.md#workstream-f-response-modes), [Background](background.md#response-modes) | LLM: [`examples/sse-tap`](../../examples/sse-tap) · MCP: tools/list merge | Explicit streaming observe vs buffered mutate vs finalized-only semantics. |
| Protocol sidecars | `todo` | [Plan G](plan.md#workstream-g-protocol-sidecars), [Background](background.md#protocol-sidecar) | LLM: [`examples/ws-proxy`](../../examples/ws-proxy) · MCP: session/aggregator lifecycle in [`examples/mcp-profile-gateway`](../../examples/mcp-profile-gateway) | Embedded protocol server lifecycle for WS, MCP, and similar long-lived paths. |
| Host refresh | `todo` | [Host Refresh Loop](host-refresh-loop.md) | — | Generic host snapshot refresh for Cluster Extensions. |
| EG transport | `spike` | [Plan H](plan.md#workstream-h-envoy-gateway-transport-integration), [Background](background.md#bounded-transporttls-configuration) | LLM: [`integrations/cluster-async-router-eg`](../../integrations/cluster-async-router-eg) · MCP: [`integrations/mcp-profile-tiered-router-eg`](../../integrations/mcp-profile-tiered-router-eg) | Bounded Backend/TLS policy or `transport_socket_matches`; avoid unbounded xDS; cooperate with native Gateway API. |

Update the Status column in the same PR that lands a workstream's primitive.

## Critical Path

The two `spike` workstreams gate everything else. Treat them as research
spikes whose outcomes reshape the engineering tracks, not as deliverables in
sequence with them.

**WS-B (dynamic provider host/TLS) outcome forks the plan into one of:**

1. *Fully dynamic host + TLS personality*. Provider add/remove is
   pipeline-config-only; WS-A is sufficient and WS-H reduces to "stable
   generic transport config." Best outcome.
2. *Dynamic hosts, bounded TLS families*. WS-H ships Backend +
   BackendTLSPolicy per family; provider catalog churn within a family is
   config-only, new families are a two-channel rollout.
3. *Sidecar direct dial*. Envoy loses TLS ownership for dynamic providers;
   WS-G grows an egress-dial helper and we document the tradeoff.

Until WS-B resolves, WS-A / WS-C / WS-D can proceed in parallel but their
public API shapes (especially around host identity in `AsyncHostSelector`)
may shift. Land them behind unexported types or `internal/` packages first.

Concrete first moves:

1. Stand up the WS-B e2e harness (one dynamic-module cluster, two HTTPS
   upstreams with distinct certs) before touching `up/`.
2. Implement `PipelineConfig[T]` with static and file sources (WS-A
   foundation; no WS-B coupling).
3. Add `StreamKey[T]` and `StreamPromise[T]` unit tests in `up/` (WS-C; no
   WS-B coupling).
4. Defer `AsyncHostSelector[T]` public API until WS-B answers whether
   `HostPtr` carries a hostname or only a resolved address.

## Verification Rule

> Unit tests are enough only for local SDK behavior.

A change **requires an e2e proof** if it touches any of:

- [ ] Envoy phase ordering (decode/encode header/body/trailer interleaving)
- [ ] Dynamic-module ABI (anything crossing the Go ↔ C++ boundary)
- [ ] Cluster host selection or `ClusterLBCompletion` lifecycle
- [ ] Upstream TLS (SNI, SAN validation, transport socket selection)
- [ ] Envoy Gateway xDS (Backend, BackendTLSPolicy, EPP, generated cluster names)

If you check any box above and your PR has only unit tests, it is not done.
See the [verification matrix](plan.md#cross-cutting-verification-matrix) for
which suite covers which workstream.

## Glossary

Project-local and Envoy-internal terms used throughout the packet.

| Term | Meaning |
| --- | --- |
| **orange** | Both the multi-provider LLM proxy at [`examples/orange`](../../examples/orange) *and* the shape of pipeline this SDK generalizes. When unqualified, refers to the shape. |
| **classify / hostpick / translate** | The three orange LLM phases: parse body to pick a provider, complete async host selection, rewrite request for the upstream provider. |
| **MCP** | Model Context Protocol. Long-lived session-oriented protocol for tool/resource exchange between an MCP client and one or more MCP servers. |
| **MCP L1 / L2** | Tiered MCP topology. L1 is the public profile gateway (fan-out, session encoding, response merge). L2 is the per-server backend tier (catalog routing, host selection). See [`integrations/mcp-profile-tiered-router-eg`](../../integrations/mcp-profile-tiered-router-eg). |
| **MCP profile** | A named bundle of MCP servers exposed under one L1 endpoint; client sees one logical server, L1 fans out to members. |
| **MCP catalog** | Per-server metadata (tools, resources) used by L2 to route `/mcp/s/{server}` and by L1 to merge `tools/list`. |
| **EG** | Envoy Gateway. |
| **EPP** | Envoy Patch Policy. Envoy Gateway extension point for patching generated xDS. |
| **SDS** | Secret Discovery Service. Envoy's dynamic TLS material channel. |
| **`auto_host_sni`** | Envoy cluster option deriving SNI from the selected host's hostname. |
| **`auto_sni`** | Envoy router option deriving SNI from `:authority`. |
| **`transport_socket_matches`** | Cluster-level mechanism selecting a transport socket (and thus TLS config) by host metadata. |
| **`ClusterLBCompletion`** | Dynamic-module ABI for completing host selection asynchronously. |
| **`CLUSTER_PROVIDED`** | Load balancing policy where the cluster (not Envoy's LB) supplies the host. |
| **BackendTLSPolicy** | Gateway API resource binding TLS validation to a Backend. |
| **dynamic-module Cluster Extension** | Go-owned cluster extension running inside Envoy via the dynamic-module ABI. |
| **stream object** | Per-stream bag the dynamic module uses to carry typed values across phases; see `up/stream_finalized.go`. |
| **rendezvous** | Cross-phase handoff: one phase produces a value the next phase awaits. |
| **egress via Envoy** | Pattern where a sidecar terminates the client side but dials its upstream through the local Envoy, preserving Envoy ownership of TLS, retries, and access logging. See [`integrations/tiered-ws-proxy-eg`](../../integrations/tiered-ws-proxy-eg). |
| **break glass / escape hatch** | A deliberate departure from the "everything through Envoy" principle. Requires written rationale: missing Envoy capability, what was tried, operational cost, and what would let us close the hatch. |
