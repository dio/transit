# orange/pick

Cluster extension that selects an upstream host for each request based on the
`match.Decision` placed in the per-stream object bag by the `orange-match`
downstream filter.

---

## Lifecycle

| Callback | Thread | What happens |
|---|---|---|
| `Init` | main | `config.InitLogger` (required before any `config.Get`); DNS-resolves all provider endpoints; registers IP:port hosts; calls `PreInitComplete`. Providers whose DNS resolution fails are skipped with a warning — the refresh loop retries them. |
| `ServerInitialized` | main | Starts config polling (`config.Start`), file watching (`config.EnableFileWatch`), and the DNS refresh loop goroutine. |
| `NewClusterLB` | main | Returns an `lb` wrapping `up.AsyncHostSelector`; called per LB context. |
| `Shutdown` | main | Cancels refresh loop and config poll; waits; calls `done()`. |

---

## STRICT_DNS-style refresh

### Problem

`Init` resolves each upstream hostname once at cluster startup. For a
long-running server this is unsafe:

- LLM provider endpoints can change IP (CDN failover, maintenance, etc.)
- A stale IP goes unhealthy silently — traffic black-holes until restart
- Kubernetes DNS caches are short-TTL by design; a one-shot resolve at startup
  may already be stale within minutes

The refresh loop re-resolves every provider on TTL expiry and reconciles the
host set — the same behaviour as Envoy's built-in `STRICT_DNS` cluster type.

### Reconciliation

Run on the main thread via `applyResolved`:

```
for each provider in config snapshot:
    resolve hostname → new addrs
    if resolve fails  → keep old entry; set nextRefresh = now + minTTLFloor
    if addrs unchanged → keep existing HostPtrs and rr counter; update nextRefresh
    if addr added      → AddHosts for new addr
    if addr removed    → RemoveHosts for gone addr
    if new provider    → AddHosts for all resolved addrs

for each name in current hosts not in new snapshot:
    RemoveHosts   // provider deleted from config
atomic-store new map
```

Key invariant: a transient DNS failure does **not** evict a previously healthy
host. Only a confirmed new address triggers a remove+add cycle.

### Refresh loop

Started in `ServerInitialized` after `Init` has registered the initial host
set. Cancelled via context in `Shutdown`.

```
loop:
    delay = max(time.Until(earliestNextRefresh()), minTTLFloor)
    select ctx.Done          → return
    select time.After(delay) → resolveAddrs(ctx); applyResolved via Schedule
```

Falls back to `defaultDNSRefreshInterval` when no hosts are registered (e.g.
all Init resolves failed).

### Shutdown ordering

```
Shutdown:
    stopRefresh()   // cancel refresh goroutine
    stopFileWatch() // stop fsnotify watcher
    stopConfig()    // stop config poll
    done()          // signal Envoy
```

### Constants

| Name | Default | Notes |
|---|---|---|
| `defaultResolveTimeout` | 5 s | Per-refresh DNS timeout; covers k8s ndots chaining |
| `defaultDNSRefreshInterval` | 30 s | Fallback wake interval when no hosts are registered |
| `minTTLFloor` | 10 s | Clamps short TTLs; also the retry delay after a DNS failure |

### DNS TTL-aware scheduling

`github.com/miekg/dns` is used instead of `net.DefaultResolver` so the minimum
TTL from the A-record answer section is available. Each upstream stores
`nextRefresh = now + max(ttl, minTTLFloor)`. The loop wakes at the earliest
`nextRefresh` across all upstreams, so a 60s-TTL provider is re-resolved every
60s while a 300s-TTL one is left alone for 5 minutes — no unnecessary churn.

On a transient DNS failure the existing host is preserved and `nextRefresh` is
reset to `now + minTTLFloor` so a retry happens soon without hammering the
resolver.

---

## Multi-IP round-robin

Each provider may resolve to multiple A records (e.g. `api.openai.com` returns
two Cloudflare IPs). All IPs are registered as distinct `HostPtr`s so a single
unreachable IP does not black-hole the provider.

`resolvedUpstream` holds parallel `addrs []string` and `ptrs []HostPtr` slices
plus an `atomic.Uint64` round-robin counter (`rr`). `lookupHost` increments `rr`
and selects `ptrs[rr % len(ptrs)]` — lock-free on the hot path.

`resolvedUpstream` is always heap-allocated (`*resolvedUpstream` in the map) so
`rr` is never copied (`atomic.Uint64` has a `noCopy` guard). `applyResolved`
carries the old `rr` value into the new entry on IP-set changes, preventing a
counter reset from bunching requests onto `ptrs[0]`.

`pickAddrs` sorts A records by byte value before returning them so that DNS
round-robin rotation produces a stable slice order, allowing `applyResolved` to
detect IP-set changes by slice equality rather than permutation — no spurious
remove/add churn.

