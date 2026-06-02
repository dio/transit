# Cluster Host Refresh Loop SDK Proposal

Status: design sketch, not implemented.

## Problem

Cluster Extension examples that own their host set repeat the same operational
pattern:

1. Build a desired host snapshot from user-owned business logic.
2. Compare it with the currently published host set.
3. Add new Envoy hosts and mark them healthy.
4. Publish a read-optimized key-to-host map for `ChooseHost`.
5. Remove hosts that are no longer referenced.
6. Repeat from a background goroutine without violating Envoy's main-thread
   host-mutation rule.

The business logic is not generic. For example, `orange/hostpick` resolves a
provider endpoint to `ip:port`, and another user might resolve model IDs from a
catalog API. The loop, lifecycle, scheduling, snapshot publication, and host-set
diffing are generic enough to live in the SDK.

## Goals

- Provide a reusable SDK helper for periodically refreshing Cluster Extension
  hosts.
- Keep resolution and service-discovery logic in user code.
- Make the safe path easy: background work runs off-thread, Envoy host mutation
  runs through `ClusterHandle.Schedule`.
- Preserve the last-good host set on refresh errors by default.
- Provide a read path suitable for request-time `ClusterLB.ChooseHost`.
- Keep the API generic over the caller's key type, e.g. provider name, model
  name, route name, or catalog entry ID.

## Non-goals

- Do not bake DNS resolution into the SDK.
- Do not introduce a new control-plane protocol.
- Do not replace Envoy EDS/CDS for clusters Envoy already owns.
- Do not make health checking sophisticated in the first version. Initial host
  health can be `HostHealthy`; active or passive health can come later.
- Do not expose CGO or `down/abi_impl` details.

## Proposed API

The API should live in `up/` because Cluster Extension users already import
`github.com/dio/transit/up`.

Sketch:

```go
package up

type HostSnapshot[K comparable] map[K]HostSpec

type HostEntry struct {
    Address string
    Host    HostPtr
}

type HostSet[K comparable] struct {
    // unexported: handle, atomic published map, optional policy
}

func NewHostSet[K comparable](handle ClusterHandle) *HostSet[K]

// Apply replaces the desired host snapshot. It must be called on Envoy's
// cluster main thread, either from Cluster.Init or from ClusterHandle.Schedule.
func (s *HostSet[K]) Apply(snapshot HostSnapshot[K])

func (s *HostSet[K]) Get(key K) (HostPtr, bool)
func (s *HostSet[K]) Entry(key K) (HostEntry, bool)
func (s *HostSet[K]) Current() map[K]HostEntry
```

The refresh loop should be separate from `HostSet` so users can also apply
snapshots from push-based sources:

```go
type HostSnapshotFunc[K comparable] func(context.Context) (HostSnapshot[K], error)

type HostRefreshOptions struct {
    Interval time.Duration
    Timeout  time.Duration
    Jitter   time.Duration       // optional; +/- random offset added to Interval
    Observe  HostRefreshObserver // optional; see "Metrics" below
}

type HostRefreshEvent struct {
    Duration  time.Duration
    Added     int
    Removed   int
    Unchanged int
    Err       error // nil on success; set on snapshot-fn error
}

type HostRefreshObserver func(HostRefreshEvent)

type HostRefreshLoop[K comparable] struct {
    // unexported
}

func NewHostRefreshLoop[K comparable](
    handle ClusterHandle,
    set *HostSet[K],
    snapshot HostSnapshotFunc[K],
    opts HostRefreshOptions,
) *HostRefreshLoop[K]

func (l *HostRefreshLoop[K]) Start()
func (l *HostRefreshLoop[K]) Stop()
func (l *HostRefreshLoop[K]) RefreshOnce(ctx context.Context) error
```

## Semantics

### HostSet.Apply

`Apply` accepts a complete desired snapshot. For each key:

- if the key exists and both `Address` and `Metadata` are unchanged, keep the
  existing `HostPtr`;
- if the key is new, or `Address` changed, or `Metadata` changed, call
  `ClusterHandle.AddHosts` with the new spec and queue the old `HostPtr` (if
  any) for removal;
- call `ClusterHandle.UpdateHostHealth(ptr, HostHealthy)` for newly added
  hosts;
- publish a new immutable key-to-entry map atomically;
- remove old hosts that are no longer referenced by the new map.

The implementation should add new hosts before publishing the snapshot, and
publish before removing unused old hosts. This prevents `ChooseHost` from
seeing an entry for a host Envoy does not know about yet.

`HostSpec.Metadata` must be preserved when adding hosts. This matters for
`transport_socket_matches`, such as per-provider SNI in `orange`. Metadata is
part of the diff key because Envoy binds metadata to the `HostPtr` at
`AddHosts` time; changing it requires a new host.

