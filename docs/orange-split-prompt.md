# Orange LLM traffic split — feature prompt

**Goal**: ship per-key weighted traffic splitting for orange. Given a
request that selects a model alias on a known key, orange resolves a
`split` routing node by sampling one child at random according to the
configured weight distribution; the sampled child becomes the primary
target for that request. Fallback chaining within and across split
arms is supported.

Streaming is unaffected: the split selects a target once, before the
upstream connection is established. No retry mechanics are added here —
those are already in place from fallback.

## Prereqs

**Fallback substrate is fully landed** as of commit `0912ff7` (latest
functional commit on main; see `docs/orange-fallback-prompt.md`). No
new substrates are needed before starting this feature.

The following are in place and must not be re-implemented:

- `keys[]` blob loading, `KeyBlob`, `LookupKey`, per-key model resolution.
- `RoutingNode` / `ChainNode` / `TargetLeaf` types and `validateRoutingNode`.
- `Decision.Fallbacks []Target`, `Apply`, filter-state keys.
- `pick.ChooseHost` + `adapt.go` per-attempt target advancement.
- Blanket `retry_policy` on LLM routes in `envoy.tmpl.yaml`.

## Context

Read these in order before starting:

- `docs/orange-policies-llm-mcp.md` § 1 "LLM routing tree (per-key)"
  and the "Traffic splits" subsection — schema shape and sampling
  contract.
- `examples/orange/internal/config/config.go` — `RoutingNode`,
  `ChainNode`, `TargetLeaf`, `validateRoutingNode`, `MaxChainRetries`,
  `ChainRetryPolicy`, `walkModelChains`.
- `examples/orange/internal/pipeline/match/match.go` — `Decision`,
  `bodyHandler` chain-resolution branch, `Apply`.
- `examples/orange/internal/pipeline/pick/pick.go` — `lookupHostN`,
  `ChooseHost` — understand what they expect from `Decision.Fallbacks`.

## Scope

### Config

Add `SplitNode` and `SplitChild` in
`examples/orange/internal/config/config.go`:

```go
// SplitNode is a routing node that samples one child by weight on
// every request. Weights must be positive integers summing to 100.
// Decision.Model is always the Models map key (the client-facing alias;
// it can be any string and need not match a real provider model ID).
type SplitNode struct {
    Children []SplitChild `yaml:"children"`
}

// SplitChild is one weighted arm of a SplitNode.
type SplitChild struct {
    Weight      int    `yaml:"weight"`
    RoutingNode `yaml:",inline"`
}
```

Add `Split *SplitNode` to `RoutingNode`:

```go
type RoutingNode struct {
    Chain  *ChainNode  `yaml:"chain,omitempty"`
    Target *TargetLeaf `yaml:"target,omitempty"`
    Split  *SplitNode  `yaml:"split,omitempty"`
}
```

Update `validateRoutingNode`:

- Accept exactly one of `chain` / `target` / `split`.
- For `split`: require `len(Children) >= 2`, `<= 8`; verify weights sum
  to exactly 100; recursively validate each child's `RoutingNode`.

Update `walkModelChains` to descend into `split.children` so that
`MaxChainRetries` and `ChainRetryPolicy` pick up any chains nested
inside split arms.

### JSON Schema

In `examples/orange/internal/config/config.schema.json`, extend the
`RoutingNode` definition to allow `split` alongside `chain` and
`target`. Add a `SplitChild` sub-schema with required `weight`
(integer, 1–100) and the inline `RoutingNode` properties. Enforce
`minItems: 2` and `maxItems: 8` on `split.children`.

Weight-sum validation (must equal 100) is a semantic check handled in
`validateRoutingNode`, not in the JSON Schema.

### Sampler

Add `examples/orange/internal/pipeline/match/sampler.go`:

```go
// sampleSplit picks a child index from s.Children using crypto/rand
// and the configured weight distribution. Panics if weights do not
// sum to a positive integer (caller must guarantee valid config).
func sampleSplit(s *config.SplitNode) int {
    return sampleSplitFrom(s.Children, cryptoIntn)
}

// sampleSplitFrom is the testable core. randN(n) must return a
// uniformly distributed value in [0, n).
func sampleSplitFrom(children []config.SplitChild, randN func(n int) int) int
```

Build a cumulative-weight prefix array once per call; binary-search (or
linear scan for ≤8 arms) for the arm whose window contains the draw.
`cryptoIntn` draws from `crypto/rand.Reader` via `rand.Int(rand.Reader,
big.NewInt(int64(n)))`. The split between production and test surfaces allows injecting a seeded
`math/rand/v2` source (`rng.IntN`) in tests without touching production
code paths.

### Match

Refactor `bodyHandler` in `match.go` to replace the flat
chain-vs-sugar if/else with a recursive resolver:

```go
// resolveRouting walks node top-down and returns the primary target,
// the ordered fallback slice, and the effective backend model name.
// Sampling (for split nodes) happens inside this call.
func resolveRouting(cfg *config.Config, node config.RoutingNode, entryModel string) (
    upstream, backendModel, binding string, fallbacks []Target,
)
```

