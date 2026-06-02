# Orange Pipeline SDK Research and Development Plan

Status: proposed execution plan.

Related background:

- `docs/orange-pipeline-sdk/README.md` (entry point, principles, glossary)
- `docs/orange-pipeline-sdk/background.md`
- `docs/orange-pipeline-sdk/host-refresh-loop.md`
- `docs/cluster-async-router-eg.md`

Reference consumers (every primitive must serve at least one LLM and one MCP
consumer before it lands in `up/`):

- LLM: `examples/orange/`, `examples/request-ui/`, `examples/sse-tap/`,
  `examples/ws-proxy/`
- MCP: `examples/mcp-profile-gateway/`, `examples/mcp-catalog-router/`,
  `examples/mcp-profile-router/`
- Cross-cutting: `examples/observability/`, `examples/trace-propagation/`

Envoy Gateway proving grounds (every WS-H decision must be exercised on at
least one of these):

- `integrations/tiered-router-eg/`
- `integrations/tiered-ws-proxy-eg/` (egress-via-Envoy dance reference)
- `integrations/cluster-async-router-eg/`
- `integrations/mcp-profile-tiered-router-eg/`

## Principles

These rank above every workstream decision. When a primitive's design
conflicts with a principle, the principle wins.

1. **Everything goes through Envoy.** Data-path egress, ingress, TLS,
   retries, timeouts, load balancing, observability emission, and access
   logging flow through Envoy. Sidecars *shape* protocol traffic; they do
   not bypass Envoy. When the natural implementation would skip Envoy,
   route the traffic back through it — even when that requires the dance
   in `integrations/tiered-ws-proxy-eg`. Observability flows through
   Envoy's stats/access-log/trace surfaces (`examples/observability`),
   including across sidecars (`examples/trace-propagation`).
2. **Break-glass requires written rationale.** Any deviation from
   principle 1 must document: which Envoy capability is missing, what was
   tried, the operational cost, and the conditions that would let us
   close the hatch. Sidecar direct dial is the canonical escape hatch and
   must be labeled as such wherever it appears.
3. **Protocol-pluggable.** `up/` stays neutral. LLM and MCP both consume
   it; neither's concepts leak in.
4. **Phase ordering is visible.** Users see Envoy's decode/encode phase
   order; the SDK does not hide it behind a request-lifecycle facade.
5. **Bounded xDS surface.** Pipeline config carries business churn; xDS
   carries bounded transport/identity. Provider catalog churn must not
   require xDS updates if avoidable.
6. **Translator layer is pure functions, not an interface.** Request
   and response translation between API schemas (OpenAI ↔ AWS Bedrock,
   OpenAI ↔ Anthropic, MCP fan-out merge, …) ships as concrete
   functions with concrete types — not as a `Translator[ReqT, SpanT]`
   interface. Selection happens via a `Route` value carrying function
   pointers and upstream metadata, not via vtable dispatch. Rationale:
   no mocks needed for unit tests, ~5–7% faster on the hot path,
   reader follows real types instead of generic parameters. See
   `/Users/dio/src/dio/llm-spike/ALT_PATH_PURE_FUNCTIONS.md` and
   `/Users/dio/src/dio/llm-spike/CODE_COMPARISON.md`. Applies to
   WS-I and to WS-F (response-side translation).

## Goal

Turn the repeated orange-shape patterns (LLM and MCP) into SDK primitives
for building a pipeline, not one filter:

- one pipeline-wide config snapshot shared by all extension points;
- typed cross-phase request decisions (model id, profile id, session id);
- SDK-owned async host-selection coordination;
- complete request/response/finalized exchange records (including fan-out
  merge for MCP `tools/list` and `initialize`);
- explicit response observation and mutation modes;
- protocol sidecar lifecycle helpers with egress-via-Envoy as the default;
- bounded Envoy Gateway transport/TLS coordination across all four EG
  integrations;
- research outcome for whether provider/backend host resolution and TLS
  personality can be fully dynamic.

## Non-Goals

- Do not bake protocol-specific concepts (LLM, MCP, anything else) into the
  SDK. Protocol logic lives in examples and consumers.
- Do not invent parallel data paths that bypass Envoy. No sidecar direct
  dial without break-glass rationale; no out-of-band telemetry; no
  side-channel auth.
- Do not fork the SDK per consumer: if LLM and MCP need different shapes of
  the same primitive, the primitive is wrong.
- Do not make xDS the transport for large business configuration.
- Do not hide Envoy phase ordering from SDK users.
- Do not force every feature into one mega-filter.
- Do not replace native Gateway API resources when they are the better
  bounded platform API.

## Planning Assumptions

- Work can proceed in parallel across research, SDK implementation, examples,
  and Envoy Gateway integrations.
- Each SDK primitive lands with unit tests before examples are migrated, and
  proves itself against **both** an LLM and an MCP consumer before its
  public API is frozen.
- Anything that crosses Envoy ABI, cluster host selection, or Envoy Gateway
  xDS requires an e2e proof, not only unit tests.
- Anything that emits or consumes observability (logs/metrics/traces) does
  so through Envoy surfaces; sidecars participate in Envoy-carried trace
  context.
- Polling is the first production-grade config source unless research finds
  a better operational default.
