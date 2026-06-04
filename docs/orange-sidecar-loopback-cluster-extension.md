# Orange sidecar loopbacks as ClusterExtensions

Fold each Orange sidecar's lifecycle and its loopback endpoint into a
single dynamic-modules cluster, eliminating the static
`orange-*-loopback` clusters and the no-op `orange-*` entries in the
HCM `http_filters` list.

**Two concrete conversions, then one extraction.** This doc covers, in
order:

1. `orange-mcp` — simplest case (plain HTTP/SSE), proves the shape.
2. `orange-responsesws` — same shape with WebSocket-upgrade wrinkles;
   also explains why this one keeps a meter-bridge filter that
   `orange-mcp` doesn't need.
3. **Generalization into a shared `up/sidecarcluster` package**
   (top-level SDK, peer to `up/buffer` and `up/compress`) — discussed
   at the end on purpose. We do *not* extract
   until both one-off conversions land. Designing the abstraction
   from one example would lock in choices that don't fit the second
   (UDS support is the obvious risk). After both ship as one-off
   clusters, the extraction is mechanical.

Scope of each per-sidecar PR: replace the lifecycle plumbing only.
Egress listeners, egress clusters, and meter-bridge filters stay.

## Today (shared shape)

Each sidecar wires into Envoy in two scopes:

1. **HTTP filter as lifecycle hook.** `up.Register(FilterName, noop,
   up.WithGroup(g))` registers a no-op filter whose `WithGroup`
   side-effect starts the sidecar Group. The filter is listed in
   `envoy.tmpl.yaml`'s `http_filters` purely to trigger this
   registration at filter-chain init.
2. **STATIC cluster as endpoint.** `orange-*-loopback` hard-codes
   `127.0.0.1:<port>`. The port duplicates `defaultListenAddr` in the
   Go source; if they drift, the route silently fails.

Egress path (sidecar → `orange-*-egress` listener → `orange_default` →
real backend) is unrelated and stays.

## Proposed (shared shape)

Replace both pieces with one dynamic-modules cluster per sidecar,
mirroring `pick.go`:

- `factory.Create` / `cfgFactory.NewCluster` return a `*cluster`
  carrying the sidecar handle.
