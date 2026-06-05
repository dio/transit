# Prelim 3 — Dynamic-module fallback-host substrate (no xDS)

**Status**: prerequisite for orange LLM fallback.
**Depends on**: prelim 2 (binding-keyed host map).

## Why

The fallback feature needs Envoy to retry on an *alternate*
`(provider, binding)` on attempt N. The literal Envoy shape — "send
attempt N to a different cluster via dynamic cluster selection" —
requires every fallback cluster to be known via xDS. Two reasons we
won't take that path:

1. **Round-trip defeats the materialized-blob model.** The blob is
   already in the dynamic module; consulting the control plane on
   retry is the opposite of "data plane reads, never computes."
2. **Envoy Gateway is a hard dependency in real deployments.** Pushing
   xDS-affecting config means authoring Gateway CRDs and waiting for
   reconcile. That cadence is wrong for per-key blob churn.

The mechanism we use instead: register all addressable hosts at runtime
on the single `orange-pick` cluster via the dynamic module's
`AddHosts`, with `auto_host_sni` and a bounded SNI-scoped TLS session
cache so the runtime-added hosts handle TLS correctly without xDS
supplying SNI.

**The Envoy-side substrate is already in place.** The custom Envoy
build used here ships with `auto_host_sni` and the bounded SNI session
cache — see
`https://gist.github.com/dio/965d1e555909c02013ca882a2b3caa78` for the
background. This prelim does **not** need to add or modify
`auto_host_sni` / TLS-session-cache config in the Envoy template; the
`orange-pick` upstream TLS context already has it.

What this prelim *does* wire is the dynamic-module side: enumerating
the full catalog's `(provider, binding)` pairs and tagging the
runtime-added hosts so `pick.ChooseHost` can target a specific pair on
retry. Fallback selection logic itself lives in the fallback prompt.

## Scope

### Cluster TLS context — already done

The Envoy template's `orange-pick` upstream TLS context already has
`auto_host_sni` and the bounded SNI session cache enabled (custom
Envoy build, per the gist linked above). Do **not** re-add or modify
those fields. The only template changes this prelim may need are
incidental — e.g. clarifying comments. Verify by grepping for
`auto_host_sni` in `examples/orange/envoy.tmpl.yaml`; it should
already be present.

### Host registration at load time

`pick.applyResolved` already calls `AddHosts`/`RemoveHosts` for
DNS-resolved upstreams. Extend that path so:

- Every `(provider, binding)` in the *current snapshot's catalog* is
  registered, regardless of whether any key currently references it.
  Rationale: a fallback chain may name a binding that no Primary
  uses, and we want that host present without a reload race.
- Each `HostSpec` carries metadata `{provider, binding}` so
  `pick.ChooseHost` can filter by the pair.
- On config reload, diff the `(provider, binding)` set and call
  `AddHosts`/`RemoveHosts` accordingly. Idempotent: re-applying the
  same snapshot is a no-op.

### Host filtering in `ChooseHost`

`pick.lookupHost` (`internal/pipeline/pick/pick.go:192`) today reads
`d.ProviderBackend` and round-robins inside that bucket. Generalise to
`(d.ProviderBackend, d.Binding)`:

```go
key := provBindingKey{d.ProviderBackend, d.Binding}
if r := (*m)[key]; r != nil && len(r.ptrs) > 0 { ... }
```

For prelim 3 there is no attempt-count read yet; that's the fallback
prompt's job. This prelim only ensures the host pool is correctly
populated and queryable by the pair.

### Reload safety

`applyResolved` must remain main-thread-only (per the existing
contract at `pick.go:133`). The reload path is the only mutator;
readers (`lookupHost`) load the `atomic.Pointer` snapshot.

## Deliverables

- `internal/pipeline/pick/pick.go` — `(provider, binding)`-keyed host
  map (from prelim 2), full catalog enumeration on resolve, idempotent
  reconcile.
- `internal/pipeline/pick/README.md` and code-level comments — record
  that the no-xDS substrate (`auto_host_sni` + bounded SNI session
  cache, per the gist) is already enabled in the `orange-pick`
  upstream and that `AddHosts` calls from `applyResolved` are the
  designed entry point for *all* runtime host registration, including
  fallback targets. This is implicit knowledge today; the comment
  should make it explicit so future readers don't reach for xDS.
- Tests:
  - All catalog `(provider, binding)` pairs are registered after a
    fresh load, even ones not referenced by any model entry.
  - Reload with one binding removed → `RemoveHosts` called for that
    binding only; other hosts untouched.
  - `lookupHost` returns the right ptr for an explicit
    `(provider, binding)` decision.
  - TLS to a runtime-added host handshakes with the expected SNI
    (e2e — run `examples/orange/e2e/` against a stub provider with two
    bindings).

## Out of scope

- Attempt-count / retry plumbing. That's the fallback prompt.
- Health-based host eviction beyond the existing DNS-refresh path.
- Per-binding auth headers; provider-level auth still applies.
- Confpack packing of the host metadata table; plain Go maps are fine.

## Conventions

- `gsed` for any in-place YAML/JSON edits.
- Don't introduce a separate "host registry" abstraction — the
  existing `c.hosts atomic.Pointer[hostMap]` is the right shape.

## Acceptance

`go test ./examples/orange/...` passes; e2e suite under
`examples/orange/e2e/` runs against a catalog with one provider and
two bindings, with `orange-pick` carrying both runtime-added hosts and
serving TLS with `auto_host_sni`. No new xDS resources are required to
add or remove a binding — a config reload alone suffices.