- **Fully dynamic provider TLS: proven — V1 confirmed.** A patched Envoy ABI
  (`add_hosts_with_hostnames`) lets dynamic-module hosts carry a logical FQDN.
  `auto_host_sni` + `auto_sni_san_validation` derive SNI and SAN validation from
  that hostname at connect time. Provider add/remove is application config only
  for ordinary public WebPKI providers. Verdict and e2e evidence in
  `docs/auto-host-sni-verdict.md`.
- Backend + BackendTLSPolicy and `transport_socket_matches` are both Envoy
  config channels. They may be acceptable bounded fallbacks, but the
  preferred outcome is avoiding provider-level xDS updates for catalog
  churn (LLM provider add/remove, MCP server add/remove).

## Phase Layout

Work is organized by **dependency**, not calendar. Phases can overlap; what
matters is that a workstream does not freeze its public API until its
dependencies have resolved. Approximate week ranges are guidance for
sequencing review load, not a contract.

```
Phase 0  ─── WS-B (TLS spike) ─────────────────┐
             gates WS-D API, gates WS-H path   │
                                               ▼
Phase 1  ─── WS-A (config) ────┬── WS-C (rendezvous) ────┐
             no WS-B coupling │   no WS-B coupling    │
                              │                       ▼
Phase 2                       ├─────► WS-D (async host selector)
                              │       needs A+C; HostPtr shape from B
                              └─────► WS-I (Translate stage)
                                      needs C; pure-function translators
Phase 3  ─── Observe phase: WS-E (exchange) + WS-F (response translate)
             ── WS-G (sidecar)
             independent surface work; can parallelize with Phase 2
Phase 4  ─── WS-H (EG transport)
             chosen path determined by WS-B outcome
```

The three-phase model from the README (Classify / Translate / Observe)
maps onto the workstreams as: Classify ≈ WS-C consumer code in
examples; Translate = WS-I (with WS-D for async host selection slotted
between 2a and 2b on the LLM side); Observe = WS-E + WS-F together.

### Phase 0 — Research Spike (start first, blocks nothing else from starting but reshapes APIs)

