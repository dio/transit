# Orange LLM fallback — feature prompt

**Goal**: ship LLM cross-target fallback for orange. Given a request
that selects a model alias on a known key, orange resolves a chain of
provider targets; on classified transport errors *before*
any response byte has streamed, Envoy retries on the next target.
Streaming bypasses fallback (acknowledged limitation, ch. 16).

Traffic splitting is **out of scope** here — split lands after this.

## Prereqs

**All three are landed** in commit `e2325b9`.

1. `docs/orange-fallback-prelim-1-keys.md` — `keys[id]` blob loading,
   key-id parsing, 401 on unknown key, `LookupModelForKey`.
2. `docs/orange-fallback-prelim-2-bindings.md` — provider `bindings[]`
   in catalog, `Decision.Binding`, `(provider, binding)`-keyed
   `pick.lookupHost`.
3. `docs/orange-fallback-prelim-3-dyn-hosts.md` — full-catalog
   enumeration in `applyResolved` so every `(provider, binding)` pair
   is host-resolved and registered regardless of current model
   references; the Envoy-side `auto_host_sni` + SNI session cache
   substrate is already in place (do not re-add).

## Context

Read these in order before starting:

- `docs/orange-policies-llm-mcp.md` § 1 "LLM routing tree (per-key)"
  and § 1 "Fallback enforcement (the hard part)" — schema, the
  Chain/Split/Target tree, and the no-xDS workaround.
- `pear/design/16-llm-traffic-class.md` — Chain semantics, attribution
  tuple, classified-error retry list.
- `examples/orange/internal/pipeline/match/match.go` — current
  `Decision` shape, body-handler resolution.
- `examples/orange/internal/pipeline/pick/pick.go` — `lookupHost`,
  `applyResolved`. The package doc now records that AddHosts is the
  designated entry point for all runtime hosts.

## Scope

### Config

Add `routing` to the model entry. Existing `provider`/`name`/`binding`
shape stays as sugar for a single-Target leaf:

```yaml
keys:
  ws-prod/user-maya/key-a:
    workspace: ws-prod
    user: user-maya
    llm:
      models:
        claude-haiku-4-5:
          routing:
            chain:
              children:
                - target: { provider: anthropic, name: claude-haiku-4-5 }
                - target: { provider: vertex_anthropic, name: claude-haiku-4-5@20251001 }
```

Types in `internal/config`:

```go
type RoutingNode struct {
    Chain  *ChainNode  `yaml:"chain,omitempty"`
    Target *TargetLeaf `yaml:"target,omitempty"`
    // Split *SplitNode — deferred.
}

type ChainNode struct {
    Children []RoutingNode // first = primary, rest = ordered fallbacks
}

type TargetLeaf struct {
    Provider string
    Name     string // backend model id
}
```

Validation:

- Exactly one of `chain` / `target` set on a node.
- Chain has ≥1 child and ≤ some sane cap (8 — covers any realistic
  fallback chain; rejects pathological configs).
- All Target `provider` values must exist in the catalog's `providers[]`.

### Match

`Decision` (`match.go:53`) already carries the flat primary-target
fields (`ProviderBackend`, `ProviderKind`, `BackendModel`, `Binding`)
from the prelims. Extend it with the fallback routing fields:

```go
type Target struct {
    ProviderBackend string
    BackendModel    string
    ProviderKind    string
}

// Additions to Decision — existing fields unchanged.
//   Fallbacks []Target  // ordered; empty for sugar entries
```

The flat primary-target fields on `Decision` remain authoritative for
the primary attempt; `Fallbacks[N-1]` is read by `pick` on attempt N+1.

Resolver:

- For a model entry with `routing.chain`, populate the flat fields from
  the first Target child and store the remaining children in
  `Fallbacks[]`, preserving order.
- For a sugar entry (`provider`/`binding`/`name` at the model level),
  leave `Fallbacks` nil.
- Disallow nested Chain-of-Chain in v1 (flatten or reject — pick one;
  rejecting is simpler).

`Apply` (`match.go:74`) already writes primary-target state; no changes
needed there. Add a small `attempt_targets` slice serialized into
dynamic metadata for access logs.

### Pick

- `lookupHost` (`pick.go:220`) reads the attempt number directly from
  `ctx.GetHostSelectionRetryCount()` — this method is on
  `down.ClusterLBContext` (and therefore `up.ClusterLBContext`) and
  returns 0 on the primary attempt, 1 on the first retry, etc. No
  stream-bag counter or SDK changes are needed. Use the value as a
  zero-indexed offset: 0 → primary (existing flat fields), N →
  `Fallbacks[N-1]`. If N exceeds `len(Fallbacks)`, return
  `orange.fallback_exhausted` and let Envoy fail the request.
- Continue to multi-IP round-robin *within* the chosen
  `(provider, binding)` bucket.
- The host pool for every fallback target is already registered by
  prelim 3 — no `AddHosts` call from the retry path.

### Envoy template

In `examples/orange/envoy.tmpl.yaml` (and the e2e copy), add a
**fixed blanket** `retry_policy` on `/v1/chat/completions`,
`/v1/messages`, and `/v1/responses` routes:

```yaml
retry_policy:
  retry_on: "5xx,gateway-error,reset,connect-failure,refused-stream,retriable-status-codes"
  retriable_status_codes: [429]
  num_retries: 7   # ceiling; pick is the real gatekeeper
  per_try_timeout: <existing per-request timeout>
```

