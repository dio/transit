# Implementation prompt: `orange-mcp` loopback as a ClusterExtension

Hand this file to a fresh agent. The full design rationale is in
`docs/orange-sidecar-loopback-cluster-extension.md` — read that first
for the "why"; this file is the "how" for the first of three PRs.

---

## Task

Fold the `orange-mcp` sidecar's lifecycle and its loopback endpoint
into a single dynamic-modules cluster. After this change:

- The no-op `orange-mcp` HTTP filter is gone.
- The static `orange-mcp-loopback` cluster is replaced by a
  dynamic-modules cluster that owns the sidecar.
- The sidecar bind port lives in Go only — the YAML no longer
  duplicates it.
- `/mcp` behavior is unchanged end-to-end.

Out of scope: `orange-responsesws` (next PR), extracting a shared
`up/sidecarcluster` package (PR after that), egress listener/cluster
changes, ephemeral-port migration.

## Read these first

1. `docs/orange-sidecar-loopback-cluster-extension.md` — the design,
   especially the "Today / Proposed", "Conversion 1: `orange-mcp`",
   and "Open questions" sections.
2. `examples/orange/internal/pipeline/pick/pick.go` — the existing
   dynamic-modules cluster you'll mirror. Focus on `factory`,
   `cfgFactory`, `cluster.Init`, `cluster.NewClusterLB`,
   `cluster.Shutdown`, and how it's registered via
   `up.RegisterCluster` in `init()`.
3. `up/cluster.go` and `up/cluster_group.go` — the SDK surface.
   `up.BaseCluster` exists; embed it. `up.ClusterGroup` manages
   background goroutines tied to cluster lifecycle.
4. `examples/orange/internal/pipeline/mcp/mcp.go` and
   `examples/orange/internal/pipeline/mcp/sidecar.go` — the code
   you're moving.
5. `examples/orange/envoy.tmpl.yaml` — the YAML you'll edit. Note
   line ~67 (`orange-mcp` http_filter, delete) and line ~309
   (`orange-mcp-loopback` cluster, replace).

## Open questions to resolve before writing code

These are flagged in the design doc; spend ~30 min confirming
empirically, *don't guess*:

1. **Single-host LB.** Can you return the same `HostPtr` from every
   `ChooseHost`, or does the cluster need to go through
   `AsyncHostSelector` like `pick` does? Check
   `up.EmptyClusterLB` semantics in `up/lb.go` and how it's used in
   tests. A fixed-host LB is simpler if allowed.
2. **`PreInitComplete` on failure.** Verify the contract in
   `up/cluster.go:16` (the doc comment on `ClusterHandle`).
   `pick.go:118` calls `PreInitComplete` after a possibly-failing
   `applyResolved`, so the answer is almost certainly "yes, zero
   hosts is fine," but read the comment.
3. **Group-start API.** `up.ClusterGroup.Start()` is called from
   `ServerInitialized`, not `Init` (see `up/cluster_group.go:51`).
   But we need the sidecar bound before `PreInitComplete` in `Init`.
   Resolution: bind the listener synchronously in `Init` (so
   `ListenAddr()` is valid and `AddHosts` can publish it), then
   register `Serve` as a `ClusterGroup` goroutine that
   `ServerInitialized` will start. See "Step 2" below for the
   listen/serve split.

Write down what you find for each, in the PR description.

## Steps

### Step 1 — New package skeleton

Create `examples/orange/internal/pipeline/mcp/loopback/loopback.go`:

- `const ClusterName = "orange-mcp-loopback"`.
- `init()` calls `up.RegisterCluster(ClusterName, &factory{...})`,
  mirroring `pick.go:48` and `pick.go:56`. Pass an
  `observability.Logger("orange/mcp/loopback")`.
- `factory` → `cfgFactory` → `cluster` chain identical in shape to
  `pick`, but the cluster carries `*mcp.Sidecar` (or whatever the
  exported handle becomes — see Step 2) instead of DNS state.
- `cluster` embeds `up.BaseCluster` so you only override `Init`,
  `ServerInitialized`, `NewClusterLB`, and `Shutdown`.

Add `loopback_test.go` with a `fakeHandle` modeled on
`up/async_host_selector_test.go:51`. Tests:

- Sidecar binds and `Ready` fires before timeout → `AddHosts`
  called with `sc.ListenAddr()`, `UpdateHostHealth(_, HostHealthy)`
  called, `PreInitComplete` called.
- Bind fails → no `AddHosts`, `PreInitComplete` still called, error
  logged.
- `Shutdown` stops the sidecar Group and calls `done()`.
- `ChooseHost` returns the stored ptr (or 503-shaped completion
  when none is registered, depending on what Step 1 question (1)
  resolves to).

### Step 2 — Split sidecar lifecycle

In `examples/orange/internal/pipeline/mcp/sidecar.go`:

- Expose `Listen() error` that does the current `net.Listen` call
  (lines 53–67 of `sidecar.go`), sets `s.ln`, `s.srv`, `s.resolved`,
  and closes `s.ready` + `s.started`.
- Rename `execute(name string) error` to `Serve() error`; its body
  becomes just `return s.srv.Serve(s.ln)` plus the empty-egress-URL
  warn. The `name` arg goes away (or move logging up to the caller).
- `Stop()` already exists and is correct.
- Export the type if it isn't already (`*Sidecar`), or add a
  constructor `NewSidecar()` that the loopback package can call.

In `examples/orange/internal/pipeline/mcp/mcp.go`:

- Move handler + sidecar construction (lines 49–66) out of `init()`
  into an exported `NewSidecar() (*Sidecar, error)` function. The
  loopback's `cluster.Init` calls it.
- Delete `up.Register(FilterName, noop, up.WithGroup(g))`. The
  loopback owns lifecycle now.
