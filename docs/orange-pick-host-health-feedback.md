# orange/pick: cx_connect_fail → UpdateHostHealth feedback loop

## Problem

When Envoy cannot establish a connection to an upstream IP it increments that
host's `cx_connect_fail` counter and returns a 502 to the caller. The host
remains `health_flags::healthy` in the cluster because nothing calls
`UpdateHostHealth(ptr, HostUnhealthy)`. Every subsequent request that round-robins
to the same IP gets the same 502 until the DNS TTL fires and `applyResolved`
re-registers the host.

Concretely: with two IPs per provider (`162.159.140.245` and `172.66.0.243`),
if the first IP becomes unreachable, half of all requests fail — indefinitely.

---

## Gap today

`ClusterHandle` (the cluster-lifetime handle available in `Init`,
`ServerInitialized`, the `refreshLoop` goroutine via `Schedule`) does not expose
per-host stats. `ClusterLBHandle` (passed per-request to `ChooseHost`) exposes
positional stats but is not safe to hold beyond the `ChooseHost` call.

Required API addition — same gap as the p2c doc:

```go
// HostStatByPtr returns the requested stat for ptr.
// Must be called on the cluster main thread.
HostStatByPtr(ptr HostPtr, stat HostStat) uint64
```

---

## Design

### Per-host baseline tracking

`resolvedUpstream` adds a slice of last-observed `cx_connect_fail` values, one
entry per IP, so the health-check can detect *increments* rather than threshold
absolute values (an IP that always had some failures is not newly broken):

```go
type resolvedUpstream struct {
    addrs          []string
    ptrs           []up.HostPtr
    nextRefresh    time.Time
    rr             atomic.Uint64

    // lastCxFail mirrors the most recent cx_connect_fail reading per IP.
    // Updated on the cluster main thread by healthCheck.
    lastCxFail     []uint64
}
```

### Health-check pass

Piggybacked on the existing `refreshLoop` — no new goroutine needed.
After `applyResolved` finishes reconciling DNS, `healthCheck` runs on the
cluster main thread (already inside the `Schedule` block):

```go
func (c *cluster) healthCheck(h up.ClusterHandle) {
    m := c.hosts.Load()
    if m == nil {
        return
    }
    for _, r := range *m {
        for i, ptr := range r.ptrs {
            current := h.HostStatByPtr(ptr, up.HostStatCxConnectFail)
            if current > r.lastCxFail[i]+healthCheckFailThreshold {
                h.UpdateHostHealth(ptr, up.HostUnhealthy)
                c.logger.Warn("marking host unhealthy: cx_connect_fail threshold exceeded",
                    "addr", r.addrs[i], "last", r.lastCxFail[i], "current", current)
            }
            r.lastCxFail[i] = current
        }
    }
}
```

`healthCheckFailThreshold` is a package-level constant (proposed default: `3`).
An IP that accumulated 3 new connection failures since the last check is ejected.

### Recovery

Unhealthy hosts are re-evaluated on the next `applyResolved` cycle. When DNS
confirms the address is unchanged and the host was previously marked unhealthy,
`applyResolved` calls `UpdateHostHealth(ptr, HostHealthy)` only if
`cx_connect_fail` has stopped growing (i.e. `current == lastCxFail[i]`). If it
is still climbing, the host stays unhealthy.

This is conservative: an IP that briefly recovers and then fails again is not
flapped back to healthy.

```
applyResolved — addr unchanged path:
    keep ptr
    if was unhealthy AND cx_connect_fail unchanged since last check:
        UpdateHostHealth(ptr, HostHealthy)
        reset lastCxFail[i]
```

### Threading

`healthCheck` and the recovery logic in `applyResolved` both call
`HostStatByPtr` and `UpdateHostHealth`. Both run on the cluster main thread
(inside the `Schedule` block in `refreshLoop`). The `lastCxFail` slice lives
inside `*resolvedUpstream` and is only written from the main thread — no
additional synchronisation needed.

---

## Constants

| Name | Proposed default | Notes |
|---|---|---|
| `healthCheckFailThreshold` | `3` | New cx_connect_fail increments before marking unhealthy |

---

## What is NOT in scope

- Active probing (TCP connect / TLS handshake) — passive stat monitoring is
  sufficient and avoids extra connections on the hot path
- Per-provider thresholds from `orange.yaml` — constant threshold is enough for
  the initial implementation
- Envoy outlier detection — the pick cluster owns the LB policy and cannot
  delegate to Envoy's built-in outlier detector without switching to a different
  cluster type
- Circuit breaking — a separate concern; stopping new requests to an unhealthy
  provider is handled at the match/pick boundary via the Decision error path once
  the host is marked unhealthy
