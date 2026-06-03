# orange/pick: power-of-two-choices load balancing

## Problem

The current multi-IP round-robin in `lookupHost` distributes requests evenly by
count but ignores request cost. LLM traffic has high latency variance: a short
prompt completes in < 1 s while a long context window can take 30 s+. With
round-robin, a slow host accumulates a backlog of in-flight requests while the
fast host sits idle after completing its last one.

Power-of-two-choices (p2c) fixes this: pick two candidate IPs at random, compare
their active-request counts, route to the one with fewer. The randomisation avoids
the thundering-herd of pure least-connections while the comparison avoids the
hot-host problem of pure random.

---

## Gaps today

### 1. No `HostStatByPtr` on `ClusterHandle`

`ClusterHandle` (passed to `Init` / `ServerInitialized`) is the cluster-lifetime
handle used for host mutations. It exposes `AddHosts`, `RemoveHosts`,
`UpdateHostHealth`, `FindHostByAddress`, and `Schedule`.

`ClusterLBHandle` (passed per-request to `ChooseHost`) has the stat API:

```go
h.HostStat(priority, index, stat) uint64  // positional
h.Host(priority, index) HostPtr           // positional
h.HostCount(priority) int
```

The positional API is the only way to read per-host stats today. Mapping a
`HostPtr` back to its positional index requires scanning `HostCount` entries on
every call, and the indices shift whenever any host is added or removed globally.
That scan is `O(n)` and fragile for a multi-provider cluster where providers share
the same cluster.

`ClusterHandle` needs:

```go
// HostStatByPtr returns the requested stat for the host identified by ptr.
// Must be called on the cluster main thread.
HostStatByPtr(ptr HostPtr, stat HostStat) uint64
```

Once this exists, `lookupHost` can read `HostStatRqActive` directly via
`c.handle` without any index translation and without touching `ClusterLBHandle`
or `AsyncHostSelector`.

### 2. `lookupHost` must run on the cluster main thread

`lookupHost` already runs on the cluster main thread (called from
`AsyncHostSelector.complete` which is dispatched via `ClusterHandle.Schedule`).
`HostStatByPtr` would need the same guarantee — calling it off-thread is
undefined behaviour. No change to the call site is required since the threading
contract is already satisfied.

---

## Design

### `up` package change

Add `HostStatByPtr` to the `ClusterHandle` interface (and the corresponding ABI
call in the `down` package):

```go
// in up/cluster.go — mirrors the existing HostStat constants already exposed
type ClusterHandle interface {
    // existing methods …
    HostStatByPtr(ptr HostPtr, stat HostStat) uint64
}
```

### `pick.lookupHost` change

Replace the current single-host round-robin with p2c:

```go
func (c *cluster) lookupHost(d match.Decision) up.HostResult {
    if d.Err != "" {
        return up.HostResult{ErrDetail: d.Err}
    }
    m := c.hosts.Load()
    if m == nil {
        return up.HostResult{ErrDetail: "orange.unknown_upstream"}
    }
    r := (*m)[d.Provider]
    if r == nil || len(r.ptrs) == 0 {
        return up.HostResult{ErrDetail: "orange.unknown_upstream"}
    }
    return up.HostResult{Host: p2cPick(c.handle, r.ptrs)}
}

// p2cPick samples two candidates at random and returns the one with fewer
// active requests. Falls back to the first candidate when len == 1.
func p2cPick(h up.ClusterHandle, ptrs []up.HostPtr) up.HostPtr {
    if len(ptrs) == 1 {
        return ptrs[0]
    }
    i := rand.IntN(len(ptrs))
    j := rand.IntN(len(ptrs) - 1)
    if j >= i {
        j++
    }
    a, b := ptrs[i], ptrs[j]
    if h.HostStatByPtr(a, up.HostStatRqActive) <= h.HostStatByPtr(b, up.HostStatRqActive) {
        return a
    }
    return b
}
```

`rand.IntN` (Go 1.22 global rand, no lock) is intentionally used here: uniform
distribution over the candidate set, zero allocation.

### Threading invariant

`p2cPick` calls `HostStatByPtr` on `c.handle`. `lookupHost` is called from
`AsyncHostSelector.complete`, which runs via `ClusterHandle.Schedule` on the
cluster main thread. `HostStatByPtr` must document "must be called on the cluster
main thread" — the same constraint already on `AddHosts` / `RemoveHosts`.

---

## What is NOT in scope

- Weighted p2c (weight is already stored per `HostSpec` but orange does not set it)
- Peak EWMA (exponentially weighted moving average latency) — requires timing each
  request, which is not available from a `ClusterLBHandle` callback today
- Changing `AsyncHostSelector`'s generic lookup signature — `HostStatByPtr` on
  `ClusterHandle` makes that unnecessary