- Delete the `FilterName` constant.
- Keep `up.Register(EgressFilterName, egressHandler)` and the
  `EgressFilterName` constant — that filter is real and unrelated.
- Update `mcp_test.go` if it references `FilterName`.

### Step 3 — Cluster `Init` / `ServerInitialized` / `Shutdown`

In `loopback.go`:

```go
func (c *cluster) Init(h up.ClusterHandle) {
    c.handle = h
    sc, err := mcp.NewSidecar()
    if err != nil { /* log; PreInitComplete; return */ }
    c.sc = sc
    if err := sc.Listen(); err != nil { /* log; PreInitComplete; return */ }
    ptrs := h.AddHosts([]up.HostSpec{{Address: sc.ListenAddr()}})
    if len(ptrs) == 0 { /* log; PreInitComplete; return */ }
    h.UpdateHostHealth(ptrs[0], up.HostHealthy)
    c.host = ptrs[0]
    h.PreInitComplete()
}

func (c *cluster) ServerInitialized(_ up.ClusterHandle) {
    if c.sc == nil { return } // bind failed in Init
    c.bg.Go(func(ctx context.Context) {
        // Serve returns on Stop; ignore http.ErrServerClosed.
        _ = c.sc.Serve()
    })
    c.bg.Start()
}

func (c *cluster) Shutdown(_ up.ClusterHandle, done func()) {
    if c.sc != nil { c.sc.Stop() }
    c.bg.Stop()
    done()
}
```

Adapt to whatever the open-question answers dictate. `Serve` does
not take a `ctx`; the existing `Stop()` triggers `http.Server.Shutdown`
which causes `Serve` to return. The `ctx` from `ClusterGroup.Go`
is unused here, which is fine — sidecars that need it (future) can
read it.

### Step 4 — `NewClusterLB`

If open question (1) resolves to "fixed-host is fine," return a
trivial LB whose `ChooseHost` returns `c.host, nil`. If it doesn't,
follow `pick.go:355–373`'s `AsyncHostSelector` pattern with a lookup
that always returns the single registered host.

### Step 5 — YAML

Edit `examples/orange/envoy.tmpl.yaml`:

- Delete the `orange-mcp` HTTP filter entry (currently lines
  ~67–75, including the leading comment about WithGroup).
- Replace the `orange-mcp-loopback` cluster (currently lines
  ~307–318) with the dynamic-modules version from the design doc's
  "Conversion 1 → Rendered config → clusters" section. The cluster
  name stays `orange-mcp-loopback` so the route at `prefix: /mcp`
  doesn't need to change.
- Regenerate `envoy.yaml` if the repo tracks it (it does: it sits
  next to the template; check the Makefile's `run` target — likely
  `envsubst < envoy.tmpl.yaml > envoy.yaml`).

Leave alone: `orange-mcp-egress` listener, `orange-mcp-egress`
cluster, `orange-mcp-egress-match` filter registration, all access
logs, `orange-responsesws-*`, anything else.

### Step 6 — Build and e2e

1. `make` at repo root to rebuild `liborange.so`.
2. Run the orange e2e suite:
   `cd examples/orange/e2e && make` (check the actual command in
   `examples/orange/e2e/Makefile` — there's likely a single target).
3. Sanity-check `make run` in `examples/orange/` and hit `/mcp`
   with the existing `mcp-demo` script to confirm the
   black-box behavior (session creation, SSE streaming, access log
   includes `mcp_method`/`mcp_tool`).

If e2e fails, do *not* guess at fixes — diagnose with logs first.
The likeliest failure modes:

- Cluster `Init` blocks forever because `Listen` blocks waiting for
  something it shouldn't. Check that the split is clean.
- Port conflict between Envoy's own listeners and the sidecar
  (you removed the http_filters entry that was implicitly serialising
  startup — verify Envoy still starts the sidecar before opening
  the inbound listener).
- `AddHosts` returns no ptrs because the address format isn't what
  the cluster expects. Check what `pick` passes vs. what
  `sc.ListenAddr()` returns (it returns `ln.Addr().String()`, e.g.
  `127.0.0.1:10004` — should be fine).

## Acceptance

- `make` builds clean.
- Orange e2e suite passes.
- `make run` + `mcp-demo` works end-to-end: session POST returns a
  session id, GET streams SSE, access log shows `mcp_method` /
  `mcp_tool`.
- `grep -rn "orange-mcp" examples/orange/envoy.tmpl.yaml` returns
  only the route match, the egress listener, the egress cluster,
  and the new dynamic-modules `orange-mcp-loopback` block —
  *not* an `http_filters` entry.
- `grep -rn "FilterName" examples/orange/internal/pipeline/mcp/`
  returns only `EgressFilterName` references.

## PR description checklist

Include in the PR body:

- Resolutions for the three open questions above.
- One-line description of the listen/serve split in `sidecar.go`.
- Confirmation that `ORANGE_MCP_LISTEN_ADDR` still works as an
  override (test it once: `ORANGE_MCP_LISTEN_ADDR=127.0.0.1:19004
  make run`, then hit `/mcp`).
- Note that `orange-responsesws` is intentionally not touched.

## What you must not do

- Do not extract a shared `sidecarcluster` package in this PR. That
  is PR #3 and requires the responsesws conversion to land first so
  the abstraction reflects two real callers, not one.
- Do not touch `orange-responsesws-*`, `orange-meter`,
  `orange-mcp-egress*`, or any access-log block.
- Do not change `/mcp` route semantics, session crypto, or any
  user-visible behavior.
- Do not switch the sidecar to ephemeral port (`:0`) in this PR,
  even though the new cluster would support it. Separate decision.
- Do not skip the open-question investigation. Guessing at SDK
  contracts here will cost more time than confirming them.