Recursive cases:

- **`target`** — base case. Returns `target.Provider`, `target.Name`
  (or `entryModel` when unset), `""` binding, nil fallbacks.
- **`chain`** — resolve `children[0]` recursively → primary. Resolve
  each subsequent child recursively → append to fallbacks (depth-first,
  preserving order). Disallow nested chain-of-chain in v1 (a chain child
  that is itself a chain node → return an error; log and fall through to
  `ErrUnknownModel`).
- **`split`** — call `sampleSplit` to select one child index. Resolve
  that child recursively → primary + fallbacks. The `upstream` /
  `backendModel` / `binding` / `fallbacks` from the selected child become
  the return values of the split node. `Decision.Model` is always
  `entryModel` (the `Models` map key), which is the client-facing alias
  and can be any string — it need not match any real provider model ID.

`Decision.BackendModel` is always the concrete backend model name from
the sampled/resolved `TargetLeaf` (i.e. `Target.Name`, falling back to
`entryModel` when `Target.Name` is unset).

The sugar path (`entry.Routing == nil`) is unchanged.

`Apply` and the rest of `bodyHandler` need no changes once
`resolveRouting` returns the same (`upstream`, `backendModel`,
`binding`, `fallbacks`) tuple as before.

### Envoy template

No changes. The blanket `retry_policy` on LLM routes is already present
and sufficient — splits resolve to a single target before the upstream
connection is established.

### Demo config

Create `examples/orange/demos/split/orange.yaml`. All three arms route
to the same real Vertex AI endpoint so every request succeeds and the
access log's `provider_backend` field proves which arm was selected.
Needs the same `GCP_PROJECT` / `GCP_SERVICE_ACCOUNT_JSON` env vars as
`demo-fallback`.

```yaml
llm:
  providers:
    primary:
      kind: anthropic
      backend_schema: gcpanthropic
      endpoint: https://us-east5-aiplatform.googleapis.com
      extra:
        anthropic_version: "vertex-2023-10-16"
        gcp_project: env://GCP_PROJECT
        gcp_location: us-east5
      auth:
        type: gcp
        secret_ref: env://GCP_SERVICE_ACCOUNT_JSON
    primary2:
      kind: anthropic
      backend_schema: gcpanthropic
      endpoint: https://us-east5-aiplatform.googleapis.com
      extra:
        anthropic_version: "vertex-2023-10-16"
        gcp_project: env://GCP_PROJECT
        gcp_location: us-east5
      auth:
        type: gcp
        secret_ref: env://GCP_SERVICE_ACCOUNT_JSON
    primary3:
      kind: anthropic
      backend_schema: gcpanthropic
      endpoint: https://us-east5-aiplatform.googleapis.com
      extra:
        anthropic_version: "vertex-2023-10-16"
        gcp_project: env://GCP_PROJECT
        gcp_location: us-east5
      auth:
        type: gcp
        secret_ref: env://GCP_SERVICE_ACCOUNT_JSON
  models: {}

keys:
  demo/maya/sk-split:
    workspace: demo
    user: maya
    llm:
      models:
        claude-haiku-4-5:   # the map key is the client-facing alias; any string works
          routing:
            split:
              children:
                - weight: 34
                  target: { provider: primary,  name: claude-haiku-4-5 }
                - weight: 33
                  target: { provider: primary2, name: claude-haiku-4-5 }
                - weight: 33
                  target: { provider: primary3, name: claude-haiku-4-5 }
```

`demos/split/envoy.tmpl.yaml` is a verbatim copy of
`demos/fallback/envoy.tmpl.yaml` — no Envoy changes are needed for
split. Update the file header comment only.

The demo config is deliberately minimal: pure split, no chains.
Split-within-chain and the kitchen-sink combination of features belong
in the main `orange.yaml` later; keep the demo focused.

The `demo-split` Makefile target follows the same pattern as
`demo-fallback`: GCP env-var guard, banner with curl instructions,
`envsubst` render to a temp file, Envoy launch. The banner should
suggest running the curl 10–20 times and piping access log output
through `jq .provider_backend` to observe the distribution across
`primary`, `primary2`, and `primary3`.

### Tests

**`internal/config/config_test.go`**:

- Valid split (two arms, weights 60+40=100) — loads without error.
- Valid split (three arms, weights 34+33+33=100) — loads without error.
- Invalid: weights sum to 99 → loader error.
- Invalid: weights sum to 101 → loader error.
- Invalid: only one child → loader error.
- Invalid: nine children → loader error.
- Invalid: child target references unknown provider → loader error.
- Split nested inside chain child — valid; `MaxChainRetries` and
  `ChainRetryPolicy` return 0 (no chains inside this split).
- Chain nested inside split arm — valid; `MaxChainRetries` returns the
  correct depth from that inner chain.

**`internal/pipeline/match/sampler_test.go`**:

