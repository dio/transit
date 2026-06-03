---
name: transit-cluster-main-thread
description: Threading rules for Envoy dynamic-module ClusterHandle operations. Covers which methods must be called on the main thread, the Schedule escape hatch, how to split background DNS work from main-thread host mutations, and the envoy_bug symptom that fires when the contract is violated.
---

# Transit cluster main-thread contract

Use this skill whenever a `Cluster` implementation spawns goroutines (e.g. a
DNS refresh loop, a config watcher, or any background worker) that need to
mutate the cluster host set.

Hard-won from a production bug in `examples/orange/internal/pipeline/pick`.

## The contract

`down.ClusterHandle` (re-exported as `up.ClusterHandle`) documents:

> All methods except `Schedule` must be called on the main thread.

The affected methods are:

| Method | Restriction |
|---|---|
| `AddHosts` | main thread only |
| `RemoveHosts` | main thread only |
| `UpdateHostHealth` | main thread only |
| `FindHostByAddress` | main thread only |
| `PreInitComplete` | main thread only |
| `Schedule(fn)` | **thread-safe** — safe from any goroutine |

Violating this produces the following Envoy log and a no-op (the health update
is silently discarded):

```
[error][envoy_bug] [source/extensions/clusters/dynamic_modules/abi_impl.cc:240]
envoy bug failure: false. Details:
envoy_dynamic_module_callback_cluster_update_host_health must be called on the main thread
```

The same error fires for `AddHosts` / `RemoveHosts` violations (different line
numbers in `abi_impl.cc`).

## When Cluster callbacks run on the main thread

- `Init(h)` — main thread; blocking DNS here is acceptable since Envoy waits
  for `PreInitComplete`.
- `ServerInitialized(h)` — main thread; safe to call `AddHosts` etc. here, but
  the idiomatic pattern is to launch goroutines from this method and use
  `Schedule` to marshal mutations back.
- `NewClusterLB()` — main thread.
- `DrainStarted(h)` — main thread.
- `Shutdown(h, done)` — main thread.
- `ClusterLB.ChooseHost` / `CancelHostSelection` — **worker thread**; must not
  call `ClusterHandle` methods directly (only via `Schedule`).

## The two-phase pattern for background refresh loops

Split any function that mixes IO (DNS, HTTP, file) with host mutations into two
phases. Call phase 1 on the goroutine; schedule phase 2 back to the main thread.

```go
// Phase 1: IO — goroutine-safe.
func (c *cluster) resolveAddrs(ctx context.Context) map[string]dnsResult { … }

// Phase 2: host mutations — must run on main thread.
func (c *cluster) applyResolved(h up.ClusterHandle, resolved map[string]dnsResult) {
    // AddHosts / RemoveHosts / UpdateHostHealth all go here.
}

// refreshLoop is a background goroutine launched from ServerInitialized.
func (c *cluster) refreshLoop(ctx context.Context, h up.ClusterHandle) {
    for {
        // ... sleep until next TTL ...
        addrs := c.resolveAddrs(rctx)           // DNS on goroutine — fine
        done := make(chan struct{})
        c.handle.Schedule(func() {              // mutations on main thread
            c.applyResolved(h, addrs)
            close(done)
        })
        select {
        case <-ctx.Done():
            return
        case <-done:
        }
    }
}
```

The `done` channel lets the goroutine wait for each reconcile before computing
the next sleep interval — important when `applyResolved` updates the TTL state
that `earliestNextRefresh` reads.

### Why not schedule the whole function including DNS?

DNS resolution is blocking (tens to hundreds of milliseconds). Running it on
the Envoy main thread stalls all other cluster operations on that thread for the
duration of the lookup.

### Why not just skip the wait?

Without the `done` channel the goroutine immediately computes the next sleep
using stale TTL data (the `applyResolved` call that updates it hasn't run yet).
On the first few iterations this produces a tight spin at `minTTLFloor`
until the scheduled function catches up.

## `up.HostSet.Apply` follows the same rule

`up.HostSet[K].Apply` calls `AddHosts`, `RemoveHosts`, and `UpdateHostHealth`
internally. The same restriction applies: call `Apply` on the main thread only.
From a background goroutine, build the `HostSnapshot` off-thread and then
`Schedule` a call to `Apply`.

## `up.AsyncHostSelector` is already correct

`AsyncHostSelector.ChooseHost` receives a `promise.OnResolve` callback that may
fire from a worker goroutine. It always marshals the `completion.Complete` call
back via `handle.Schedule`. This is the canonical example of the pattern — if
you're adding a new completion pathway, follow it exactly.

## Diagnostics

```bash
# Health counter should increment on every successful refresh cycle.
curl -s http://127.0.0.1:9901/stats | grep upstream_rq_total

# Per-host health status.
curl -s http://127.0.0.1:9901/clusters | grep health_flags

# The envoy_bug line in Envoy stderr is the definitive signal:
# "must be called on the main thread"
```

If hosts appear in `/clusters` but `health_flags` shows `/failed_active_hc`
or a stale unhealthy state, the most likely cause is `UpdateHostHealth` being
silently dropped because it was called off the main thread.