| Workstream | Output | Exit Criterion |
| --- | --- | --- |
| [WS-B](#workstream-b) | Extends [`integrations/cluster-async-router-eg`](../../integrations/cluster-async-router-eg) (already TLS-aware, already exercises async body-driven selection via `Pending`, already uses `transport_socket_matches` + `EnvoyPatchPolicy`). Adds two HTTPS upstreams with distinct certs; experiments for `auto_host_sni`, `auto_sni`, generic TLS, SDS, hostname-preserving `HostSpec`. | **EXIT CRITERION MET — Verdict: V1.** `docs/auto-host-sni-verdict.md`. Patched ABI (`add_hosts_with_hostnames`) + `HostSpec.Hostname` + `auto_host_sni`/`auto_sni_san_validation` proven across `e2e-static-tls`, `e2e-static-tls-matches`, and canonical `e2e`. Provider add/remove is config-only for public WebPKI. Remaining experiments (5–9: header-SNI timing, EG native API, Cohere-style add) transfer to WS-H. |

**Starting harness.** Do **not** build a greenfield e2e. The deltas on
`cluster-async-router-eg` are: (a) second upstream with distinct cert and
SAN, (b) toggleable `auto_host_sni` config, (c) `:authority` rewrite from
body phase to test the `auto_sni` router timing trap, (d) hostname-bearing
`HostSpec` if ABI permits, (e) SDS data source variant.

**Until WS-B has a verdict:** WS-A, WS-C, WS-E/F/G may proceed; WS-D and
WS-H must stay behind `internal/` packages.

#### WS-B Verdict Forks — Downstream Workstream Shifts

| If WS-B verdict is… | WS-D shifts | WS-G shifts | WS-H shifts |
| --- | --- | --- | --- |
| **V1 — fully dynamic host+TLS ✅ CHOSEN** | `HostPtr` carries hostname; `AsyncHostSelector[T]` lookup is `func(T) (host, sni)`. | No change. | Implements stable generic transport; provider/server add/remove is config-only across all four EG integrations. |
| ~~V2 — bounded TLS families~~ | ~~`HostPtr` carries host + family id; selector exposes family for `transport_socket_matches` metadata.~~ | ~~No change.~~ | ~~Implements Backend + BackendTLSPolicy per family; new family = two-channel rollout (config + Gateway resource).~~ |
| ~~V3 — sidecar direct dial~~ | ~~`HostPtr` shape is unconstrained; selector still exists for the non-sidecar path.~~ | ~~**WS-G grows scope**: direct-dial mode is no longer "break glass," it is a production path with mandatory rationale labeling per call site.~~ | ~~Documents the limit of Envoy-owned TLS; ships the bounded fallback for non-sidecar providers.~~ |

Plan execution does not block on which verdict lands — only WS-D and WS-H
freeze public API on the result.

### Phase 1 — Foundations (parallel, no WS-B coupling)

| Workstream | Output | Exit Criterion |
| --- | --- | --- |
| [WS-A](#workstream-a) | `PipelineConfig[T]` with static/file/polling sources; last-good; observer hooks; field-tagged secret redaction. | Orange config migration green (file + polling) **and** `mcp-profile-gateway` reads `MCP_PROFILE_GATEWAY_CONFIG` through file source via the same primitive. (Polling is not required for MCP fit; see [mcp-fit-notes WS-A](mcp-fit-notes.md#ws-a--pipelineconfigt-good-with-caveats).) |
| [WS-C](#workstream-c) | `StreamKey[T]`, `StreamPromise[T]`, typed helpers over the stream-object bag. | Orange `pending` package deleted; `mcp-profile-gateway` `tools/call` decision (session decode + tool→server resolution + credential ref) carried by a typed `StreamKey`; e2e body-driven routing green on both. |

### Phase 2 — Integration (depends on Phase 0 verdict + Phase 1 foundations)

| Workstream | Output | Exit Criterion |
| --- | --- | --- |
| [WS-D](#workstream-d) | `AsyncHostSelector[T]` owning `ClusterLBCompletion` lifecycle + cancellation; uses WS-C promise and WS-A config. | Orange hostpick reduces to decision-to-host map; MCP **cluster-router** (the host selector behind `mcp-profile-tiered-router-eg`, not the catalog-router which only injects the routing key) uses the same primitive; e2e abort / unknown-model / unknown-server / success green. See [mcp-fit-notes WS-D](mcp-fit-notes.md#ws-d--asynchostselectort-partial). |
| [WS-I](#workstream-i) | Translate stage: 2a `RouteFor(schema, endpoint, override) Route` selecting pure translator functions + upstream metadata; 2b applies the translator on the complete body; `BackendAuthHandler` runs after 2b (SigV4 over mutated body). | Orange has request-side translation in `up/` (no per-translator interface; `Route` value carries function pointers and `UpstreamCluster`/`UpstreamPath`); MCP `tools/call` Translate emits per-leg request with `{prefix}__{tool}` resolved to owning server slug + credential injected; e2e green for both. See README "The Three Phases" and `/Users/dio/src/dio/llm-spike/ALT_PATH_PURE_FUNCTIONS.md`. |
| [Host Refresh](host-refresh-loop.md) | Snapshot refresh helper for Cluster Extension host sets; atomic publication, last-good. | Generic across orange and at least one second consumer or covered by SDK-level tests. |

### Phase 3 — Surface (parallel with Phase 2; independent of WS-B)

The first three rows (WS-E, WS-E.fan, WS-F) together form the **Observe
phase** from the README. They do real work — response translation,
token-usage extraction, fan-out merge, span finalization — not just
record accumulation. WS-F's response translators follow Principle 6
(pure functions, not interfaces).

| Workstream | Output | Exit Criterion |
| --- | --- | --- |
| [WS-E](#workstream-e) *(Observe — exchange side)* | `ExchangeHooks[T]` + `WithExchangeObserver[T]`; request/response/finalized accumulator for the 1:1 case. | request-ui migrated; MCP `tools/call` (1:1) uses the helper; success, local-reply, upstream-failure e2e green; observability flows through Envoy access-log/stats (per `examples/observability`). Fan-out merge does **not** fold into base `ExchangeHooks` — see WS-E.fan below. |
| WS-E.fan (deferrable to Phase 3.5) *(Observe — fan-out side)* | Fan-out merge layer on top of `ExchangeHooks[T]`: per-leg accumulators, aggregate finalize, user-supplied merge func, partial-failure policy. | `mcp-profile-gateway` `initialize` and `tools/list` use the helper; partial-failure policy observable; `HTTPCalloutAllSettled` integration green. Can ship after Phase 3 if it would slow it. See [mcp-fit-notes WS-E](mcp-fit-notes.md#ws-e--exchange-observer-weak-for-fan-out-good-for-11). |
| [WS-F](#workstream-f) *(Observe — response translate side)* | Explicit streaming observer vs buffered mutator APIs; bounded head/tail helpers; response translators as pure functions paired with their request-side counterparts in WS-I `Route` values. | sse-tap green on streaming observer; MCP `tools/list` merge green on buffered mutator (via WS-E.fan); one buffered JSON mutation example green; token-usage extraction lands as a pure function reused across providers. |
| [WS-G](#workstream-g) | Sidecar lifecycle helper (bind/readiness/shutdown/session record); **egress-via-Envoy is the default dial path**, not an option. Trace context propagation through sidecars per `examples/trace-propagation`. Also delivers the SDK shape for MCP streaming transport (SSE / streamable-HTTP) — see WS-G MCP exit below. | ws-proxy migrated; `integrations/tiered-ws-proxy-eg` e2e proves egress-via-Envoy; any direct-dial mode ships with break-glass rationale. |
| WS-G MCP streaming sidecar | MCP SSE / streamable-HTTP sidecar built on the WS-G helper. Sidecar terminates the stateful stream and exposes a **stateless header-keyed HTTP surface** to Envoy (Envoy AI Gateway-style session-via-headers; no server-side session store). Egress back through Envoy. | New example sidecar + new EG integration `integrations/mcp-streaming-sidecar-eg` e2e green; client SSE → sidecar → stateless HTTP → Envoy → upstream proven; trace headers propagated; session encoded into header envelope. This is how the stack gains MCP streaming support at all. See [mcp-fit-notes WS-G](mcp-fit-notes.md#ws-g--protocol-sidecars-critical-not-yet-built). |

### Phase 4 — Transport (gated on WS-B verdict)

| Workstream | Output | Exit Criterion |
| --- | --- | --- |
| [WS-H](#workstream-h) | Implementation of whichever path WS-B selected: stable generic transport, Backend + BackendTLSPolicy, or `transport_socket_matches` baseline. Sidecar direct dial documented as escape hatch with rationale. All four EG integrations (`tiered-router-eg`, `tiered-ws-proxy-eg`, `cluster-async-router-eg`, `mcp-profile-tiered-router-eg`) updated to use the chosen path. | EG e2e green on all four integrations; provider-add and MCP-server-add runbooks documented (config-only vs two-channel); egress-via-Envoy preserved in `tiered-ws-proxy-eg`. |

### Cross-cutting hardening (final pass)

- Full [verification matrix](#cross-cutting-verification-matrix) green.
- Migration notes for each replaced orange-local primitive.
- Secret redaction and bounded output for all debug endpoints.
- Goroutine and stream-object leak tests under abort + local-reply paths.
- Residual risks documented.

<a id="workstream-a"></a><a id="workstream-a-pipeline-configuration"></a>
## Workstream A: Pipeline Configuration

### Research

- Define the minimum snapshot metadata: version, source, fetched time, published
  time, checksum, last error, age, and stale flag.
- Decide whether decoded snapshots are immutable by convention or enforced by
  cloning/copy-on-write helpers.
- Decide where eager stashing belongs: decoder, `PipelineConfig`, or cache
  package.
- Decide debug dump policy for huge snapshots and secret redaction.

### Development

1. Add `up.PipelineConfig[T]`.
2. Add `up.ConfigSource` and `up.ConfigDecoder[T]`.
3. Add static source.
4. Add file source.
5. Add polling source:
   - interval;
   - timeout;
   - jitter;
   - backoff;
   - last-good behavior;
   - refresh observer hooks.
6. Add optional bounded LRU helper or cache interface.
7. Add metrics/log hooks for refresh success, error, age, and version.
8. Wire orange config through one shared pipeline runtime.

### Verification

- Unit: initial snapshot is visible before refresh.
- Unit: successful refresh publishes a whole new snapshot.
- Unit: fetch error keeps last-good snapshot.
- Unit: decode/validation error keeps last-good snapshot.
- Unit: readers never observe a partially updated snapshot.
- Unit: `Snapshot()` is safe under concurrent refresh/read.
- Unit: polling stops when pipeline/group stops.
- Unit: observer receives success/error events with version and duration.
- Example: orange classify, hostpick, and translate read the same version.
- E2E: config server changes model/provider mapping without Envoy restart.
- E2E: invalid config update does not break existing traffic.

### Acceptance Criteria

- Request path never performs remote config fetch.
- All orange extension points read one shared config snapshot.
- Last-good behavior is covered by tests.
- Huge config debug output is redacted and bounded.

<a id="workstream-b"></a><a id="workstream-b-dynamic-provider-host-resolution-and-tls"></a>
## Workstream B: Dynamic Provider Host Resolution And TLS

### Research

Answer these questions:

- Can a Cluster Extension add hosts by hostname rather than only resolved IP
  while preserving enough identity for Envoy to derive SNI?
- Does `auto_host_sni` work with dynamic-module Cluster Extension hosts?
- Does `auto_host_sni` still work when host selection completes asynchronously
  via `ClusterLBCompletion`?
- Does automatic SAN validation work correctly with host-derived SNI?
- Can one static `transport_socket` with `auto_host_sni: true` and
  `auto_sni_san_validation: true` replace provider-specific
  `transport_socket_matches` entirely for ordinary public TLS providers?
- Does `auto_sni` work if `:authority` is changed from the body phase, or does
  it hit the known router timing trap?
- Can SDS provide enough dynamic validation context without unbounded cluster
  transport config?
- Can Envoy Gateway express the generic TLS context without listing every
  provider?
- Where does Backend + BackendTLSPolicy stop being enough?
- Can provider add/remove happen entirely through pipeline config, without
  reconciling Gateway resources or cluster transport socket config?
- If xDS coordination is unavoidable, what is the smallest bounded object set
  that must be coordinated?
- Concrete case: what happens when adding Cohere as a new provider family?
  Can it be only a pipeline config update, or does it require Gateway/xDS
  transport changes?

### Experiments

1. Local Envoy e2e with one dynamic-module cluster, two HTTPS upstreams, and
   static `transport_socket_matches` baseline.
2. Same e2e with one generic TLS context and no `transport_socket_matches`:
   static cluster `transport_socket`, `auto_host_sni: true`,
   `auto_sni_san_validation: true`, and a shared trusted CA.
3. Same e2e with async body-driven host selection.
4. Same e2e with hostnames preserved in `HostSpec` if ABI supports it.
5. Same e2e with `auto_sni` from `:authority` and from override header.
6. Envoy Gateway e2e expressing the generic TLS context through native Gateway
   APIs if possible.
7. Envoy Gateway e2e comparing bounded Backend + BackendTLSPolicy resources
   against transport_socket_matches EPP.
8. Negative/control experiment showing what currently forces xDS coordination
   when a new provider has a distinct TLS identity.
9. Cohere-style provider addition experiment:
   - add provider endpoint and model match in pipeline config;
   - no Gateway resource change;
   - no Envoy restart;
   - verify successful TLS origination if generic transport claims to support
     public WebPKI providers.

### Verification

- E2E: upstream A and B present different certs.
- E2E: each upstream rejects the wrong SNI.
- E2E: body-driven async selection reaches the right TLS upstream.
- E2E: a single static TLS `transport_socket` derives SNI and SAN validation
  from the selected dynamic-module host, with no provider-specific
  `transport_socket_matches` in the cluster config.
- E2E: SAN validation fails when cert SAN does not match selected host.
- E2E: config can add a provider hostname without Envoy restart if the chosen
  design claims full data-plane dynamism.
- E2E: config can add a provider hostname without Gateway resource or xDS
  update if the chosen design claims provider-level xDS avoidance.
- E2E: Cohere-style provider addition works as pipeline-config-only if the
  chosen design claims generic public-provider support.
- E2E: if the design is bounded, adding an unconfigured TLS personality fails
  clearly and is documented.
- Config dump: cluster transport config remains bounded.
- Envoy stats/logs: no unexpected `upstream_cx_connect_fail` on valid cases.

### Acceptance Criteria

- We can state one of:
  - fully dynamic host + TLS personality is proven and documented;
  - dynamic host resolution is supported, but TLS personalities are bounded;
  - fully dynamic TLS requires sidecar direct dial and does not preserve Envoy
    TLS ownership.
- We can state whether provider add/remove requires xDS/controller
  coordination, and exactly which resource type owns that coordination.
- We can state the operational runbook for adding a new provider family such as
  Cohere, including whether it is config-only or a two-channel rollout.
- The chosen production recommendation has an Envoy and Envoy Gateway e2e.

<a id="workstream-c"></a><a id="workstream-c-typed-rendezvous"></a>
## Workstream C: Typed Rendezvous

### Research

- Should stream keys be generic functions or generic wrapper types?
- Should stream keys be SDK-global strings or strongly scoped by pipeline?
- Should missing typed value return zero/false or a typed error?
- Should stream promises allow one callback or multiple callbacks?
- Should `OnResolve` return a cancellation function?

### Development

1. Add `StreamKey[T]`.
2. Add typed helpers over existing stream-object bag.
3. Add `StreamPromise[T]`.
4. Port `examples/orange/pending` tests to SDK tests.
5. Update orange to use typed `Decision`.
6. Remove local orange `pending` package after host selector lands.

### Verification

- Unit: typed set/get round trips from `Writer`.
- Unit: typed set from `Writer` is readable from `ClusterLBContext`.
- Unit: wrong key type cannot compile in normal use.
- Unit: promise first resolve wins.
- Unit: callback fires if resolve happens after registration.
- Unit: callback fires inline if already resolved.
- Unit: callback cancellation prevents callback.
- Unit: stream-object bag still drains on stream completion.
- E2E: orange body-driven routing still works after migration.

### Acceptance Criteria

- Orange no longer owns a custom pending primitive.
- No process-wide orange pending registry exists.
- Existing stream-object cleanup guarantees remain intact.

<a id="workstream-d"></a><a id="workstream-d-async-host-selector"></a>
## Workstream D: Async Host Selector

### Research

- Should the selector be part of `up`, or a helper package under `up/cluster`?
- What is the default behavior when the promise is missing?
- Should host lookup be supplied as `func(T) (HostPtr, string)` or an
  interface with richer observability?
- Should cancellation remove callbacks, mark completions canceled, or both?

### Development

1. Add `AsyncHostSelector[T]`.
2. Own `ctx.NewCompletion()`.
3. Register promise callback.
4. Schedule completion through `ClusterHandle.Schedule`.
5. Guard completion after `CancelHostSelection`.
6. Add observer hooks for selected, failed, canceled, and missing-decision.
7. Replace orange hostpick cancellation map.

### Verification

- Unit: already-resolved promise completes synchronously via scheduled path.
- Unit: later-resolved promise completes once.
- Unit: cancellation before resolve prevents `Complete`.
- Unit: cancellation after resolve is harmless.
- Unit: missing promise follows documented behavior.
- Unit: host lookup error maps to `errDetail`.
- E2E: client abort after headers does not leak pending or goroutine.
- E2E: unknown model completes host selection with error detail.
- E2E: successful model reaches expected upstream.

### Acceptance Criteria

- App code owns only decision-to-host mapping.
- SDK owns completion lifecycle, scheduling, and cancellation.

<a id="workstream-i"></a><a id="workstream-i-translate-stage"></a>
## Workstream I: Translate Stage

The Translate phase from the README, made concrete. Splits cleanly into
**2a** (select) and **2b** (apply), with an auth handler running after
2b. Follows Principle 6 (pure functions, not interfaces).

Reference: `/Users/dio/src/dio/llm-spike/AI_GATEWAY_EXTPROC_PHASES.md`,
`/Users/dio/src/dio/llm-spike/TWO_TRANSLATE_STAGES_EXPLAINED.md`,
`/Users/dio/src/dio/llm-spike/BACKEND_AND_SCHEMA_SELECTION.md`,
`/Users/dio/src/dio/llm-spike/AUTHENTICATION_PATTERNS.md`,
`/Users/dio/src/dio/llm-spike/ALT_PATH_PURE_FUNCTIONS.md`,
`/Users/dio/src/dio/llm-spike/TRANSLATORS_QUICK_REFERENCE.md`.

### Research

- Should the `Route` value live alongside the `StreamKey` decision, or
  be selected at the upstream filter from `(endpointSpec, schema,
  modelNameOverride)` re-derived from headers + config?
- Where does endpoint-spec live? `up/translate/endpoint.go` or
  per-protocol packages that register pure functions at init?
- Auth handler interface vs pure function: SigV4 has state
  (credentials provider, signer) — does the principle still apply, or
  is auth the documented exception?
- For MCP, is the per-leg request build (one per fan-out member) a
  Translate or an Observe concern? Plan: Translate produces N legs,
  WS-E.fan owns the merge.

### Development

1. Add `up/translate` with a `Route` value:
   - `TranslateRequest func(body []byte, headers http.Header) (*TranslationResult, error)`
   - `TranslateResponse func(body []byte, headers http.Header) (*TranslationResult, error)`
   - `UpstreamCluster string`, `UpstreamPath string`
2. Add `RouteFor(schema APISchemaName, endpoint EndpointSpec, override ModelOverride) Route`
   — the discriminator. Switch over schema constants.
3. Port a minimum set of pure translator functions to prove the shape:
   `TranslateOpenAIToOpenAI`, `TranslateOpenAIToAWSBedrock` request-side.
   Their response-side counterparts land under WS-F.
4. Add `BackendAuthHandler` (interface is fine here; auth is stateful):
   `Do(ctx, headers, body) error`. Wire after 2b.
5. Wire orange's classify-to-translate handoff so the `StreamKey[Decision]`
   carries the `Route` (or enough to compute it).
6. For MCP, the Translate stage emits a `Route` (or N routes for
   fan-out) carrying L2 cluster + `x-mcp-server` header injection +
   credential ref.

### Verification

- Unit: `RouteFor(OpenAI, ChatCompletions, "")` returns the OpenAI→OpenAI route.
- Unit: `RouteFor(AWSBedrock, ChatCompletions, "anthropic.claude-…")` returns Bedrock route with `UpstreamCluster` set.
- Unit: pure-function translator round-trips a known fixture (no mocks).
- Unit: `BackendAuthHandler` for SigV4 signs over the body *after* 2b mutation.
- E2E: orange request reaches the right upstream with the right body shape and the right auth header.
- E2E: MCP `tools/call` lands at L2 with `x-mcp-server` set and credential injected; `{prefix}__{tool}` resolves to the owning server.

### Acceptance Criteria

- No `Translator[ReqT, SpanT]` interface ships. The dispatch is a
  `switch` on `APISchemaName` returning a `Route` value.
- Request-side translators have no test mocks.
- Auth runs after the translator, not in parallel, and the SigV4 case
  has e2e coverage proving it signs over the mutated body.
- The `Route` value is the single thing carried across the Classify →
  Translate handoff (via `StreamKey` from WS-C).

<a id="workstream-e"></a><a id="workstream-e-exchange-observer"></a>
## Workstream E: Exchange Observer

### Research

- Should the SDK allocate state via `sync.Pool` or let users provide state?
- Should request and response body capture be built in or opt-in helpers?
- How should finalized info merge with local replies and upstream failures?
- How should records include typed request decisions and response usage?

### Development

1. Add `ExchangeHooks[T]`.
2. Add `WithExchangeObserver[T]`.
3. Ensure same accumulator reaches request, response, and final callbacks.
4. Add body truncation helper.
5. Add local-reply and upstream-failure coverage.
6. Port or mirror request-ui to use the helper.

### Verification

- Unit: request callback initializes state.
- Unit: response headers update same state.
- Unit: response body truncates at configured limit.
- Unit: finalized callback receives accumulated state and `FinalizedInfo`.
- Unit: local reply path produces a complete record.
- Unit: upstream failure path produces a complete record.
- Unit: downstream disconnect path finalizes safely.
- E2E: request-ui records success, local reply, and upstream failure.

### Acceptance Criteria

- Complete exchange records no longer require hand-wired callback plumbing.
- Request-ui behavior remains equivalent.

<a id="workstream-f"></a><a id="workstream-f-response-modes"></a>
## Workstream F: Response Modes

### Research

- Which modes are safe with current ABI: streaming observe, buffered mutate,
  finalized-only?
- Should streaming transform be supported now or deferred?
- What backpressure semantics are possible?
- How should content encoding interact with mutation?

### Development

1. Add explicit streaming response observer API or clarify existing API names.
2. Add buffered response mutator API if current mutable response support is too
   implicit.
3. Add helpers for bounded head/tail buffering.
4. Validate sse-tap against streaming observer mode.
5. Add a small buffered JSON response mutation example.

### Verification

- Unit: streaming observer sees headers and chunks without replacing body.
- Unit: head/tail buffer bounds memory.
- Unit: buffered mutator replaces body and content length correctly.
- Unit: observer mode cannot accidentally mutate already-forwarded bytes.
- E2E: sse-tap token usage still emits counters/metadata.
- E2E: buffered mutation changes response body as expected.

### Acceptance Criteria

- SDK users choose response semantics explicitly.
- Streaming observation and body mutation are not confused in examples.

<a id="workstream-g"></a><a id="workstream-g-protocol-sidecars"></a>
## Workstream G: Protocol Sidecars

### Research

- Should sidecar helpers live in `up` or an examples/internal package first?
- What is the minimum lifecycle helper: bind, readiness, shutdown, session
  deadline, records?
- How should egress-via-Envoy be configured and validated?
- Can MCP and WebSocket share a common sidecar lifecycle helper?

### Development

1. Add sidecar lifecycle helper around `RegisterWithGroup`.
2. Add listener readiness and graceful shutdown.
3. Add session context/deadline helper.
4. Add session record hook.
5. **Envoy egress dial helper is the default**, not optional. Direct dial
   exists only as a labeled escape hatch with required rationale field.
6. Trace context propagation: sidecar accepts and forwards Envoy-carried
   trace headers per `examples/trace-propagation`.
7. Migrate ws-proxy (LLM) and mcp-profile-gateway session/aggregator (MCP).
8. Validate egress-via-Envoy through `integrations/tiered-ws-proxy-eg`.

### Verification

- Unit: sidecar starts and stops with group.
- Unit: readiness blocks until listener is bound.
- Unit: shutdown closes listener and active sessions by deadline.
- Unit: session record hook fires on normal close and error close.
- Unit: trace headers propagate through sidecar to upstream call.
- E2E: WebSocket `/v1/responses` proxy still forwards frames.
- E2E: egress-via-Envoy is the path taken by default; TLS/auth/access-log
  remain Envoy-owned.
- E2E: MCP sidecar can inspect and route protocol messages.
- E2E: `integrations/tiered-ws-proxy-eg` proves the egress-via-Envoy dance.
- E2E: any direct-dial mode emits a startup log naming the missing Envoy
  capability and the rationale.

### Acceptance Criteria

- Embedded protocol servers do not repeat lifecycle boilerplate.
- Egress-via-Envoy is the documented default; direct dial is the labeled
  escape hatch with rationale.
- Sidecars participate in Envoy-carried trace context, not a parallel one.

<a id="workstream-h"></a><a id="workstream-h-envoy-gateway-transport-integration"></a>
## Workstream H: Envoy Gateway Transport Integration

### Research

- For bounded providers, can Backend + BackendTLSPolicy fully replace
  transport_socket_matches?
- Does EG materialize one cluster per Backend, and is that acceptable?
- Can Transit route to bounded backend families without unbounded cluster
  growth?
- Does EPP remain required for CLUSTER_PROVIDED dynamic-module clusters?
- What controller coordination is required between pipeline config and EG xDS?
- Can provider-level Gateway/xDS coordination be avoided entirely by using a
  stable generic transport config?
- If not, can coordination be limited to bounded provider families rather than
  every provider profile?

### Development

1. Keep current transport_socket_matches EPP integration as baseline.
2. Add a Backend + BackendTLSPolicy integration variant if feasible.
3. Add a generic-transport experiment that tries to avoid provider-level xDS
   updates.
4. Add controller-shape notes only for the fallback case where bounded
   transport identities still require reconciliation.
5. Document when to choose each path.

### Verification

- E2E: EPP transport_socket_matches path still passes SNI tripwire
  (`cluster-async-router-eg`).
- E2E: Backend + BackendTLSPolicy path passes SNI/SAN validation if
  implemented (`tiered-router-eg`).
- E2E: egress-via-Envoy path preserved end-to-end (`tiered-ws-proxy-eg`).
- E2E: MCP tiered topology (`mcp-profile-tiered-router-eg`) routes L1→L2
  through the chosen transport path.
- Config dump: cluster/backend count remains bounded across all four
  integrations.
- E2E: dynamic business config changes do not require xDS update.
- E2E: provider add/remove (LLM) and server add/remove (MCP) do not require
  xDS update if the generic transport experiment succeeds.
- E2E: adding a new bounded provider family requires expected Gateway
  resource reconciliation and then works.

### Acceptance Criteria

- Production recommendation is explicit:
  - generic stable transport config when it can safely avoid provider-level xDS
    coordination;
  - Backend + BackendTLSPolicy for naturally bounded provider families;
  - transport_socket_matches when a single dynamic cluster is required;
  - sidecar direct dial only as an explicit escape hatch.

<a id="cross-cutting-verification-matrix"></a>
## Cross-Cutting Verification Matrix

| Area | Unit | Example e2e | Envoy Gateway e2e | Notes |
| --- | --- | --- | --- | --- |
| Pipeline config | required | required | optional | EG only needed when config affects transport. |
| Dynamic host/TLS | required for helpers | required | required | Must prove SNI/SAN behavior. |
| Stream rendezvous | required | required | optional | Local Envoy is enough unless EG timing differs. |
| Async host selector | required | required | optional | EG coverage through cluster-async-router-eg. |
| Translate stage (WS-I) | required | required | required | Pure-function translators; SigV4 e2e proves auth-after-translate ordering; MCP `tools/call` e2e proves `{prefix}__{tool}` resolution + credential injection. |
| Exchange observer | required | required | optional | request-ui is primary proof. |
| Response modes | required | required | optional | sse-tap plus mutation example. |
| Sidecars | required | required | required for egress-via-Envoy | ws-proxy + MCP streaming sidecar. |
| Observability & tracing | required | required | required for sidecar paths | Every workstream's e2e must show signals flowing through Envoy access-log/stats/traces (`examples/observability`); sidecars must propagate Envoy-carried trace context (`examples/trace-propagation`). Regression = release blocker. |
| EG transport | optional | optional | required | Needs k3d gated suite across all four EG integrations. |

## Commands

Run focused unit tests while developing:

```sh
go test ./up -count=1
cd examples && GOWORK=off go test ./orange/... -count=1
cd examples && GOWORK=off go test ./request-ui/... -count=1
cd examples && GOWORK=off go test ./sse-tap/... -count=1
cd examples && GOWORK=off go test ./ws-proxy/... -count=1
```

Run focused example e2e tests after SDK behavior crosses Envoy:

```sh
make -C examples/orange e2e
make -C examples/request-ui e2e
make -C examples/sse-tap e2e
make -C examples/ws-proxy e2e
```

Run Envoy Gateway suites only when explicitly gated:

```sh
RUN_CLUSTER_ASYNC_ROUTER_EG_E2E=1 make -C integrations/cluster-async-router-eg e2e
```

## Release Gates

Before marking the SDK work usable:

- all affected `up` unit tests pass;
- migrated examples pass unit and e2e tests;
- at least one Envoy Gateway e2e proves the chosen transport/TLS story;
- docs explain phase ordering and transport boundaries;
- orange no longer contains custom one-off versions of SDK primitives unless
  deliberately kept as example-local business logic;
- debug endpoints redact secrets and bound output size;
- config refresh failure modes are covered and observable;
- goroutine and stream-object leak tests pass under abort and local-reply paths.

## Risks

- **Parallel-data-path temptation.** The fastest way to ship a feature is
  often to skip Envoy (sidecar direct dial, in-extension metrics, custom
  access log). Each such shortcut violates Principle 1 and accumulates
  invisible debt. Mitigation: PR template asks "does this introduce a
  data path that bypasses Envoy?" and the answer must be no or
  accompanied by break-glass rationale.
- **Fully dynamic TLS may not be viable** while preserving Envoy
  ownership. Mitigation: WS-B verdict forks pre-named; fallback paths
  are real, not theoretical.
- **MCP fan-out merge semantics under partial failure** (WS-E.fan): how
  does one timed-out leg interact with `tools/list` merge ordering, with
  `initialize` session-envelope encoding, with the client's view of
  partial success? Underspecified today. Mitigation: WS-E.fan ships
  with explicit policy enum, not implicit behavior.
- **MCP streaming sidecar reconnect / backpressure** (WS-G MCP): SSE
  client reconnect via Last-Event-ID, backend reconnect, slow-client
  buffering, who owns retry. Mitigation: design note before
  implementation; cross-check against `examples/trace-propagation`
  invariants.
- **EG version skew across four integrations.** `tiered-router-eg`,
  `tiered-ws-proxy-eg`, `cluster-async-router-eg`,
  `mcp-profile-tiered-router-eg` (and the new
  `mcp-streaming-sidecar-eg`) may pin different EG versions and
  generated xDS names may drift. Mitigation: lock EG version in one
  place; e2e gate catches per-version breakage early.
- **Multiple control channels** can make operations brittle: pipeline
  config, Gateway resources, xDS, SDS, and dynamic-module config must
  not all be required for ordinary provider/server catalog updates.
- **Generic APIs may become too abstract** if designed before migrating
  one real example. Mitigation: the "two consumers before public API
  freeze" rule in Planning Assumptions.
- **Polling provider semantics can be underspecified** around partial
  failures (slow source, decode error mid-refresh, observer firing
  policy).
- **Sidecar helpers can obscure** the fact that sidecars are heavier
  than filters — every sidecar adds a process, a port, a lifecycle, and
  a failure mode. The helper should make the cost visible, not hide it.

## Immediate Next Steps

### First Three PRs

The plan executes one PR at a time. The first three are concrete; each is
small enough to land in a day, large enough to prove the foundation.

1. **PR #1 — WS-B harness delta on `cluster-async-router-eg`.**
   Add a second HTTPS upstream with a distinct cert + SAN, a toggle for
   `auto_host_sni`, and an e2e case that proves the existing path still
   passes while the new upstream is reachable. No `up/` changes. This
   stands up the Phase 0 spike rig without committing to a verdict.
   Owner: TBD.

2. **PR #2 — `up.PipelineConfig[T]` with static source only.**
   Generic `PipelineConfig[T]`, `ConfigSource`, `ConfigDecoder[T]`,
   static source, atomic snapshot, last-good. No file source, no
   polling, no observer hooks yet. Unit tests only. Establishes the
   public type surface. File source lands in PR #4. Owner: TBD.

3. **PR #3 — `up.StreamKey[T]` + `up.StreamPromise[T]` with unit
   tests.** Typed wrappers over the existing stream-object bag; no
   consumer migration in this PR. Locks the typed-rendezvous surface so
   orange and MCP migrations (later PRs) target a stable API. Owner:
   TBD.

After PR #3 the plan is **executable in parallel**: WS-A polling source,
WS-B experiments, orange migration to `StreamKey`, and the MCP fit
validation can all run independently.

### Standing decisions deferred to first PRs

1. Whether orange config migration should happen before or after
   `AsyncHostSelector[T]` — deferred until PR #4 (WS-A file source +
   first migration). Default: orange config first, then `AsyncHostSelector`.
2. Which EG transport experiment lands first — deferred until WS-B
   verdict. Default if no clear signal: generic TLS + `auto_host_sni`,
   falling back to Backend + BackendTLSPolicy.