This policy is static and shared — it does not vary per key or per
model. All it does is give Envoy permission to call `ChooseHost` again
after a transport error; `pick` decides whether to actually advance to
a fallback. When `len(Fallbacks) == 0` (sugar entry or chain
exhausted), `ChooseHost` returns `orange.fallback_exhausted` and Envoy
stops. No per-chain `retry_on` field exists; the chain config is
intentionally silent on which error codes trigger a retry, because
`ClusterLBContext` does not expose the previous response code — `pick`
cannot enforce it anyway.

**Future: per-request retry_policy override via dynamic module extension.**
The blanket policy is the right trade-off today, but the ideal end state
is a dynamic module HTTP filter that rewrites the route's `retry_policy`
per request based on the resolved chain — so a key with no fallbacks gets
`num_retries: 0` (no retry at all) and a key with a chain gets a policy
scoped to exactly the codes that chain cares about. This requires an
Envoy dynamic module extension point for mutating per-route retry config
after the downstream filter chain runs, which does not exist today. When
that primitive lands, `ChainNode` can grow a `retry_on` field again and
the coordination problem disappears.

### Credinject

- Read `provider` from the Decision and inject the right endpoint +
  auth header on each attempt. A cross-provider fallback rewrites
  credentials; same-provider retries reuse the same key.
  `Provider.Host()` gives the `:authority` value — no binding lookup
  needed since `TargetLeaf` carries no binding. Confirm the retry path
  re-runs the headers phase — otherwise the SNI lock-in bites. If
  retries do not re-run the downstream filter chain, document the path;
  otherwise no change is needed beyond reading the current target from
  the Decision.

### Streaming caveat

- For `/v1/chat/completions` with `stream: true` and SSE responses,
  once a `200 OK` + first body byte has flushed, retry is not
  possible. The chain's fallback only covers connect / 5xx /
  gateway-error / reset / timeout *before* upstream bytes begin
  streaming. Log this explicitly in `pick` when the streaming flag is
  set and the chain has fallbacks the client won't see if the upstream
  partially errors mid-stream.

### Tests

Table-driven in `internal/pipeline/match` and `internal/pipeline/pick`:

- Chain of three targets, primary returns 502 → second target serves
  200. Verify the response body comes from the second target.
- Chain returns 429 on primary and 502 on secondary → tertiary
  serves. Verify retry count == 2.
- Chain exhausts → request fails with the last upstream's status
  (or `orange.fallback_exhausted` if all hosts unreachable).
- Streaming response from primary errors after first SSE byte → no
  retry; client sees the partial stream + transport error.
- Sugar form (no `routing`) still works; no retries.
- Unknown `provider` in a chain Target → loader rejects with a clear error.
- Per-key isolation: same alias on two different keys with different
  chains; each resolves to its own chain.

e2e in `examples/orange/e2e/`:

- Two stub provider upstreams ("anthropic-fake", "vertex-fake") plus a
  third ("openai-fake"); chain authored across them. Kill the primary →
  retry hits the secondary; kill both → tertiary.

## Out of scope

- **Traffic splitting (Split nodes, weights, RNG, sampling
  determinism).** Lands next.
- **Per-binding auth.** Provider-scoped auth is sufficient.
- **BYOK envelope refs.** Defer until BYOK lands.
- **Health-aware target eviction beyond Envoy's `retry_on`.** Don't
  add a custom circuit breaker here.
- **Observability deep dive.** Counters per `(provider, binding)` for
  attempts/successes/fallbacks-used are fine; histograms, per-arm
  latency dashboards, etc. are follow-ups.
- **MCP changes.** Part II of the policy doc is independent.

## Conventions

- `gsed` (not `sed`) for any in-place YAML/JSON edits.
- No backwards-compat shims for code paths we're replacing. The sugar
  shape (`provider`/`name`/`binding` directly on the model entry)
  stays because operators use it; `Decision` is extended with a
  `Fallbacks` field rather than restructured.
- No new comments beyond what's already requested in pick's package
  doc + the README's "Runtime hosts without xDS" section. Don't
  annotate every retry branch.
- Keep `internal/config` plain Go structs. Confpack-style packing
  (`docs/orange-policies-llm-mcp.md` § 5.1) is deferred; just don't
  paint yourself into a corner that would require ripping match/pick
  apart later. The loader interface should be the same shape that a
  packed view could implement.

## Acceptance

- `go test ./examples/orange/...` passes including new fallback tests.
- e2e suite under `examples/orange/e2e/` passes the chain scenarios
  above against stub upstreams.
- No new xDS resources required to introduce a fallback target —
  authoring a chain in the YAML and reloading is sufficient.
- `docs/orange-policies-llm-mcp.md` § 1 schema accurately describes
  what shipped; update it in the same PR if any field name diverges.

## How to think about ambiguities

If something in the prereqs is not landed exactly as those prompts
describe, **don't paper over it** — stop and fix the prereq. The whole
point of the prelim split is that each substrate is independently
correct. Fallback assumes the substrate; if the substrate is wrong,
fallback inherits the bug.

If the Envoy retry mechanics turn out to behave differently than this
prompt describes (e.g. attempt count not exposed where expected, or
the headers phase not re-running on retry), surface that before
hacking around it; the data-plane contract may need adjusting and
that's a design conversation, not an implementation one.
