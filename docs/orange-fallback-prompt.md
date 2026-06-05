# Orange LLM fallback — feature prompt

**Goal**: ship LLM cross-target fallback for orange. Given a request
that selects a model alias on a known key, orange resolves a chain of
`(provider, binding)` targets; on classified transport errors *before*
any response byte has streamed, Envoy retries on the next target.
Streaming bypasses fallback (acknowledged limitation, ch. 16).

Traffic splitting is **out of scope** here — split lands after this.

## Prereqs (must be merged first)

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

If any of these are not landed, **stop and finish them first**. Do not
ship fallback on top of a partial substrate.

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
        smart:
          routing:
            chain:
              retry_on: [429, 5xx, timeout, reset]
              children:
                - target: { provider: anthropic, binding: us-east, name: claude-haiku-4-5-20251001 }
                - target: { provider: anthropic, binding: us-west, name: claude-haiku-4-5-20251001 }
                - target: { provider: openai, name: gpt-4o-mini }
```

Types in `internal/config`:

```go
type RoutingNode struct {
    Chain  *ChainNode  `yaml:"chain,omitempty"`
    Target *TargetLeaf `yaml:"target,omitempty"`
    // Split *SplitNode — deferred.
}

type ChainNode struct {
    RetryOn  []string      // accepts "429", "5xx", "timeout", "reset"
    Children []RoutingNode // first = primary, rest = ordered fallbacks
}

type TargetLeaf struct {
    Provider string
    Binding  string
    Name     string // backend model id
}
```

Validation:

- Exactly one of `chain` / `target` set on a node.
- Chain has ≥1 child and ≤ some sane cap (8 — covers any realistic
  fallback chain; rejects pathological configs).
- All Target `(provider, binding)` pairs must exist in the catalog.
- `retry_on` codes are a subset of the union supported by the Envoy
  template route's `retry_policy`.

### Match

Replace the single-target `Decision` with primary + fallbacks:

```go
type Decision struct {
    Primary   Target
    Fallbacks []Target
    RetryOn   []int     // numeric HTTP codes; "timeout"/"reset" mapped to retry_on policy bits, not status codes
    // ... existing fields (Err, etc.)
}

type Target struct {
    ProviderBackend string
    Binding         string
    BackendModel    string
    ProviderKind    string
}
```

Resolver:

- For a model entry with `routing.chain`, flatten the chain's Target
  children into `Primary` + `Fallbacks[]`, preserving order.
- For a sugar entry (`provider`/`binding`/`name` at the model level),
  emit a single-target Decision with empty `Fallbacks`.
- Disallow nested Chain-of-Chain in v1 (flatten or reject — pick one;
  rejecting is simpler).

Write `Decision` to filter state as today (`Apply` at
`match.go:72`). Add a small `attempt_targets` slice serialized into
dynamic metadata for access logs.

### Pick

- `lookupHost` (`pick.go:192`) reads attempt count from filter state.
  The dynamic-modules SDK surfaces it via
  `ClusterLBContext`/`envoy.lb.previous_hosts`; if that path is not
  yet wired, add a minimal helper that exposes attempt count to
  `ChooseHost`. On attempt N (1-indexed past the primary), pick the
  host for `Fallbacks[N-1]`; if N exceeds `len(Fallbacks)`, return
  `orange.fallback_exhausted` and let Envoy fail the request.
- Continue to multi-IP round-robin *within* the chosen
  `(provider, binding)` bucket.
- The host pool for every fallback target is already registered by
  prelim 3 — no `AddHosts` call from the retry path.

### Envoy template

In `examples/orange/envoy.tmpl.yaml` (and the e2e copy), add a
`retry_policy` on `/v1/chat/completions`, `/v1/messages`, and
`/v1/responses` routes:

```yaml
retry_policy:
  retry_on: "5xx,gateway-error,reset,connect-failure,refused-stream,retriable-status-codes"
  retriable_status_codes: [429]
  num_retries: 7   # matches the chain cap above; ChooseHost gates further
  per_try_timeout: <existing per-request timeout>
```

Per-request gating is enforced by `pick`: when `len(Fallbacks) == 0`
on a given Decision, `ChooseHost` returns the primary and refuses to
serve a different host on retry — Envoy's `num_retries` cap is a
ceiling, not a mandate. This keeps the policy decision in the data
plane's blob, not in xDS.

### Credinject

- Read `(provider, binding)` from the Decision and inject the right
  endpoint + auth header on each attempt. Provider-scoped auth means
  a regional fallback uses the same key; a cross-provider fallback
  rewrites credentials. Confirm `:authority` is set from the headers
  handler (per the README's `auto_sni` trap section) and that the
  retry path re-runs the headers phase — otherwise the SNI lock-in
  bites. If retries do not re-run the downstream filter chain,
  document the path; otherwise no change needed beyond reading the
  current target.

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
- Unknown `(provider, binding)` in a chain Target → loader rejects
  with a clear error.
- Per-key isolation: same alias on two different keys with different
  chains; each resolves to its own chain.

e2e in `examples/orange/e2e/`:

- Two stub upstreams ("us-east-fake", "us-west-fake") plus a third
  ("openai-fake"); chain authored across them. Kill the primary →
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
  stays because operators use it; the `Decision`-single-target shape
  goes away cleanly.
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