### Planned upgrade: power-of-two-choices

Round-robin ignores request cost. LLM traffic has high latency variance: a
short prompt completes in < 1 s while a long context window can take 30 s+.
With round-robin, a slow host accumulates a backlog while the fast host sits
idle.

P2C fixes this: pick two candidate IPs at random, compare their active-request
counts, route to the one with fewer. The randomisation avoids the
thundering-herd of pure least-connections while the comparison avoids the
hot-host problem of pure random.

**Blocking gap:** `ClusterHandle` does not yet expose `HostStatByPtr`. The
positional `ClusterLBHandle.HostStat(priority, index, stat)` API exists but
requires scanning all hosts to map a `HostPtr` to its index — O(n) and fragile
when the host set changes. Once `HostStatByPtr` is added to `ClusterHandle`,
`lookupHost` can be replaced with:

```go
func p2cPick(h up.ClusterHandle, ptrs []up.HostPtr) up.HostPtr {
    if len(ptrs) == 1 {
        return ptrs[0]
    }
    i := rand.IntN(len(ptrs))
    j := rand.IntN(len(ptrs) - 1)
    if j >= i { j++ }
    a, b := ptrs[i], ptrs[j]
    if h.HostStatByPtr(a, up.HostStatRqActive) <= h.HostStatByPtr(b, up.HostStatRqActive) {
        return a
    }
    return b
}
```

`lookupHost` runs on the cluster main thread (via `AsyncHostSelector.complete`
→ `Schedule`), satisfying the main-thread requirement that `HostStatByPtr`
will carry.

---

## Cluster main-thread contract

`ClusterHandle` has strict threading rules. Violations produce a silent no-op
and an `envoy_bug` log line:

```
[error][envoy_bug] [source/extensions/clusters/dynamic_modules/abi_impl.cc:240]
envoy bug failure: false. Details:
envoy_dynamic_module_callback_cluster_update_host_health must be called on the main thread
```

The same error fires for `AddHosts` / `RemoveHosts` violations (different line
numbers).

| Method | Restriction |
|---|---|
| `AddHosts` | main thread only |
| `RemoveHosts` | main thread only |
| `UpdateHostHealth` | main thread only |
| `FindHostByAddress` | main thread only |
| `PreInitComplete` | main thread only |
| `Schedule(fn)` | **thread-safe** — safe from any goroutine |

### When cluster callbacks run on the main thread

- `Init(h)` — main thread; blocking DNS here is acceptable since Envoy waits
  for `PreInitComplete`.
- `ServerInitialized(h)` — main thread; safe to call `AddHosts` etc. here, but
  the idiomatic pattern is to launch goroutines and use `Schedule` to marshal
  mutations back.
- `NewClusterLB()` — main thread.
- `DrainStarted(h)` / `Shutdown(h, done)` — main thread.
- `ClusterLB.ChooseHost` / `CancelHostSelection` — **worker thread**; must not
  call `ClusterHandle` methods directly (only via `Schedule`).

### Two-phase pattern for background refresh loops

Split any function that mixes IO (DNS, HTTP, file) with host mutations into two
phases. Call phase 1 on the goroutine; schedule phase 2 back to the main thread:

```go
// Phase 1: IO — goroutine-safe
addrs := c.resolveAddrs(rctx)

// Phase 2: host mutations — main thread
done := make(chan struct{})
c.handle.Schedule(func() {
    c.applyResolved(h, addrs)
    close(done)
})
<-done   // wait so next TTL computation sees updated nextRefresh
```

**Why not schedule the whole function including DNS?** DNS resolution is
blocking (tens to hundreds of milliseconds). Running it on the Envoy main thread
stalls all other cluster operations for the duration of the lookup.

