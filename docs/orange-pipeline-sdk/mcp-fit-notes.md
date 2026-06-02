# MCP Fit Notes

Status: pre-execution audit. Answers, for each workstream that claims an MCP
consumer in [`plan.md`](plan.md), whether the existing MCP example's pattern
maps cleanly onto the proposed `up/` primitive, or whether the primitive's
shape would have to bend to fit MCP.

Sources audited:

- [`examples/mcp-profile-gateway`](../../examples/mcp-profile-gateway) — L1
  public gateway; dynamic-module filter; reads
  `MCP_PROFILE_GATEWAY_CONFIG` env JSON; uses `HTTPCallout` and
  `HTTPCalloutAllSettled` for L2 egress.
- [`examples/mcp-catalog-router`](../../examples/mcp-catalog-router) — L2
  catalog front end; dynamic-module filter; reads
  `MCP_CATALOG_ROUTER_CONFIG` env JSON; header-driven (`x-mcp-server`);
  forwards to a fixed egress URL per server.
- [`examples/mcp-profile-router`](../../examples/mcp-profile-router) —
  status: **rewrite plan**. README is a spec for a future shape, not a
  current implementation. Treat as design input, not a migration target.

Verdict legend: **GOOD** (lands cleanly) · **PARTIAL** (lands with
caveat) · **WEAK** (primitive's shape must change or MCP claim should be
withdrawn) · **NONE** (no current MCP fit).

## Summary

| WS | Primitive | MCP fit | Action |
| --- | --- | --- | --- |
| A | `PipelineConfig[T]` | **GOOD (with caveats)** | Migrate env-JSON loaders to file/polling source. Cluster-name references and credential redaction are constraints, not blockers. |
| C | `StreamKey[T]`, `StreamPromise[T]` | **GOOD** | MCP `tools/call` resolution (session decode + tool→server + credential) is exactly a typed multi-field decision. |
| D | `AsyncHostSelector[T]` | **PARTIAL** | MCP L1 does not pick hosts — it picks named clusters (map lookup, synchronous). The real MCP fit is at L2 cluster-router, which already matches orange's hostpick shape. **Adjust WS-D MCP exit criterion to name cluster-router (used by `mcp-profile-tiered-router-eg`), not L1.** |
| E | `ExchangeHooks[T]` | **WEAK for fan-out, GOOD for 1:1** | Orange-shaped `ExchangeHooks` is request → upstream → finalized. MCP `initialize` and `tools/list` are request → N upstream → merged response. Fan-out merge does not fold into the base hook; **split into a sub-workstream layered on WS-E**, not folded inside it. MCP `tools/call` is 1:1 and lands fine. |
| G | Sidecar lifecycle helper | **CRITICAL — not yet built** | MCP SSE / streamable-HTTP transport is not supported in the current stack. The sidecar is exactly how that gap closes: terminate the long-lived stateful stream in the sidecar, expose a stateless header-keyed HTTP surface to Envoy (the Envoy AI Gateway pattern). WS-G's MCP exit criterion should be "MCP SSE/streamable-HTTP sidecar shipped; sessions reach Envoy as stateless header-keyed requests; egress-via-Envoy preserved." |

## WS-A — `PipelineConfig[T]`: GOOD (with caveats)

**Today.** Both `mcp-profile-gateway` and `mcp-catalog-router` read JSON
from env vars (`MCP_PROFILE_GATEWAY_CONFIG`, `MCP_CATALOG_ROUTER_CONFIG`).
Static at process start. `mcp-profile-gateway` exposes `GET /dump` with
credential/session redaction.

**Fits the primitive cleanly:**

- Decoded snapshot is a Go struct (`CatalogServer`, `Profile`,
  `ProfileServer` already exist for the profile-router rewrite spec).
- File source maps cleanly to "read this JSON path on startup."
- Last-good behavior is what env-var loading already implicitly does (fail
  → keep last in-memory).
- `/dump` redaction is exactly WS-A's secret-redaction acceptance criterion.

**Caveats that constrain the primitive:**

1. **Cluster names in business config.** `catalog_servers[*].cluster`
   references Envoy cluster names. `PipelineConfig[T]` must accept that
   the decoded snapshot can name xDS-managed identifiers without trying to
   reshape clusters from config. Aligns with principle 5 (bounded xDS
   surface).
2. **Credential refs vs raw credentials.** Today the config carries raw
   `credential` strings. The redaction policy must be **field-tagged on
   the user's struct**, not inferred by the SDK. Suggested: a
   `up:"redact"` struct tag honored by `up.RedactedJSON()` /
   `PipelineConfig.DumpRedacted()`.