- `cluster.Init(h)` (main thread):
  1. Build the handler and sidecar (code currently in the package's
     `init()`).
  2. Start the sidecar Group; block on `<-sc.Ready()` with a 5 s
     timeout (matches `pick`'s `defaultResolveTimeout`).
  3. `h.AddHosts([]HostSpec{{Address: sc.ListenAddr()}})`,
     `UpdateHostHealth(ptr, HostHealthy)`, store ptr.
  4. `h.PreInitComplete()`.
- `cluster.NewClusterLB` returns a fixed-host LB.
- `cluster.Shutdown(h, done)` stops the Group, calls `done()`.

Init runs synchronously on the main thread, so blocking on `Ready()`
matches `pick.Init` blocking on DNS — Envoy already tolerates this.

Cluster `Init` runs during bootstrap, before listeners accept
traffic. The sidecar binds before the first request — same guarantee
the current HTTP-filter `WithGroup` provides (filters init before
listener accept).

Failure-mode shift:

| Failure | Today | After |
|---|---|---|
| `net.Listen` fails | filter init logs, `WithGroup` returns err | `Init` cannot publish a host; `PreInitComplete` still fires → cluster has 0 hosts, route returns 503 |
| Port already bound | same | same |
| Sidecar handler panic at runtime | sidecar crashes, loopback cluster has stale endpoint, requests fail at TCP | identical |

We make `Init` fail loud by calling `h.AddHosts` only after `Ready`
fires; on timeout, log an error and skip `AddHosts`. That matches
`pick`'s "resolve failed → no host registered → 503" behavior.

## Conversion 1: `orange-mcp`

Simplest case. Plain HTTP/SSE on `:10004`, no upgrade.

### What moves out of `mcp.go`

- The `init()` body that builds `handler`, `sidecar`, and Group moves
  into `internal/pipeline/mcp/loopback` (or stays in `mcp` and is
  invoked from `loopback`).
- `up.Register("orange-mcp", noop, WithGroup(g))` is deleted.
- `up.Register(EgressFilterName, egressHandler)` stays — the egress
  filter is real, lives on a different listener, and has nothing to
  do with the loopback cluster.

`FilterName = "orange-mcp"` can be deleted along with the no-op
filter. The new cluster gets `LoopbackClusterName = "orange-mcp-loopback"`.

### Rendered config

`http_filters` — delete the `orange-mcp` entry:

```diff
                 http_filters:
                   - name: orange-match
                   - name: orange-responsesws
-                  # orange-mcp: no-op for HTTP; its WithGroup starts the MCP sidecar.
-                  - name: orange-mcp
-                    typed_config:
-                      "@type": type.googleapis.com/envoy.extensions.filters.http.dynamic_modules.v3.DynamicModuleFilter
-                      dynamic_module_config:
-                        name: orange
-                      filter_name: orange-mcp
-                      filter_config:
-                        "@type": type.googleapis.com/google.protobuf.StringValue
-                        value: '{}'
                   - name: envoy.filters.http.router
```

Route at `prefix: /mcp` — unchanged.

`clusters` — replace the STATIC loopback with a dynamic-modules cluster:

```diff
-    # orange-mcp-loopback: static cluster pointing at the orange-mcp sidecar.
-    # Port must match ORANGE_MCP_LISTEN_ADDR (default 127.0.0.1:10004).
-    - name: orange-mcp-loopback
-      type: STATIC
-      connect_timeout: 5s
-      load_assignment:
-        cluster_name: orange-mcp-loopback
-        endpoints:
-          - lb_endpoints:
-              - endpoint:
-                  address:
-                    socket_address: { address: 127.0.0.1, port_value: 10004 }
+    # orange-mcp-loopback: dynamic-modules cluster. The cluster extension
+    # owns the orange-mcp sidecar's lifecycle: it starts the sidecar at
+    # Init, registers sc.ListenAddr() as its single host, and stops the
+    # sidecar at Shutdown.
+    - name: orange-mcp-loopback
+      connect_timeout: 5s
+      lb_policy: CLUSTER_PROVIDED
+      cluster_type:
+        name: envoy.clusters.dynamic_modules
+        typed_config:
+          "@type": type.googleapis.com/envoy.extensions.clusters.dynamic_modules.v3.ClusterConfig
+          dynamic_module_config:
+            name: orange
+          cluster_name: orange-mcp-loopback
+          cluster_config:
+            "@type": type.googleapis.com/google.protobuf.StringValue
+            value: '{}'
```

Net effect: `http_filters` shrinks from 5 to 4; loopback cluster
keeps name and route binding, swaps `STATIC` for dynamic-modules,
drops `load_assignment`. Egress listener (`:10005`) and
`orange-mcp-egress` cluster untouched. No new env vars, no flags.

Bind-port duplication between Go and YAML disappears.

## Conversion 2: `orange-responsesws`

Same shape as `orange-mcp`, with two WebSocket-specific wrinkles and
one filter that does *not* move.

### Wrinkles vs `orange-mcp`

| Concern | `orange-mcp` | `orange-responsesws` |
|---|---|---|
| Loopback transport | HTTP/1.1 + SSE | HTTP/1.1 with WebSocket upgrade |
| Route timeout | `0s` | `0s`, plus `upgrade_configs: [{ upgrade_type: websocket }]` |
| HCM upgrade config | none | `upgrade_configs[0]` with `orange-responsesws-meter` + router |
| UDS support in sidecar | TCP-only | `unix://` prefix (`listenForSidecar`, `responsesws.go:130`) |

None of these touch the cluster definition — the WebSocket upgrade
is negotiated on the HCM and the route, not on the cluster. The
cluster delivers a TCP/UDS endpoint, which `sc.ListenAddr()` returns
verbatim.

The UDS case is a small bonus: the STATIC cluster couldn't cleanly
express a Unix socket endpoint (would need `pipe.path`). The
dynamic-modules cluster sidesteps this — `AddHosts` takes an
arbitrary address string. So the refactor unlocks the UDS path the
sidecar already supports.

### What stays put: `orange-responsesws-meter`

This is *not* a no-op filter and does not move. It sits in
`upgrade_configs.filters` of the inbound HCM
(`envoy.tmpl.yaml:32`), bridging sidecar meter records into the
inbound `/v1/responses` access log.

The asymmetry — `orange-responsesws` has a meter-bridge filter,
`orange-mcp` does not — comes from the transport, not from a design
mismatch we should normalize:

`orange-meter` (the upstream HTTP filter on `orange_default`)
extracts token usage from response bodies via terminal HTTP body
events. For `/v1/responses` over WebSocket:

- The inbound side terminates as `101 Switching Protocols`. No
  response body, no terminal event.
- The sidecar dials Envoy via `orange-responsesws-egress` (`:10003`),
  which routes to `orange-responsesws-default`. That cluster
  *intentionally omits* upstream HTTP filters
  (`envoy.tmpl.yaml:320–323`) for the same reason: once upstream
  returns 101, body callbacks never fire on tunnel frames.
- Neither end of the pipeline can run `orange-meter`. The sidecar is
  the only component that sees WS frames in cleartext.

`orange-responsesws-meter` plugs the gap:

1. Sidecar parses frames, accumulates `meter.TokenUsage`, publishes
   a `responseswsMeterRecord` keyed by `x-request-id`
   (`meter_bridge.go:31`).
2. The meter filter, on `EndStream` of the upgrade response, waits
   up to 250 ms for the sidecar's record, then writes
   `orange:model`, `orange:upstream`, `orange:provider`,
   `orange:backend_model`, `orange:endpoint`, and emits usage via
   `meter.EmitUsage` — so the file access log sees the same fields
   a normal HTTP request would have.

MCP doesn't need this because:

1. MCP traffic is real HTTP — terminal body events fire normally on
   both inbound `/mcp` (sidecar response) and the sidecar's egress
   call. `orange-meter` runs on the latter without modification.
2. MCP is JSON-RPC, not LLM completions. No input/output tokens to
   extract — the `/mcp` access-log filter (`envoy.tmpl.yaml:111`)
   omits token fields and only logs `mcp_method` / `mcp_tool`
   derived from `%RESP(x-orange-mcp-method)%` /
   `%RESP(x-orange-mcp-tool)%` response headers the sidecar sets.
3. Sidecar response headers + access-log `%RESP(...)` are enough for
   MCP's observability. Responsesws can't use that path — WS 101 has
   no meaningful response headers after the upgrade; data only
   exists in tunnel frames.

**General rule worth writing down:** *if a sidecar's transport
terminates as a WebSocket upgrade, you need a meter-bridge filter on
the inbound upgrade path. If it terminates as ordinary HTTP, you
don't.* A hypothetical `orange-mcp`-over-WebSocket variant would
need its own meter-bridge filter for the same reason.

### Rendered config

`http_filters` — delete the `orange-responsesws` entry:

```diff
                 http_filters:
                   - name: orange-match
-                  # orange-responsesws: no-op for HTTP; its WithGroup starts the Responses WS sidecar.
-                  # Keep it out of upgrade_configs to avoid starting the same
-                  # sidecar Group twice.
-                  - name: orange-responsesws
-                    typed_config:
-                      "@type": type.googleapis.com/envoy.extensions.filters.http.dynamic_modules.v3.DynamicModuleFilter
-                      dynamic_module_config:
-                        name: orange
-                      filter_name: orange-responsesws
-                      filter_config:
-                        "@type": type.googleapis.com/google.protobuf.StringValue
-                        value: '{}'
                   - name: orange-mcp        # (already removed in mcp PR)
                   - name: envoy.filters.http.router
```

The "Keep it out of upgrade_configs to avoid starting the same
sidecar Group twice" warning disappears with the entry — the cluster
owns lifecycle now, so there's no double-start to warn about.

`upgrade_configs` — unchanged. Route — unchanged.

`clusters` — STATIC → dynamic-modules:

```diff
-    # orange-responsesws-loopback: static cluster pointing at the orange-responsesws sidecar.
-    # Port must match ORANGE_RESPONSESWS_LISTEN_ADDR (default 127.0.0.1:10002).
-    - name: orange-responsesws-loopback
-      type: STATIC
-      connect_timeout: 5s
-      load_assignment:
-        cluster_name: orange-responsesws-loopback
-        endpoints:
-          - lb_endpoints:
-              - endpoint:
-                  address:
-                    socket_address: { address: 127.0.0.1, port_value: 10002 }
+    # orange-responsesws-loopback: dynamic-modules cluster. The cluster
+    # extension owns the orange-responsesws sidecar's lifecycle and
+    # publishes sc.ListenAddr() as its single host. The address may be
+    # a TCP host:port or a Unix socket path (when
+    # ORANGE_RESPONSESWS_LISTEN_ADDR uses unix://).
+    - name: orange-responsesws-loopback
+      connect_timeout: 5s
+      lb_policy: CLUSTER_PROVIDED
+      cluster_type:
+        name: envoy.clusters.dynamic_modules
+        typed_config:
+          "@type": type.googleapis.com/envoy.extensions.clusters.dynamic_modules.v3.ClusterConfig
+          dynamic_module_config:
+            name: orange
+          cluster_name: orange-responsesws-loopback
+          cluster_config:
+            "@type": type.googleapis.com/google.protobuf.StringValue
+            value: '{}'
```

## Generalization: `up/sidecarcluster`

**Do this after both one-off conversions land, not during.** The
cluster code on each side will be ~90% identical; the per-sidecar
deltas (UDS support, WS server config, options struct shape) sit
inside the sidecar struct, not the cluster. Once both PRs have shipped
and the e2e suite is green against the one-off clusters, the
extraction is mechanical.

### Package location: `github.com/dio/transit/up/sidecarcluster`

Top-level under `up`, peer to `up/buffer`, `up/compress`,
`up/testutil`. It is part of the public Orange-pipeline SDK surface,
not private to Orange — any consumer building a sidecar-shaped
extension benefits from it. Placing it under `internal/` would
prevent that.

The package composes on top of `up.ClusterGroup`
(`up/cluster_group.go`), which already implements the
"goroutines scoped to a cluster extension's lifecycle" primitive
this design needs. We do not reinvent it.

Sketch:

```
up/sidecarcluster/
    cluster.go   // generic dynamic-modules cluster: starts a Sidecar
                 // at Init via ClusterGroup, publishes a single host
                 // from ListenAddr(), stops at Shutdown.
```

### Minimal `Sidecar` interface

Both `mcp.sidecar` and `responsesws.responsesWSSidecar` already
satisfy this modulo method-name renames (`execute(name)` →
`Run(ctx) error`, to fit `ClusterGroup.Go`'s context-aware signature):

```go
package sidecarcluster

type Sidecar interface {
    Run(ctx context.Context) error
    Ready() <-chan struct{}
    ListenAddr() string
}
```

`Stop()` drops off the interface: `ClusterGroup` already cancels the
context on `Stop()` and waits for `Run` to return, which is the
right shutdown contract. Sidecars implement graceful shutdown
inside `Run` by selecting on `ctx.Done()`.

### Caller-side API

In `examples/orange/internal/pipeline/mcp/loopback/loopback.go`:

```go
package loopback

import (
    "github.com/dio/transit/examples/orange/internal/observability"
    "github.com/dio/transit/examples/orange/internal/pipeline/mcp"
    "github.com/dio/transit/up/sidecarcluster"
)

func init() {
    sidecarcluster.Register(sidecarcluster.Config{
        ClusterName:  "orange-mcp-loopback",
        Logger:       observability.Logger("orange/mcp/loopback"),
        NewSidecar:   func() (sidecarcluster.Sidecar, error) { return mcp.NewSidecar() },
        ReadyTimeout: 5 * time.Second,
    })
}
```

### Shared cluster behavior

1. `Init`: create the sidecar via `Config.NewSidecar`. Register it
   with a `ClusterGroup` via `cg.Go(sc.Run)`. The Group itself is
   started in `ServerInitialized` per the existing contract
   (`up/cluster_group.go:51`). Block on `sc.Ready()` with
   `Config.ReadyTimeout` *before* `PreInitComplete`. On success,
   `AddHosts` + `UpdateHostHealth` + `PreInitComplete`. On
   failure/timeout, log and `PreInitComplete` with zero hosts (route
   503s, same as `pick` on DNS failure).
2. `NewClusterLB`: fixed-host LB.
3. `Shutdown`: `cg.Stop()`, then `done()`.

This means `Init` blocks on a sidecar that hasn't been `Run` yet —
slight reordering needed. The cleanest split is:

- `Init` calls a thin `Sidecar.Listen()` (binds the socket
  synchronously, makes `ListenAddr()` valid, makes `Ready()` close).
- `Run(ctx)` only calls `Serve` on the already-bound listener.

That keeps the bind-before-PreInitComplete invariant without needing
a goroutine race against `Ready()`. Both existing sidecars already
do this internally (`execute` binds then serves); we just expose the
seam. Worth confirming in PR #3 review rather than guessing now —
flagged as open question (5) below.

Stays per-sidecar:

- Sidecar struct (handler, options, listen address resolution, UDS
  support, ready/started channels). These differ between `mcp` and
  `responsesws` in non-trivial ways.
- Any *other* `up.Register(...)` calls. `responsesws` keeps its
  meter filter; `mcp` keeps its egress filter. The shared cluster
  does not touch the filter registry.

If we add a third sidecar (or the hypothetical `orange-mcp` over WS),
it gets a 10-line `loopback.go` and reuses `sidecarcluster` directly.
The meter-bridge question is separate and answered by transport
shape, per the rule in the responsesws section.

## Rollout

Three PRs, in order:

1. **`orange-mcp` loopback.** New `internal/pipeline/mcp/loopback`
   + test. Move sidecar construction out of `mcp.go:init` into a
   constructor `loopback` calls. Delete `up.Register("orange-mcp",
   ...)` and `FilterName`. Update `envoy.tmpl.yaml`: delete HCM
   filter entry, swap STATIC cluster for dynamic-modules. Run e2e.
2. **`orange-responsesws` loopback.** Same steps, plus keep
   `MeterFilterName` and its registration. e2e: WS upgrade smoke
   test, existing meter-bridge test confirms access log still gets
   token fields.
3. **Extract `sidecarcluster`.** Mechanical de-duplication, no
   behavior change. Easy to review against a green e2e baseline.

No flag, no staged migration on (1) or (2). Surface area is small,
e2e is the acceptance test.

## Open questions

1. **Group start API.** `up.WithGroup(g)` attaches a Group to the
   filter's lifecycle. Does `up` already have a cluster-side
   equivalent? Check `up/cluster.go` and `up/group.go` before
   writing code. If not, `cluster.Init` drives the goroutine
   directly; we don't need the Group abstraction at all.
2. **Single-host cluster LB.** Confirm that returning the same
   `HostPtr` from every `ChooseHost` is acceptable. `pick` always
   goes through the async selector even for cache hits; a fixed-host
   LB is simpler but needs a quick check of `up.EmptyClusterLB`
   semantics.
3. **`PreInitComplete` on failure.** Confirm a cluster can publish
   zero hosts and still call `PreInitComplete`. `pick.go:118` does
   this on DNS failure today, so the answer is almost certainly yes
   — verify against the contract comment in `up/cluster.go`.
4. **UDS in `AddHosts`.** Confirm the dynamic-modules cluster path
   accepts a Unix socket path as an `Address`. If not, we need a
   small adapter that turns `unix:///tmp/foo` into something the
   upstream connector understands. 10-minute spike before
   committing to the responsesws YAML changes.
5. **Listen/Serve split on the `Sidecar` interface.** Confirm both
   `mcp.sidecar` and `responsesws.responsesWSSidecar` can cleanly
   expose `Listen()` (synchronous bind) separate from `Run(ctx)`
   (`Serve` on the bound listener). The existing `execute` methods
   already do this internally — we just need a public seam. Resolve
   during PR #3; not a blocker for the one-off conversions.

## Non-goals

- Touching the egress listeners (`orange-*-egress`) or egress
  clusters.
- Removing or moving `orange-responsesws-meter` or
  `orange-mcp-egress-match`.
- Switching either sidecar to an ephemeral port. Works for free once
  the cluster reads `sc.ListenAddr()`, but no tooling change in
  these PRs.
- Changing any user-visible behavior on `/mcp` or `/v1/responses`.