**Why not skip the `<-done` wait?** Without it the goroutine immediately
computes the next sleep using stale TTL data (`applyResolved` hasn't run yet).
On the first few iterations this produces a tight spin at `minTTLFloor` until
the scheduled function catches up.

### Diagnostics

```bash
# Health counter should increment on every successful refresh cycle.
curl -s http://127.0.0.1:9901/stats | grep upstream_rq_total

# Per-host health status.
curl -s http://127.0.0.1:9901/clusters | grep health_flags
```

If hosts appear in `/clusters` but `health_flags` shows a stale unhealthy
state, the most likely cause is `UpdateHostHealth` being silently dropped
because it was called off the main thread.

---

## Body-driven host selection (async pattern)

### Why filter state doesn't work

`ChooseHost` runs during header processing, before the request body arrives.
The naive design — write routing state in a body callback, read it in
`ChooseHost` — does not work:

1. `match.OnRequestHeaders` → `Continue` (body not yet read)
2. Router opens upstream connection pool → **`ChooseHost` runs here**
3. Body arrives → `match.bodyHandler` writes routing state → **too late**

`HeadersStatusStopAllAndBuffer` is explicitly unsupported — the SDK has no
async resume path for that status and it freezes the filter chain permanently.

Symptom: `upstream_cx_none_healthy` increments even though
`membership_healthy > 0`. The cluster has hosts but `ChooseHost` returned nil
because filter state was empty.

### The async completion pattern

`match` mints a `*up.StreamPromise[Decision]` at headers time and stores it in
the per-stream object bag. `pick.ChooseHost` delegates to
`up.AsyncHostSelector`, which returns a `ClusterLBCompletion` and installs an
`OnResolve` callback on the promise. When the body arrives and `match` resolves
the promise, the callback fires, hops back to the cluster main thread via
`Schedule`, and completes host selection. No per-request goroutine is parked.

### The `:authority` / `auto_sni` trap

For HTTPS upstreams with `auto_sni: true` + `auto_san_validation: true`, do
**not** rewrite `:authority` from an upstream HTTP filter — `auto_sni` is
sampled from the request **before** the upstream filter chain runs (TLS
handshake happens first).

Empirically with the `ClusterLBCompletion` pattern:

- `:authority` set from the **downstream headers** handler reaches the TLS handshake.
- `:authority` set from the **body** handler does **not**, even though the upstream
  HTTP filter sees the post-mutation value.

The most consistent explanation: Envoy's router captures the SNI from
`:authority` during `router.decodeHeaders`, before `ChooseHost` runs. When
`ChooseHost` returns a `ClusterLBCompletion`, the host attaches later but the
SNI is already locked.

Consequence: body-driven routing across providers with different hostnames
**cannot rely on `auto_sni` alone** when `:authority` is written from a body
callback. The fix used here: `match` writes the correct provider hostname into
`:authority` from its **headers** handler (where the model is not yet known,
but the provider can be defaulted), and then corrects it in the body handler.
Since the downstream headers handler runs before `router.decodeHeaders`, the
SNI is captured from the correct value.

### Phase-ordering cheat sheet

```
                downstream phase                    upstream phase
client → HCM → match.decodeHeaders       → router → ChooseHost
             → match.decodeData (body)             → upstream HTTP filters
                                                      (adapt, meter)
                                                   → TLS handshake
                                                      (auto_sni reads :authority)
                                                   → headers written upstream
```

Rules that fall out:

- Anything that must influence `ChooseHost` must be in place before
  `router.decodeHeaders` finishes — either in the request headers, in filter
  state written from a downstream headers callback, or behind a
  `ClusterLBCompletion`.
- Anything that must influence the TLS handshake (`:authority` for `auto_sni`)
  must be in place before the upstream conn pool opens — i.e. downstream phase,
  not upstream filters.
- Upstream HTTP filters (`adapt`, `meter`) are for credential injection, header
  stripping, and final request shaping — **not** a routing extension point.

### Diagnostics

```bash
# Distinguishes "ChooseHost returned nil" from "connection broke after selection".
curl -s http://127.0.0.1:9901/stats | grep -E 'orange|none_healthy|connect_fail'

# Per-host cx counters: cx_connect_fail on the wrong host's IP means routing
# picked correctly but TLS/transport broke.
curl -s http://127.0.0.1:9901/clusters
```

---

## Runtime hosts without xDS

All upstream hosts on the `orange-pick` cluster are added at runtime by
`applyResolved` via `ClusterHandle.AddHosts` — never via xDS. This is the
designed entry point for every host that orange ever needs to dial,
including fallback / retry targets.

The custom Envoy build used here enables `auto_host_sni` and a bounded
SNI-scoped TLS session cache on the `orange-pick` upstream TLS context
(see https://gist.github.com/dio/965d1e555909c02013ca882a2b3caa78). With
that substrate already in place, runtime-added hosts get their SNI from
the `HostSpec` rather than from static xDS, and TLS session reuse is
bounded per-SNI so a churning host set does not blow out cache memory.

Practical consequence: if you need a new upstream addressable from
`ChooseHost` (a new provider binding, a fallback target, a region
shard), the path is **always** "load it into the config snapshot →
`applyResolved` calls `AddHosts`." Do not reach for xDS, Backend CRDs,
or `EnvoyPatchPolicy` for this.

---

## Non-goals

- Per-upstream refresh intervals from config (all upstreams share the TTL-based schedule)
- IPv6 (all major LLM providers are IPv4; the DNS query is `TypeA` only)
- P2C selection — blocked on `ClusterHandle.HostStatByPtr`; see design above
- `cx_connect_fail` → `UpdateHostHealth` feedback loop