3. **Polling is a new capability for MCP, not a migration.** Neither MCP
   example polls today. The MCP exit criterion in plan.md
   ("mcp-profile-router or mcp-profile-gateway reads its catalog/profile
   config through the same primitive") is satisfied by file source —
   polling is a separate proof on the LLM side.

**Suggested plan.md tweak:** WS-A's MCP exit criterion should explicitly
read "via file source," not "via polling source." Polling stays
LLM-orange-only for the foundation pass.

## WS-C — Typed Rendezvous: GOOD

**Today.** `mcp-profile-gateway` `tools/call` does (in the filter body
phase): decode `mcp-session-id` envelope → resolve `{prefix}__{tool}` to
owning server slug → fetch backend session id + credential from envelope
→ forward via `HTTPCallout`. That is exactly the orange classify →
hostpick handoff, but with a richer typed value.

**Maps cleanly:**

```go
type ToolsCallDecision struct {
    OwningServer    string         // from prefix resolution
    BackendSession  string         // from envelope decode
    CredentialRef   string         // from profile config
    EnabledTools    map[string]bool
}
var toolsCallKey = up.NewStreamKey[ToolsCallDecision]("mcp.tools-call")
```

**Idiosyncrasies, none blocking:**

- The decision is **multi-field**, not a single id. The orange `Decision`
  is already struct-shaped, so the primitive already supports this.
- The decision happens in the body phase (after envelope decode). Same
  phase semantics as orange.
- `initialize` and `tools/list` do not need a typed decision — they
  immediately fan out, no cross-phase rendezvous.

**Suggested plan.md tweak:** WS-C exit criterion is correct as written
once the typed value is acknowledged as struct-shaped (already implicit).

## WS-D — `AsyncHostSelector[T]`: PARTIAL

This is the most-important correction.

**Today.** `mcp-profile-gateway` does **not** pick hosts. It picks
**named Envoy clusters** (`catalog_servers[slug].cluster`). The mapping is
a synchronous map lookup against pipeline config. Egress goes through
`HTTPCallout` to that cluster; Envoy's standard LB picks the host inside
the cluster.

`mcp-catalog-router` (L2) does the same: it forwards to a fixed egress
URL per server. Again, no async host selection at this layer.

**Where the real MCP fit is:** the L2 **cluster-router** behind
`mcp-profile-tiered-router-eg`. The cluster-router takes the
`x-mcp-server` header injected by the catalog router and picks a
concrete backend host. That is structurally identical to orange's
`hostpick` and **does** use `ClusterLBCompletion`-style async selection.

**Plan.md MCP exit criterion is mis-attributed:**

- ❌ Current: "MCP L2 catalog router (`mcp-catalog-router`) host selection
  uses the same primitive."
- ✅ Correct: "MCP cluster-router (the host-selection extension behind
  `mcp-profile-tiered-router-eg`) uses the same primitive."

The catalog router *injects* the routing key; cluster-router *selects*.
WS-D's MCP target must be the selector, not the injector.

**Suggested plan.md tweak:** edit WS-D Phase 2 row accordingly. Also
verify that `examples/cluster-router` (or its async sibling) is the
shared underlying selector for both LLM and MCP cases — if so, WS-D
migration is one consumer that serves both.

## WS-E — Exchange Observer: WEAK for fan-out, GOOD for 1:1

This is the second-important correction.

**Today.** Two distinct response shapes in MCP:

1. **1:1 (matches orange).** `mcp/s/{slug}` direct catalog calls and
   profile `tools/call` are one client request → one upstream call →
   one response. Standard `ExchangeHooks[T]`.
2. **N-fan-out (does not match orange).** `initialize` and `tools/list`
   on a profile fan out via `HTTPCalloutAllSettled` to all member L2
   endpoints, collect N results, **merge** into a single client
   response. The "record" is N-element; finalization waits on the
   slowest (subject to `timeout_millis` + partial-failure policy).

**Why it does not fold into base `ExchangeHooks[T]`:**

- The accumulator state is *per-fan-out-leg*, not per-request.
- Finalization semantics are different: orange waits for one upstream;
  MCP fan-out waits for N (or N − failures with policy).
- "Local reply" and "upstream failure" are *per-leg* and *per-aggregate*
  — both need to be observable.
- The response observer (WS-F) modes (streaming/buffered) interact
  differently when bytes from N legs must be merged before any client
  byte is written.

**Recommendation:** keep WS-E as the 1:1 primitive. Add a layered
sub-workstream — call it **WS-E.fan** — that:

1. Builds on `ExchangeHooks[T]` (does not replace it).
2. Owns the per-leg accumulator + aggregate finalize.
3. Owns the merge function: `func(legs []LegResult[T]) Response`.
4. Has its own acceptance criterion: `mcp-profile-gateway`
   `initialize` and `tools/list` use it; partial-failure policy
   observable; `tools/call` continues to use base `ExchangeHooks[T]`.
5. Can be deferred to a Phase 3.5 if it would slow Phase 3.

**Suggested plan.md tweak:** split WS-E Phase 3 row into WS-E (1:1,
covers `tools/call`) and WS-E.fan (deferred, covers `initialize` +
`tools/list`). Withdraw the MCP fan-out claim from base WS-E.