### HostSet.Get

`Get` is the request-time read path. It should be lock-free or near-lock-free,
using an atomic pointer to an immutable map. `ChooseHost` can call it directly.

`Current` returns a freshly-copied map so callers cannot mutate the published
snapshot. Callers that only need a single key should prefer `Get`/`Entry`.

### HostRefreshLoop

The loop owns the boring lifecycle:

- create and start an `up.Group`;
- tick at `HostRefreshOptions.Interval` (plus +/- `Jitter` if set);
- call the user `HostSnapshotFunc` with a bounded context;
- on success, schedule `set.Apply(snapshot)` with `ClusterHandle.Schedule`;
- on error, keep the previous host set unchanged and continue ticking;
- coalesce: if a previously-scheduled apply has not yet run on the main
  thread, skip scheduling a new apply for this tick (latest pending wins).
  This avoids piling up applies when the main thread is slow;
- stop promptly when `Stop` is called.

`RefreshOnce` runs one snapshot fetch and applies it safely. It blocks until
the scheduled apply has completed on the main thread (or until `ctx` is
cancelled), so callers can rely on the host set being up to date when it
returns. Use it from `Cluster.ServerInitialized` or in tests; inside
`Cluster.Init` (cluster main thread, before `PreInitComplete`) callers can
call `HostSet.Apply` directly to avoid a self-deadlock on `Schedule`.

`Start` does not perform an immediate refresh. Callers that want a warm host
set before the first tick should call `RefreshOnce` (or `HostSet.Apply`
synchronously, inside `Init`) before `Start`.

### Stop

`Stop` cancels the ticker, then waits for any in-flight snapshot fetch and
any already-scheduled apply to complete before returning. After `Stop`
returns, no further `Apply` will run, even if `ClusterHandle.Schedule` was
called before `Stop`. Concretely the loop maintains a `closed` flag taken
under the same lock as the "apply pending" slot; the scheduled apply checks
the flag and returns early when set.

This is the contract `Cluster.Shutdown` relies on: callers can
`refresh.Stop(); done()` without leaking an apply onto a cluster Envoy is
tearing down.

Default behavior if `Interval` or `Timeout` is zero should be conservative:

- `Interval`: 30 seconds.
- `Timeout`: 1 second.

These defaults should be constants in `up/`, not hidden magic.

## Threading Rules

The helper must preserve the Cluster Extension rules:

- User snapshot resolution may run in a goroutine.
- `ClusterHandle.AddHosts`, `RemoveHosts`, `UpdateHostHealth`, and
  `PreInitComplete` must run on Envoy's cluster main thread.
- `HostRefreshLoop` must call `ClusterHandle.Schedule` before applying a
  refresh snapshot.
- `HostSet.Apply` should document that it is main-thread only.
- `HostSet.Get`, `Entry`, and `Current` must be safe from worker-thread
  `ChooseHost` calls.

## Failure Policy

First version should use a simple last-good policy:

- If the snapshot callback returns an error, do not change hosts.
- If the callback returns a partial snapshot without error, treat it as the
  caller's desired state — any key not present is removed.
- If the callback wants to preserve old entries on partial resolution failures,
  it should include those entries in its returned snapshot or return an error.

This keeps the SDK generic. The SDK does not know whether a missing key means
"delete this host" or "DNS temporarily failed".

The example `resolveProviders` returns `nil, err` on the first failed lookup,
which is the safe default for DNS-backed snapshots: a transient failure on
any provider keeps the previous snapshot intact. Implementations that iterate
and skip failed keys instead must accept that a transient failure deletes the
host and (briefly) fails `ChooseHost` for that key.

## Example Usage: orange hostpick

`orange` would keep endpoint-to-address resolution as business logic:

```go
func (c *cluster) resolveProviders(ctx context.Context) (up.HostSnapshot[string], error) {
    cfg := orangecfg.Get()
    snap := make(up.HostSnapshot[string], len(cfg.Providers))

    for name, p := range cfg.Providers {
        addr, err := resolveUpstream(ctx, p.Endpoint)
        if err != nil {
            return nil, err
        }

        spec := up.HostSpec{Address: addr}
        if host := p.Host(); host != "" {
            spec.Metadata = map[string]string{"sni": host}
        }
        snap[name] = spec
    }

    return snap, nil
}
```

The cluster then becomes mostly lifecycle wiring:

```go
type cluster struct {
    handle  up.ClusterHandle
    hosts   *up.HostSet[string]
    refresh *up.HostRefreshLoop[string]
}

func (c *cluster) Init(h up.ClusterHandle) {
    c.handle = h
    c.hosts = up.NewHostSet[string](h)

    if snap, err := c.resolveProviders(context.Background()); err == nil {
        c.hosts.Apply(snap)
    }
    h.PreInitComplete()

    c.refresh = up.NewHostRefreshLoop(
        h,
        c.hosts,
        c.resolveProviders,
        up.HostRefreshOptions{Interval: 30 * time.Second, Timeout: time.Second},
    )
    c.refresh.Start()
}

func (c *cluster) Shutdown(_ up.ClusterHandle, done func()) {
    c.refresh.Stop()
    done()
}
```

`ChooseHost` can then read through the SDK:

```go
host, ok := l.owner.hosts.Get(res.Provider)
if !ok {
    completion.Complete(nil, "orange.unknown_upstream")
    return
}
completion.Complete(host, "")
```

## Implementation Plan

1. Add `up/host_set.go`.
   - Define `HostSnapshot`, `HostEntry`, `HostSet`.
   - Implement atomic snapshot publication.
   - Implement add-before-publish, publish-before-remove ordering.

2. Add `up/host_refresh_loop.go`.
   - Define `HostSnapshotFunc`, `HostRefreshOptions`, `HostRefreshLoop`.
   - Use `up.Group` for lifecycle.
   - Use `ClusterHandle.Schedule` for refresh applies.

3. Add unit tests in `up/`.
   - Initial apply adds and marks hosts healthy.
   - Reapplying unchanged snapshot does not add/remove.
   - Changed address adds new host, publishes new pointer, removes old pointer.
   - Removed key removes old host after publish.
   - Metadata is passed through to `AddHosts`.
   - `Get` returns the published pointer.
   - Refresh callback error keeps old snapshot.
   - `Stop` stops the loop.

4. Optionally migrate `examples/orange/hostpick`.
   - Keep `resolveUpstream` and `splitEndpoint` local.
   - Replace the local atomic host map with `up.HostSet[string]`.
   - Add refresh loop configuration under `orange.yaml` only if needed; otherwise
     use SDK defaults for the example.

5. Optionally migrate `examples/cluster-router`.
   - This can validate that the helper handles catalog/API snapshots, not only
     DNS-like resolution.

## Metrics

`ClusterHandle` does not expose `DefineCounter`/`DefineGauge`/`DefineHistogram`
the way `ConfigHandle` does for HTTP filters (`up/up.go` ConfigHandle vs.
`down/cluster.go` ClusterHandle). Rather than extend the cluster surface in
this first cut, the refresh loop exposes a callback:

```go
type HostRefreshEvent struct {
    Duration  time.Duration
    Added     int
    Removed   int
    Unchanged int
    Err       error
}
type HostRefreshObserver func(HostRefreshEvent)
```

The loop invokes `Observe` once per tick, after the scheduled `Apply` has
run, on Envoy's cluster main thread. `Duration` covers fetch + apply.
`Added`/`Removed`/`Unchanged` are computed by `Apply` and zero on error.

Users wire `Observe` to whatever metrics surface they have. For a cluster
extension co-located with an HTTP filter, the typical pattern is:

1. Define counters/gauges/histograms in the filter's `WithConfig` callback
   and capture the returned `MetricID`s in package state.
2. From `Cluster.Init`, pass an `Observe` closure that calls the filter's
   writer (or any user surface that ultimately reaches Envoy stats).

Suggested initial metrics (names are user choice; the SDK only delivers the
event):

- counter `host_refresh_total{result="success|error"}`
- counter `host_refresh_changes_total{kind="added|removed"}`
- gauge `host_refresh_current_hosts`
- histogram `host_refresh_duration_seconds`

Extending `ClusterHandle` with first-class metric definition is a separate
proposal; the observer keeps this design self-contained until a second use
case demands it.

## Open Questions

- Should `HostSet.Apply` return an error when `AddHosts` returns fewer hosts
  than requested, or silently keep old entries for those keys?
- Should removal be optional for users who want stale hosts retained until a
  grace period expires?
- Should the SDK expose hooks for health status other than `HostHealthy`?

## Acceptance Criteria

- SDK users can implement periodic host refresh without writing their own
  goroutine lifecycle or `ClusterHandle.Schedule` wrapper.
- Request-time host lookup is safe from `ClusterLB.ChooseHost`.
- Envoy host mutation still happens only on the cluster main thread.
- Existing Cluster Extension examples continue to compile.
- New unit tests cover snapshot diffing, atomic publication, metadata changes
  forcing re-add, refresh errors preserving last-good, apply coalescing under
  slow `Schedule`, `Stop` draining an in-flight scheduled apply, and observer
  invocation on both success and error.