All distribution tests inject a seeded `math/rand/v2` source via
`sampleSplitFrom` so results are deterministic and reproducible.

*Boundary tests* (deterministic, inject a mock `randN` that returns a
fixed value):

- `randN` returns 0 → always selects arm 0 regardless of weights.
- `randN` returns `w[0]-1` → arm 0 (last value inside first window).
- `randN` returns `w[0]` → arm 1 (first value of second window).
- `randN` returns `total-1` → last arm.

*Distribution proof via chi-squared goodness-of-fit*:

Use `rand.New(rand.NewPCG(42, 0))` (`math/rand/v2`) as the seeded source.
Run N=100 000 draws, accumulate per-arm counts, then compute:

```
χ² = Σ (observed_i − expected_i)² / expected_i
     where expected_i = N × weight_i / 100
```

Assert `χ² < χ²_crit` for `df = k−1` at α=0.001. Hardcode the
critical values for the arm counts used:

| arms (k) | df | χ²_crit (α=0.001) |
|---|---|---|
| 2 | 1 | 10.83 |
| 3 | 2 | 13.82 |
| 8 | 7 | 24.32 |

Test cases using this helper:

- Two-arm (50/50): χ² < 10.83.
- Three-arm (34/33/33): χ² < 13.82.
- Three-arm (1/1/98): χ² < 13.82; also assert `observed[2] > 95 000`
  to catch a bug that normalises weights incorrectly.
- Eight-arm (uniform 12/13/13/13/12/13/12/12=100): χ² < 24.32.

A correct implementation with N=100 000 produces χ² ≈ df on average;
the test is flaky only if the sampler is wrong. At α=0.001 the false
positive rate is 0.1% per case — acceptable without a fixed seed; with
the fixed seed it is deterministically zero.

**`internal/pipeline/match/match_test.go`** (table-driven):

- Split with three equal arms: call `bodyHandler` N=300 times (via test
  stub); assert all three upstreams are selected at least once.
- Verify `Decision.Model` always equals the `Models` map key regardless
  of which arm is sampled; `Decision.BackendModel` equals the sampled
  child's `Target.Name` (or the map key when `Target.Name` is unset).
- Split arm is a chain: verify `Decision.Fallbacks` is populated from
  the inner chain's remaining children.
- Sugar entry and chain entry still resolve correctly (regression).

**`e2e/e2e_test.go`**:

- Three stub upstreams behind a split config. Send 30 requests; assert
  all three backends received at least one request.
- Split where one arm is a chain: kill the primary of that arm; verify
  the chain's fallback within the arm is reached.

## Out of scope

- **Health-aware weight adjustment.** Weights are static; no adaptive
  rebalancing based on error rates.
- **Sticky sessions / consistent hashing.** Each request is sampled
  independently.
- **Per-split observability.** Counters per arm (which arm was selected,
  per-arm latency) are a follow-up.
- **Split inside split (nested splits).** `validateRoutingNode` rejects
  a `split` child whose `RoutingNode` is itself a `split`. Chain and
  target children are allowed; the only forbidden nesting is
  split→split. Revisit after observability lands.
- **MCP changes.** Part II of the policy doc is independent.
- **BYOK credential overrides.**

## Conventions

- `gsed` for any in-place YAML/JSON edits.
- `crypto/rand` for sampling — do not use `math/rand`.
- No new packages; keep `sampleSplit` in
  `internal/pipeline/match/sampler.go` within the existing match
  package.
- `resolveRouting` may return an error only for mis-configurations that
  passed schema validation but fail semantic checks (nested
  chain-of-chain). Treat these as `ErrUnknownModel` at the call site —
  log the details, don't panic.
- No new comments beyond what's requested above. The sampler function
  signature and the `SplitNode` type doc are sufficient.

## Acceptance

- `go test ./examples/orange/...` passes including all new split tests.
- Sampler distribution test converges within tolerances on both CI and
  local runs (use a fixed iteration count, not a time budget).
- `demos/split/orange.yaml` loads without error via `make demo-split`.
- No new xDS resources required: adding a split entry to the YAML and
  reloading is sufficient.
- `docs/orange-policies-llm-mcp.md` § 1 schema block updated to include
  `split` if any field name diverged during implementation.

## How to think about ambiguities

The sampling contract is: one draw per request, at body-parse time,
before any upstream connection is opened. If `resolveRouting` is called
more than once per request (e.g. retried by Envoy before the upstream is
selected), the second call must not re-sample — the decision is frozen
in `Decision` and pick reads it via filter state. Verify this invariant:
`bodyHandler` calls `resolveRouting` exactly once, stores the result in
`Decision`, and resolves the promise. pick never calls back into
`resolveRouting`.

If the weight-validation rule (sum == 100) turns out to be too strict
for operator UX (e.g. rounding errors from UI sliders as in the
screenshot), revisit before shipping: summing to ≤ 100 with the
remainder treated as "drop to last arm" is one option. Raise this as a
design question rather than silently normalising.
