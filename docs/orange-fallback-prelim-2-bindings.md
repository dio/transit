# Prelim 2 — Provider `bindings[]` in the catalog

**Status**: prerequisite for orange LLM fallback.
**Depends on**: nothing structural.
**Parallelisable with**: prelim 1.

## Why

The most common LLM fallback shape is *same provider, different
region* (e.g. `anthropic us-east → anthropic us-west` before crossing
to a different vendor). Without named bindings, a Target can only
address one endpoint per provider, so the chain has nowhere to land.
Bindings also let `pick.lookupHost` key its host map by
`(provider, binding)` which prelim 3 needs for runtime host
registration.

See `docs/orange-policies-llm-mcp.md` § 2 for the schema and § 1 for
how Targets reference bindings.

## Scope

### Config schema

Extend providers in `internal/config/config.go`:

```go
type Provider struct {
    Kind          string
    Endpoint      string       // implicit "default" binding (back-compat)
    BackendSchema string
    Auth          ProviderAuth
    Extra         map[string]string
    Bindings      []Binding    // NEW
}

type Binding struct {
    Name     string  // unique within the provider
    Endpoint string  // overrides Provider.Endpoint
    // Extra and Auth stay on the parent Provider in v1; bindings only
    // override transport endpoint.
}
```

YAML:

```yaml
llm:
  providers:
    anthropic:
      kind: anthropic
      auth: { type: anthropic, secret_ref: env://ANTHROPIC_API_KEY }
      bindings:
        - name: us-east
          endpoint: https://api.anthropic.com
        - name: us-west
          endpoint: https://api-west.anthropic.com
```

If `bindings` is omitted, treat `provider.endpoint` as the single
binding named `default`. Existing `examples/orange/orange.yaml`
continues to work.

Validation: binding names unique per provider; at least one binding or
a top-level `endpoint`; reject endpoints that don't match the
provider's URL scheme constraints.

### Decision and lookup

- `match.Decision` (`internal/pipeline/match/match.go`) gains a
  `Binding string` field on its target. Empty string means "default".
- `pick.lookupHost` (at `internal/pipeline/pick/pick.go:192`) keys the
  host map by `(provider, binding)` instead of provider alone. Update
  the `resolvedUpstream` map key type accordingly.
- `cluster.resolveAddrs` (`pick/pick.go:217`) iterates bindings:

  ```go
  for name, p := range cfg.Providers {
      for _, b := range p.AllBindings() {  // includes implicit "default"
          key := provBindingKey{name, b.Name}
          out[key] = ... resolve(b.Endpoint) ...
      }
  }
  ```

- `applyResolved` reconciles the `(provider, binding)`-keyed map.

### Credinject

`credinject` already rewrites `:authority` from the upstream
endpoint. Make sure it reads the binding's endpoint (not the
provider's top-level endpoint) when a binding is selected. Auth stays
provider-scoped in v1 (no per-binding `secret_ref`).

### `match.LookupModel` / `LookupModelForKey`

A Target may now author `provider: anthropic, binding: us-east, name: …`.
For prelim 2, even without routing trees, a model entry can carry an
optional `binding:` field:

```yaml
llm:
  models:
    claude-sonnet:
      provider: anthropic
      binding: us-east   # NEW
      name: claude-haiku-4-5-20251001
```

Threaded through the lookup return so `match.Decision.Binding` is set.

## Deliverables

- `internal/config/config.go` — `Binding`, `Provider.Bindings`,
  `Provider.AllBindings()` helper that yields the implicit default,
  validation.
- `config.schema.json` — `bindings` array under each provider.
- `internal/pipeline/match/match.go` — `Decision.Binding` field,
  filter-state plumbing.
- `internal/pipeline/pick/pick.go` — `(provider, binding)`-keyed host
  map, DNS refresh per binding, `lookupHost` matches on the pair.
- `internal/pipeline/credinject` — pick the binding endpoint when set.
- Tests:
  - Provider with two bindings produces two DNS-refresh entries.
  - Target with `binding: us-east` lands on east IPs.
  - Missing binding name → `orange.unknown_upstream` (502).
  - Omitting `bindings` keeps the legacy single-endpoint behavior.

## Out of scope

- Per-binding `auth`/`secret_ref` (defer; provider-level auth is fine
  for the same-vendor regional case).
- Routing trees (Chain/Split). Models still resolve to a single
  `(provider, binding)` here.
- Health-aware binding selection. Round-robin within a binding is the
  only LB shape.
- Runtime host registration without xDS — that's prelim 3.

## Conventions

- `gsed` (not `sed`).
- Don't introduce a new package — fold `Binding` next to `Provider`.

## Acceptance

`go test ./examples/orange/...` passes; a config with one provider and
two bindings serves traffic to both endpoints via `binding:` selection
in the model entry; the legacy single-endpoint config still works
unchanged.