## WS-G — Protocol Sidecars: CRITICAL (not yet built)

**Today.** No MCP example is a sidecar. All MCP examples are
dynamic-module filters speaking the **single-request HTTP** flavor of
MCP. The current stack **does not support MCP SSE or streamable-HTTP**.
That gap is the reason WS-G exists for MCP.

**Why the sidecar matters for MCP (and why it ranks alongside ws-proxy):**

The MCP protocol family includes long-lived, stateful transports (SSE
streaming, streamable-HTTP, future stdio bridges). Envoy is excellent at
routing stateless HTTP and adequate at WebSocket/SSE pass-through, but
**session state, stream multiplexing, and protocol-specific framing
should not live inside Envoy filters**:

- Filters cannot hold per-session state across many client requests
  cleanly.
- Envoy's worker model fights long-lived per-session loops.
- Protocol-specific reconnect/replay semantics belong in protocol code,
  not in filter callbacks.

The sidecar pattern — already proven for WebSocket via
[`integrations/tiered-ws-proxy-eg`](../../integrations/tiered-ws-proxy-eg)
— is the right shape: the sidecar terminates the stateful stream,
**translates it into stateless header-keyed HTTP** that Envoy can route
normally, and dials its upstream back through Envoy. The Envoy AI
Gateway approach (session encoded into headers, no server-side session
store) is the reference design.

**What WS-G needs to ship for MCP:**

1. The lifecycle helper (bind, readiness, shutdown, session record) used
   by ws-proxy.
2. A concrete MCP SSE / streamable-HTTP sidecar example built on top.
3. Session-via-headers envelope: the sidecar encodes per-stream backend
   session state into an opaque client-facing header (analogous to
   `mcp-profile-gateway`'s composite `mcp-session-id`), so Envoy sees
   stateless requests.
4. Egress-via-Envoy from the sidecar (principle 1; same dance as
   `tiered-ws-proxy-eg`).
5. Trace context propagation through the sidecar
   (`examples/trace-propagation`).
6. An EG integration analogous to `tiered-ws-proxy-eg` that proves the
   MCP streaming path end to end. Suggested name:
   `integrations/mcp-streaming-sidecar-eg`.

**Suggested plan.md tweak:** WS-G is **upgraded**, not weakened, on the
MCP axis. The Phase 3 row should explicitly name the MCP SSE/streamable
sidecar as a required deliverable, not an "if/when." Recommend adding
this to the plan's "Reference Pipelines" surface and noting that the EG
integration list grows by one.

**Open questions for the MCP sidecar design (deferred from this audit
but needed before WS-G implementation):**

- Single sidecar fan-out vs sidecar-per-server: does the MCP streaming
  sidecar own profile fan-out, or does L1 (filter) still do fan-out and
  each leg goes through its own sidecar? Probably the former, but it
  changes whether the sidecar interacts with WS-E.fan.
- Reconnect semantics: client SSE reconnect (Last-Event-ID) vs backend
  SSE reconnect — who owns retry?
- Backpressure: SSE is push; if the client is slow, do we buffer in the
  sidecar or drop?
- Auth surface: does the sidecar terminate the client TLS+auth, or does
  Envoy terminate and the sidecar trusts headers? Principle 1 prefers
  the latter.

## Net Effect on the Plan

Four targeted edits to `plan.md`:

1. **WS-A exit criterion**: MCP fit is via file source, not polling.
2. **WS-D exit criterion**: MCP target is cluster-router (the selector),
   not catalog-router (the injector).
3. **WS-E**: split into base (1:1, MCP `tools/call`) and WS-E.fan
   (fan-out merge, MCP `initialize` + `tools/list`), with WS-E.fan
   deferrable to Phase 3.5.
4. **WS-G**: **upgrade** the MCP claim. The MCP SSE / streamable-HTTP
   sidecar is the way the stack gains streaming-MCP support at all.
   Add a new EG integration (`integrations/mcp-streaming-sidecar-eg`)
   to the proving-grounds list.

The first three sharpen MCP exit criteria without changing phase shape.
The fourth grows scope: MCP streaming becomes an explicit deliverable of
WS-G rather than a future possibility.

## Open Questions for Implementers

- For WS-A, do we want a `up:"redact"` struct tag or a separate
  `RedactedFor(snapshot) Snapshot` method? The first is less invasive but
  ties redaction to JSON marshalling; the second is more flexible.
- For WS-D, does `examples/cluster-router` (used by
  `mcp-profile-tiered-router-eg`) already share enough shape with
  `examples/orange/hostpick` that one primitive serves both, or are they
  divergent enough that the primitive must be wider?
- For WS-E.fan, is the merge step a user-supplied function, or does it
  decompose further (per-leg headers, per-leg body, aggregate finalize)?
- For WS-G, is it worth defining the MCP-bridge sidecar shape now (so
  the helper accepts it later), or wait until one exists?
