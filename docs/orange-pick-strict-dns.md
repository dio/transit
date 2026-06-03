# orange/pick: StrictDNS-style upstream refresh

## Problem

`pick.Init` resolves each upstream hostname once at cluster startup and stores the
resulting `ip:port` in a static `HostPtr` map. For a long-running server this is
unsafe:

- LLM provider endpoints can change IP (CDN failover, maintenance, etc.)
- A stale IP goes unhealthy silently — traffic black-holes until restart
- Kubernetes DNS caches are short-TTL by design; a one-shot resolve at startup
  may already be stale within minutes

Envoy's built-in `STRICT_DNS` cluster type solves this by continuously re-resolving
and reconciling the host set. The pick cluster needs the same behaviour.

---

## Design

### Core types

```go
// resolvedUpstream pairs the resolved addr with its ClusterHandle HostPtr so
// the refresh loop can diff and remove stale entries without an extra lookup.
type resolvedUpstream struct {
    addr string     // "ip:port" last registered with the cluster
    ptr  up.HostPtr // opaque handle returned by AddHosts
}
```

`cluster.hosts` changes from `atomic.Pointer[map[string]up.HostPtr]` to
`atomic.Pointer[map[string]resolvedUpstream]`. The atomic publish keeps
`ChooseHost` lock-free on the hot path; the refresh goroutine is the only writer.

### resolveAll — incremental reconciliation

```
for each provider in config:
    resolve hostname → new addr
    if resolve fails  → keep old entry (don't evict a healthy host on a DNS hiccup)
    if addr unchanged → keep old ptr (no cluster churn)
    if addr changed   → RemoveHosts(old), AddHosts(new), UpdateHostHealth(Healthy)
    if new provider   → AddHosts, UpdateHostHealth(Healthy)

for each name in current hosts not in new map:
    RemoveHosts(old)   // provider deleted from config
atomic-store new map
```

Key invariant: a transient DNS failure does **not** remove a previously healthy
host. Only a confirmed new address triggers a remove+add cycle.

### Refresh loop

Started in `ServerInitialized` (after `Init` has registered the initial host set).
Cancelled via context in `Shutdown`.

Wakes at the earliest `nextRefresh` across all registered upstreams. Falls back to
`defaultDNSRefreshInterval` when no hosts are registered (e.g. all Init resolves failed).

```
loop:
    delay = max(time.Until(earliestNextRefresh()), minTTLFloor)
    select ctx.Done       → return
    select time.After(delay) → resolveAll(ctx, h)
```

### Shutdown ordering

```
Shutdown:
    stopRefresh()   // cancel refresh goroutine
    stopConfig()    // stop config file watcher
    done()          // signal Envoy
```

---

## Constants

| Name | Default | Notes |
|---|---|---|
| `defaultResolveTimeout` | 5s | Per-refresh DNS timeout; covers k8s ndots chaining |
| `defaultDNSRefreshInterval` | 30s | Fallback wake interval when no hosts are registered |
| `minTTLFloor` | 10s | Clamps short TTLs; also the retry delay after a DNS failure |

---

## DNS TTL-aware refresh interval

`github.com/miekg/dns` is used instead of `net.DefaultResolver` so the minimum
TTL from the A-record answer section is available. Each resolved upstream stores
its own `nextRefresh = time.Now().Add(max(ttl, minTTLFloor))`. The refresh loop
wakes at the earliest `nextRefresh` across all upstreams via `earliestNextRefresh()`,
so a provider with a 60s TTL is re-resolved every 60s while one with a 300s TTL
is left alone for 5 minutes — no unnecessary churn, no stale caches.

On a transient DNS failure the existing host is preserved and `nextRefresh` is
reset to `now + minTTLFloor` so a retry happens soon without hammering the resolver.

---

---

## Multi-IP round-robin (extended design)

The original design stored a single `addr`/`ptr` pair per provider (`pickFirstAddr`
always picked the lexicographically smallest IPv4). This caused silent failure when
that IP became unreachable while the provider had other healthy IPs.

The updated design:

**`resolvedUpstream`** holds parallel `[]addrs` / `[]ptrs` slices plus an
`atomic.Uint64 rr` counter. The struct is heap-allocated (`*resolvedUpstream` in
the map) so `rr` is never copied.

**`pickAddrs`** replaces `pickFirstAddr`. It returns all IPv4 addresses sorted by
byte value. Sorting makes the slice stable regardless of DNS round-robin rotation,
so `applyResolved` can detect IP-set changes by slice equality rather than
permutation — no spurious remove/add churn.

**`applyResolved` reconciliation** per provider:
```
for each addr in new resolved set:
    if addr already registered → retain existing HostPtr
    if addr is new             → AddHosts, UpdateHostHealth(Healthy)
for each addr in old set not in new set:
    RemoveHosts
```

**`lookupHost` round-robin**: `rr.Add(1) % uint64(len(ptrs))` — one atomic
increment, zero allocations.

**rr counter preservation**: when the IP set is unchanged across a DNS refresh,
`old.rr.Load()` is copied into the new entry. This prevents the counter from
resetting to 0 on every TTL expiry, which would bunch requests onto `ptrs[0]`
at the start of each refresh cycle.

---

## What is NOT in scope

- Per-upstream refresh intervals from config
- IPv6 support beyond the current fallback (all major LLM providers are IPv4)
- p2c selection — see docs/orange-pick-p2c.md
- cx_connect_fail → UpdateHostHealth feedback — see docs/orange-pick-host-health-feedback.md
