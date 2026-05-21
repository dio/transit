# Envoy 1.39 dynamic module upstream selection

Tested against: `1.39.0-dev / 4616750da8dfc1e3293b7dc8db9fe5093b3ff242`
ABI version: `v0.1.0`
SDK: `github.com/envoyproxy/envoy/source/extensions/dynamic_modules/sdk/go`

---

## Problem this solves

Previous Envoy approaches to request-aware upstream selection required one of:

- **Synthetic headers** — an HTTP filter writes `x-envoy-upstream-override` or
  custom subset metadata headers, Envoy re-parses them on every request
- **Route re-selection** — modifying `:authority` or a per-route action to
  trigger a second route match
- **One cluster per destination** — static or CDS-managed clusters, one per
  upstream target; scales poorly when targets are dynamic or numerous

For an LLM-MCP-Proxy, the last problem is the most acute. This proxy handles
two distinct traffic types:

- **LLM traffic** — chat completions, model routing, per-key fallback chains,
  traffic splitting across providers (GPT, Claude, Gemini, …)
- **MCP traffic** — JSON-RPC tool calls; `tools/list` fans out to all servers in
  a user's profile, `tools/call` routes to the one server that owns the tool

Neither can pre-declare a cluster for every possible destination. The Cluster
Extension solves this with a **single cluster whose host set is populated at
runtime** — the same model Envoy's own Dynamic Forward Proxy uses internally,
but driven by Go code.

Dynamic modules in 1.39 give two first-class extension points for this.

---

## Two extension points

### A — Load Balancer Policy (light path)

Envoy still manages the host set via EDS or static config. The module only
implements `ChooseHost`.

**When to use:** you have a normal cluster but want custom pick logic —
consistent hashing by model ID, least-active-requests weighted by context
window, ordered fallback across providers.

```go
type LBPolicy interface {
    // Called per request on a worker thread.
    // Return (priority, index) into the healthy host list, or (0, 0, false) for 503.
    ChooseHost(lb LB, ctx LBContext) (priority, index uint32, ok bool)

    // Called when EDS updates arrive — rebuild hash rings, address maps, etc.
    OnHostMembershipUpdate(lb LB, hostsAdded, hostsRemoved []string)
}
```

**Lifecycle:**
```
NewLBPolicyConfig(configBytes)   → LBPolicyConfig   (parse config once)
LBPolicyConfig.NewLBPolicy()     → LBPolicy          (per worker thread)
LBPolicy.ChooseHost(...)         → (priority, index, ok)
LBPolicy.OnHostMembershipUpdate(...)
LBPolicy.Close()
LBPolicyConfig.Close()
```

---

### B — Cluster Extension (heavy path)

The module owns the entire cluster: host discovery, health tracking, and
per-worker LB. Use when hosts come from a source other than EDS — e.g., a model
registry, a control plane, DNS resolved on-demand.

```go
type ClusterLB interface {
    // Called per request on a worker thread. Three modes:
    //   sync success  → return host, nil, nil
    //   sync failure  → return nil, nil, nil  (→ 503)
    //   async pending → return nil, nil, handle (→ Envoy suspends request)
    ChooseHost(lb ClusterLB, ctx ClusterLBContext) (host HostPtr, async AsyncHandle)

    // Stream torn down before async completes — cancel the goroutine.
    CancelHostSelection(handle AsyncHandle)

    // EDS-style update from module-driven host discovery.
    OnHostMembershipUpdate(lb ClusterLB, hostsAdded, hostsRemoved []string)
}

// Complete an async selection from any goroutine.
// host == nil → upstream failure; errDetail is logged.
func (lb ClusterLB) AsyncComplete(ctx ClusterLBContext, host HostPtr, errDetail string)
```

**Lifecycle:**
```
NewClusterConfig(configBytes)    → ClusterConfig     (parse config once)
ClusterConfig.NewCluster()       → Cluster           (cluster instance)
Cluster.Init()                   → discover hosts; call PreInitComplete() when ready
Cluster.NewClusterLB()           → ClusterLB         (per worker thread)
ClusterLB.ChooseHost(...)
ClusterLB.CancelHostSelection(...)
ClusterLB.OnHostMembershipUpdate(...)
ClusterLB.Close()
Cluster.ServerInitialized()      → PostInit, workers not yet started
Cluster.DrainStarted()
Cluster.Shutdown(done func())    → must call done() when draining complete
Cluster.Close()
ClusterConfig.Close()
```

---

## Single cluster as a dynamic forward proxy

The Cluster Extension's host set is mutable at runtime. One cluster declaration
in bootstrap config is enough — hosts are added and removed by the module as
destinations are discovered. This is the same internal model as Envoy's built-in
`envoy.clusters.dynamic_forward_proxy`; the difference is that your Go code
drives it instead of c-ares.

**Config side:** one cluster entry, custom type pointing at your `.so`:
```yaml
clusters:
  - name: llm_dynamic
    cluster_type:
      name: transit.cluster.dynamic    # your module's registered name
      typed_config: { ... }
    # no load_assignment — the module populates hosts
```

**Runtime flow for an unseen destination:**

```
Request → ClusterLB.ChooseHost
│
├─ read target from filter state ("llm.target" = "api.openai.com:443")
├─ look up in per-worker Go map: addr → HostPtr
│
├─ HIT  → return host, nil   (sync, ~0 ns)
│
└─ MISS → return nil, handle
           │
           └─ goroutine
               ├─ net.LookupHost("api.openai.com")
               ├─ if new IP: PostToMainThread →
               │    cluster.AddHosts([]HostSpec{{Address: resolved}})
               │    update per-worker map
               └─ lb.AsyncComplete(ctx, hostPtr, "")
```

Once a host is added, Envoy manages its connection pool exactly like a static
host. Subsequent requests for the same target return synchronously.

**Host lifecycle methods on `Cluster`:**
```go
cluster.AddHosts(hosts []HostSpec)                          // new endpoints
cluster.RemoveHosts(hosts []HostSpec)                       // TTL expiry, 404 from registry
cluster.UpdateHostHealth(host HostPtr, h HostHealth)        // Healthy | Degraded | Unhealthy
```

TTL management is your responsibility — a background goroutine sweeps the cache
and calls `RemoveHosts` for stale entries.

**Thread safety:** `AddHosts` / `RemoveHosts` / `UpdateHostHealth` must be called
on the cluster's main thread, not directly from a worker goroutine. Use
`cluster.PostToMainThread(func())` — same pattern as `w.Continue()` from an HTTP
filter goroutine.

**Goroutine lifecycle:** use `up.Group` for all background work in the cluster —
TTL sweeper, health checker, registry subscription. Async per-request goroutines
(DNS resolution in `ChooseHost`) derive their context from the group's `ctx`, so
a single `g.Stop()` in `Cluster.Close()` tears everything down cleanly:

```go
type myCluster struct {
    g   *up.Group
    // ...
}

func newCluster() *myCluster {
    g := up.NewGroup()
    c := &myCluster{g: g}

    g.AddGoroutine(func(ctx context.Context) {
        t := time.NewTicker(30 * time.Second)
        defer t.Stop()
        for {
            select {
            case <-ctx.Done():
                return
            case <-t.C:
                c.sweepExpiredHosts() // calls PostToMainThread → RemoveHosts
            }
        }
    })
    g.Start()
    return c
}

func (c *myCluster) Close() { c.g.Stop() }
```

Per-request goroutines in `ChooseHost` derive from `g.ctx` so cluster shutdown
cancels them automatically; `CancelHostSelection` handles the per-request
early-cancel case (stream torn down mid-flight).

**Comparison with built-in DFP:**

| | Built-in DFP | Cluster Extension (Go) |
|---|---|---|
| Resolver | c-ares only | Any Go resolver, SRV, service registry, MCP catalog |
| Cache TTL | From DNS response | You decide |
| Happy eyeballs / IPv4+6 | Built-in | Pick one address or implement yourself |
| Selection logic | Round-robin on resolved IPs | Full `ChooseHost` — stats, metadata, fallback |
| Async path | Internal | First-class: return `AsyncHandle`, complete from goroutine |
| Cancellation | Internal | `CancelHostSelection` → `context.CancelFunc` |

---

## Passing a routing decision from the HTTP filter to the LB

The HTTP filter (already in Go via transit's `up` package) writes the routing
decision into per-request state. The LB module reads it in `ChooseHost`.

### Override host (direct)

```go
// HTTP filter — sets a specific host on the LoadBalancerContext
w.SetUpstreamOverrideHost("api.openai.com:443", strict bool)
// strict=true  → use this exact host or 503
// strict=false → prefer this host, fall through to ChooseHost if unavailable
```

```go
// LB module — reads it
host, strict := ctx.GetOverrideHost()
```

### Filter state (arbitrary key→value)

```go
// HTTP filter writes
w.SetFilterState("llm.target", "api.openai.com:443")
```

```go
// LB module reads (inside ChooseHost)
target, ok := ctx.GetFilterState("llm.target")
```

There is no magic field name — you choose the key. A common convention:

| Key | Written by | Read by |
|---|---|---|
| `llm.target` | HTTP filter (from `:authority` or request body) | Cluster LB `ChooseHost` |
| `llm.backends` | HTTP filter (ordered fallback list) | LB `ChooseHost` |
| `mcp.tool` | HTTP filter (from JSON-RPC `params.name`) | Cluster LB `ChooseHost` |
| `tenant.pinned_host` | HTTP filter (from tenant store) | LB `ChooseHost` |

---

## What `LBContext` / `ClusterLBContext` exposes

```go
type LBContext interface {
    // Downstream request
    GetHeader(name string) (string, bool)
    GetAllHeaders() []Header

    // State written by earlier HTTP filters
    GetFilterState(key string) (string, bool)

    // Host set by HTTP filter via SetUpstreamOverrideHost
    GetOverrideHost() (addr string, strict bool)

    // Downstream TLS SNI
    GetDownstreamSNI() string

    // Consistent hash key (for hash-based LB policies)
    ComputeHashKey() uint64

    // Retry context
    GetHostSelectionRetryCount() uint32
    ShouldSelectAnotherHost(lb LB, priority, index uint32) bool
}
```

`ClusterLBContext` has the same surface; it also carries the async-completion
callback used by `lb.AsyncComplete`.

---

## What `LB` / `ClusterLB` exposes about the host set

```go
type LB interface {
    // Counts
    PriorityCount() int
    HostCount(priority uint32) int
    HealthyHostCount(priority uint32) int
    DegradedHostCount(priority uint32) int

    // Address lookup
    HostAddress(priority, index uint32) string
    HealthyHostAddress(priority, index uint32) string
    FindHostByAddress(addr string) HostPtr   // O(1) map lookup across all priorities
    HostHealthByAddress(addr string) HostHealth

    // Per-host stats (live Envoy counters)
    HostStat(priority, index uint32, stat HostStat) uint64
    // HostStat values: CxConnectFail, CxTotal, CxActive,
    //                  RqError, RqSuccess, RqTimeout, RqTotal, RqActive

    // Health
    HostHealth(priority, index uint32) HostHealth
    // HostHealth values: Unhealthy, Degraded, Healthy

    // Weight
    HostWeight(priority, index uint32) uint32
    HealthyHostWeight(priority, index uint32) uint32

    // Locality
    HostLocality(priority, index uint32) (region, zone, subZone string)
    LocalityCount(priority uint32) int
    LocalityHostCount(priority, localityIdx uint32) int
    LocalityWeight(priority, localityIdx uint32) uint32

    // Endpoint metadata (from EDS or static config)
    HostMetadataString(priority, index uint32, filterName, key string) (string, bool)
    HostMetadataFloat(priority, index uint32, filterName, key string) (float64, bool)
    HostMetadataBool(priority, index uint32, filterName, key string) (bool, bool)

    // Per-worker per-host scratch (moving averages, hash ring data, etc.)
    // Stored as uintptr_t — cast to/from unsafe.Pointer or a packed uint64.
    SetHostData(priority, index uint32, data uintptr)
    GetHostData(priority, index uint32) (uintptr, bool)
}
```

`ClusterLB` embeds `LB` and adds:
```go
    // Only valid inside OnHostMembershipUpdate
    MemberUpdateHostAddress(index uint32, isAdded bool) string
```

---

## Old vs. new

| Aspect | Old | New (1.39 Go module) |
|---|---|---|
| Pass routing decision to LB | Synthetic header, parsed every request | `w.SetFilterState` / `w.SetUpstreamOverrideHost`, read once in `ChooseHost` |
| Route re-selection | Modify `:authority`, trigger second route match | Not needed — `ChooseHost` returns `HostPtr` directly |
| Async selection | Requires ext_proc round-trip | First-class: return `AsyncHandle`, complete from goroutine |
| Custom service discovery | EDS only | `Cluster.AddHosts` / `RemoveHosts` / `UpdateHostHealth` |
| Per-host scratch state | External map keyed by IP string | `SetHostData` / `GetHostData` per-worker per-host |
| Host-level observability | Built-in stats only | `HostStat` exposes active conns, RQ counts, error rates per-host |
| Cluster count | One per destination | One total — host set populated at runtime |

---

## Per-user MCP profiles

An MCP profile is a user's configured set of MCP servers — e.g. `filesystem`,
`github`, `postgres`. Two very different request patterns flow through the same
proxy, and they use the upstream selection machinery differently.

### Pattern A — fan-out: `tools/list`

"List all tools available to this user" means querying every MCP server in their
profile and aggregating the results. This is **not** a single-host selection
problem — it is parallel fan-out, handled entirely in the HTTP filter before the
LB is involved.

```
client → tools/list
         │
         └─ HTTP filter detects method == "tools/list"
             ├─ load profile: [{name:"filesystem", addr:"mcp-fs:8080"},
             │                 {name:"github",     addr:"mcp-gh:8080"},
             │                 {name:"postgres",   addr:"mcp-pg:8080"}]
             ├─ issue parallel subrequests to each server in profile
             │   ├─ GET mcp-fs:8080/tools  → ["read_file","write_file"]
             │   ├─ GET mcp-gh:8080/tools  → ["list_prs","create_issue"]
             │   └─ GET mcp-pg:8080/tools  → ["query","exec"]
             └─ aggregate & return combined tool list to client
```

Servers that are down or return errors are skipped from the aggregate (soft
fallback) or cause the whole request to fail (hard fallback) — controlled by
the profile.

```go
func OnRequestHeaders(w *up.Writer, r *up.Request) {
    if method(r) != "tools/list" {
        return
    }
    userID := r.Header("x-user-id")
    profile := profileStore.Get(userID)

    // fan-out to all servers, skip unhealthy ones
    var tools []Tool
    for _, srv := range profile.Servers {
        result, err := fetchTools(srv.Addr) // parallel in practice
        if err != nil {
            if profile.FallbackHard {
                w.SendLocalReply(502, "MCP server unavailable: "+srv.Name)
                return
            }
            continue // soft: skip this server's tools
        }
        tools = append(tools, result...)
    }
    // cache tool→server mapping in filter state for subsequent tool calls
    w.SetFilterState("mcp.tool_map", encodeToolMap(tools))
    w.SendLocalReply(200, encodeToolList(tools))
}
```

### Pattern B — single-host routing: `tools/call`

"Call this specific tool" routes to exactly one server — the one that owns the
tool. The filter resolves the server from the tool map (cached from the
`tools/list` fan-out or from the profile directly) and writes it into filter
state. The LB module picks the host.

```go
// HTTP filter — OnRequestHeaders for tools/call
func OnRequestHeaders(w *up.Writer, r *up.Request) {
    toolName := gjson.GetBytes(r.BufferedBody(), "params.name").String()
    userID   := r.Header("x-user-id")

    // tool_map was set during tools/list, or resolve directly from profile
    toolMap  := loadToolMap(ctx.GetFilterState("mcp.tool_map"), userID)
    srv      := toolMap[toolName] // e.g. {name:"filesystem", addr:"mcp-fs:8080"}

    w.SetFilterState("mcp.target", srv.Addr)
    w.SetFilterState("mcp.server_name", srv.Name)
}

// Cluster LB — ChooseHost
func (lb *myClusterLB) ChooseHost(h ClusterLB, ctx ClusterLBContext) (HostPtr, AsyncHandle) {
    target, _ := ctx.GetFilterState("mcp.target")
    if ptr := h.FindHostByAddress(target); ptr != nil {
        return ptr, nil
    }
    // not yet in host set — resolve async (same DNS pattern as use case 4)
    return lb.resolveAsync(h, ctx, target)
}
```

### Per-user rules that apply to both patterns

Each profile entry carries rules beyond just the server address:

```go
type MCPServerEntry struct {
    Name         string
    Addr         string
    RateLimit    int            // max calls/min this user may make to this server
    TokenBudget  int            // for LLM-backed MCP servers: max tokens/request
    Split        map[string]int // canary: {"stable":90, "canary":10} — addr variants
    FallbackHard bool           // server unavailable: hard fail vs. skip
}
```

| Rule | Where enforced |
|---|---|
| Which MCP servers are in the profile | HTTP filter (profile lookup, once per request) |
| Fan-out to all servers for `tools/list` | HTTP filter (parallel subrequests) |
| Soft/hard fallback when a server is down | HTTP filter (fan-out) + LB `ChooseHost` |
| Rate limit per user per server | HTTP filter (check + decrement before fan-out or `SetFilterState`) |
| Token budget for LLM-backed servers | HTTP filter (write budget); LB routes to cheaper server when low |
| Traffic split / canary for a server | LB module `weightedRandom(entry.Split)` inside `ChooseHost` |
| Health of a server's host | Cluster Extension background goroutine → `UpdateHostHealth` |
| Retry across servers | LB `ShouldSelectAnotherHost` + profile fallback list |

### Traffic splitting for canary MCP server deployments

When rolling out a new version of an MCP server, the profile carries split
weights instead of a single address. The LB module resolves which variant to
use:

```go
// profile entry: Split: {"mcp-fs-stable:8080": 90, "mcp-fs-canary:8080": 10}

func (lb *myLB) ChooseHost(h LB, ctx LBContext) (priority, index uint32, ok bool) {
    srvName, _ := ctx.GetFilterState("mcp.server_name")
    entry := lb.profileEntry(srvName) // loaded during filter, stored in per-worker state

    if len(entry.Split) > 0 {
        chosen := weightedRandom(entry.Split) // "mcp-fs-canary:8080" 10% of the time
        if idx, found := h.IndexOfHealthyHost(chosen); found {
            return 0, idx, true
        }
        // canary down → fall back to stable
    }

    if idx, found := h.IndexOfHealthyHost(entry.Addr); found {
        return 0, idx, true
    }
    return 0, 0, false
}

---

## Per-key LLM routing policies

API keys (or user identities) can carry their own LLM routing rules independent
of any MCP profile. The same filter-state mechanism applies: the HTTP filter
resolves the key's policy once, writes it into filter state, and the LB module
executes it in `ChooseHost`.

```go
type KeyPolicy struct {
    // Fallback
    FallbackMode string         // "auto" | "custom"
    CustomChain  []string       // only used when FallbackMode == "custom"

    // Traffic split — applies before fallback
    Split        map[string]int // {"gpt4o": 80, "claude-3-5": 20}

    // Rate limits — checked before selection
    RateLimit    map[string]RateWindow // keyed by model name, or "*" for any
}

type RateWindow struct {
    Scope     string // "key" | "user"
    Remaining int    // requests left in current window
}
```

**HTTP filter resolves the policy:**

```go
func OnRequestHeaders(w *up.Writer, r *up.Request) {
    key    := r.Header("authorization") // or x-api-key
    model  := gjson.GetBytes(r.BufferedBody(), "model").String()
    policy := keyStore.Get(key)

    // check rate limit for this key+model before going further
    rl := policy.RateLimit[model]
    if rl.Remaining == 0 {
        rl = policy.RateLimit["*"] // fall back to global limit for this key
    }
    if rl.Remaining <= 0 {
        w.SendLocalReply(429, `{"error":"rate limit exceeded"}`)
        return
    }
    keyStore.Decrement(key, model) // atomic decrement in background store

    encoded, _ := json.Marshal(policy)
    w.SetFilterState("key.policy", string(encoded))
    w.SetFilterState("key.model", model)
}
```

### Auto fallback vs. custom fallback

**Auto fallback** — the LB module decides which backends to try based on live
health and stats. The key policy just opts in:

```go
func (lb *myLB) ChooseHost(h LB, ctx LBContext) (priority, index uint32, ok bool) {
    var p KeyPolicy
    json.Unmarshal([]byte(ctx.GetFilterState("key.policy")), &p)

    if p.FallbackMode == "auto" {
        // pick best healthy host by error rate, regardless of model preference
        return lb.pickLeastErrorHost(h, ctx)
    }
    // custom: walk the user's own chain
    for _, addr := range p.CustomChain {
        if idx, found := h.IndexOfHealthyHost(addr); found {
            return 0, idx, true
        }
    }
    return 0, 0, false
}
```

**Custom fallback** — the user has configured their own ordered chain, e.g.:
```json
{"fallbackMode": "custom", "customChain": ["gpt4o:443", "claude-3-5:443", "gemini-2:443"]}
```
If `gpt4o` is down or over quota, the LB tries `claude-3-5`, then `gemini-2`,
then 503. The user controls the order; the LB controls the health check.

### Per-key traffic splitting

20% of a key's requests go to Claude, 80% to GPT — regardless of which model
the request nominally asks for. Useful for A/B testing or gradual migration.

```go
func (lb *myLB) ChooseHost(h LB, ctx LBContext) (priority, index uint32, ok bool) {
    var p KeyPolicy
    json.Unmarshal([]byte(ctx.GetFilterState("key.policy")), &p)

    if len(p.Split) > 0 {
        chosen := weightedRandom(p.Split) // "claude-3-5:443" 20% of the time
        if idx, found := h.IndexOfHealthyHost(chosen); found {
            return 0, idx, true
        }
        // split target down → fall through to custom chain or auto
    }

    // ... rest of fallback logic
}
```

Split targets that are down degrade gracefully: the weight of the missing target
is implicitly redistributed to whatever the fallback path selects next.

### Rate limiting: per-key vs. per-user, per-model

Rate limits are checked in the HTTP filter (before `ChooseHost`) so the LB never
sees over-budget requests. The `Scope` field controls the counter namespace:

| Scope | Counter key | Effect |
|---|---|---|
| `"key"` | `ratelimit:{apiKey}:{model}` | Each API key gets its own quota per model |
| `"user"` | `ratelimit:{userID}:{model}` | Multiple keys sharing a user share one quota |
| `"*"` model | `ratelimit:{apiKey}:*` | Global cap across all models for this key |

```go
// policy example:
RateLimit: map[string]RateWindow{
    "gpt4o":      {Scope: "key",  Remaining: 500},  // 500 gpt4o calls/min for this key
    "claude-3-5": {Scope: "user", Remaining: 1000}, // 1000 claude calls/min shared by user
    "*":          {Scope: "key",  Remaining: 2000}, // 2000 total calls/min for this key
}
```

The filter checks model-specific limit first, falls back to `"*"` if no
model-specific entry exists. A 429 is returned before the LB is involved — no
wasted `ChooseHost` work for over-budget requests.

### Summary of what lives where

| Concern | HTTP filter | LB module |
|---|---|---|
| Resolve key → policy | ✓ (once, I/O-bound) | |
| Rate limit check + decrement | ✓ (before selection) | |
| Traffic split decision | | ✓ `weightedRandom(p.Split)` |
| Auto fallback (health-based) | | ✓ `pickLeastErrorHost` |
| Custom fallback (user-ordered) | | ✓ walk `p.CustomChain` |
| Retry skip (already-tried host) | | ✓ `ShouldSelectAnotherHost` |
| Background health gating | | ✓ `UpdateHostHealth` |

---

## Fallback

Fallback logic lives in the module — the filter sets *intent*, the LB module
executes *selection with fallback*.

### Layer 1 — proactive: skip unhealthy hosts inside `ChooseHost`

No retry loop needed. The module walks the preference list and returns the first
acceptable host.

```go
func (lb *myLB) ChooseHost(h LB, ctx LBContext) (priority, index uint32, ok bool) {
    list, _ := ctx.GetFilterState("llm.backends") // "gpt4o:443,claude:443,gemini:443"
    for _, addr := range strings.Split(list, ",") {
        ptr := h.FindHostByAddress(addr)
        if ptr == nil || h.HostHealthByAddress(addr) != Healthy {
            continue
        }
        // optional: skip high-error hosts
        errs := h.HostStat(0, indexOf(addr), RqError)
        total := h.HostStat(0, indexOf(addr), RqTotal)
        if total > 100 && float64(errs)/float64(total) > 0.1 {
            continue
        }
        return 0, indexOf(addr), true
    }
    return 0, 0, false // all down → 503
}
```

### Layer 2 — reactive: skip already-tried hosts on retry

Envoy calls `ChooseHost` again on each retry. `ctx.ShouldSelectAnotherHost`
returns `true` for hosts that already failed this request.

```go
for i := uint32(0); i < uint32(h.HealthyHostCount(0)); i++ {
    if ctx.ShouldSelectAnotherHost(h, 0, i) {
        continue // Envoy: already tried and failed
    }
    return 0, i, true
}
```

### Layer 3 — strict vs. soft override host

```go
w.SetUpstreamOverrideHost("gpt4o-1:443", false) // strict=false
// → prefer this host; if absent from healthy set, fall through to ChooseHost
```

### Layer 4 — background health gating

A background goroutine health-checks backends out-of-band and calls:
```go
cluster.PostToMainThread(func() {
    cluster.UpdateHostHealth(ptr, Unhealthy)
})
```
Once marked unhealthy, `HealthyHostCount` excludes it and Envoy's built-in
retry logic avoids it automatically.

### What belongs where

| Logic | Where |
|---|---|
| Which backends are acceptable for this request | HTTP filter → `SetFilterState` |
| Pick best among acceptable, skip unhealthy | LB module `ChooseHost` |
| Skip already-tried host on retry | `ctx.ShouldSelectAnotherHost` |
| Persistent health from out-of-band checks | `cluster.UpdateHostHealth` via `PostToMainThread` |
| No host at all | `ChooseHost` returns `false` → Envoy 503 |

---

## LLM-MCP-Proxy use cases

### 1. Model-aware routing without header manipulation

HTTP filter parses the request once and sets the override host. The LB picks it
up in `ChooseHost` with no further header manipulation.

```go
// HTTP filter — OnRequestHeaders
func OnRequestHeaders(w *up.Writer, r *up.Request) {
    body := r.BufferedBody()
    model := gjson.GetBytes(body, "model").String()
    endpoint := modelRegistry.Lookup(model) // "gpt4o-pool.internal:443"
    w.SetUpstreamOverrideHost(endpoint, false) // strict=false → fallback if down
}

// LB Policy — ChooseHost
func (lb *myLB) ChooseHost(h LB, ctx LBContext) (priority, index uint32, ok bool) {
    if host, strict := ctx.GetOverrideHost(); host != "" {
        if idx, found := h.IndexOfHealthyHost(host); found {
            return 0, idx, true
        }
        if strict {
            return 0, 0, false // 503
        }
        // soft: fall through
    }
    // pick any healthy host
    if h.HealthyHostCount(0) == 0 {
        return 0, 0, false
    }
    return 0, 0, true
}
```

---

### 2. Least-active-requests weighted by context window

LB Policy picks the host with the lowest weighted cost, using live Envoy stats.
No external state needed.

```go
func (lb *myLB) ChooseHost(h LB, ctx LBContext) (priority, index uint32, ok bool) {
    n := h.HealthyHostCount(0)
    if n == 0 {
        return 0, 0, false
    }
    best, bestScore := 0, uint64(math.MaxUint64)
    for i := 0; i < n; i++ {
        active := h.HostStat(0, uint32(i), RqActive)
        tokenLoad, _ := h.GetHostData(0, uint32(i)) // estimated tokens-in-flight
        score := active*1000 + uint64(tokenLoad)
        if score < bestScore {
            bestScore = score
            best = i
        }
    }
    return 0, uint32(best), true
}
```

---

### 3. Tenant-pinned sessions

Filter writes the pinned host; LB does a direct address lookup — bypasses index
iteration entirely.

```go
// HTTP filter — OnRequestHeaders
func OnRequestHeaders(w *up.Writer, r *up.Request) {
    tenantID := r.Header("x-tenant-id")
    if pinned := tenantStore.GetPinnedHost(tenantID); pinned != "" {
        w.SetFilterState("tenant.pinned_host", pinned)
    }
}

// LB Policy — ChooseHost
func (lb *myLB) ChooseHost(h LB, ctx LBContext) (priority, index uint32, ok bool) {
    if pinned, ok := ctx.GetFilterState("tenant.pinned_host"); ok {
        if ptr := h.FindHostByAddress(pinned); ptr != nil {
            idx := lb.ptrToIndex[ptr] // maintained in OnHostMembershipUpdate
            return 0, idx, true
        }
        // pinned host gone — fall through or return false per policy
    }
    return 0, 0, h.HealthyHostCount(0) > 0
}
```

---

### 4. Single cluster for all destinations (no cluster explosion)

One cluster in bootstrap. All LLM/MCP endpoints are added as hosts at runtime
via Go DNS. No CDS, no per-destination cluster declaration.

```go
// HTTP filter — OnRequestHeaders
func OnRequestHeaders(w *up.Writer, r *up.Request) {
    target := r.Header(":authority") // "api.openai.com:443"
    w.SetFilterState("llm.target", target)
}

// Cluster LB — ChooseHost
func (lb *myClusterLB) ChooseHost(h ClusterLB, ctx ClusterLBContext) (HostPtr, AsyncHandle) {
    target, _ := ctx.GetFilterState("llm.target")

    // fast path
    if ptr := h.FindHostByAddress(target); ptr != nil {
        return ptr, nil
    }

    // slow path: DNS resolution needed
    // derive from group ctx so cluster shutdown cancels in-flight goroutines
    reqCtx, cancel := context.WithCancel(lb.cluster.g.Ctx())
    pending := &pending{cancel: cancel}

    go func() {
        defer cancel()
        host, port, _ := net.SplitHostPort(target)
        addrs, err := net.DefaultResolver.LookupHost(reqCtx, host)
        if err != nil {
            h.AsyncComplete(ctx, nil, err.Error())
            return
        }
        resolved := net.JoinHostPort(addrs[0], port)
        lb.cluster.PostToMainThread(func() {
            lb.cluster.AddHosts([]HostSpec{{Address: resolved, Hostname: target}})
        })
        h.AsyncComplete(ctx, h.FindHostByAddress(resolved), "")
    }()
    return nil, pending
}

func (lb *myClusterLB) CancelHostSelection(handle AsyncHandle) {
    handle.(*pending).cancel()
}
```

---

### 5. MCP server catalog as the resolver

Same pattern as (4) but the async step queries an MCP catalog instead of DNS.
The logical tool name resolves to a server endpoint.

```go
// HTTP filter — OnRequestHeaders
func OnRequestHeaders(w *up.Writer, r *up.Request) {
    body := r.BufferedBody()
    toolName := gjson.GetBytes(body, "params.name").String() // "filesystem"
    w.SetFilterState("mcp.tool", toolName)
}

// Cluster LB — ChooseHost
func (lb *myClusterLB) ChooseHost(h ClusterLB, ctx ClusterLBContext) (HostPtr, AsyncHandle) {
    tool, _ := ctx.GetFilterState("mcp.tool")
    cacheKey := "mcp:" + tool

    if ptr := h.FindHostByAddress(cacheKey); ptr != nil {
        return ptr, nil
    }

    reqCtx, cancel := context.WithCancel(lb.cluster.g.Ctx())
    go func() {
        defer cancel()
        endpoint, err := mcpCatalog.Resolve(reqCtx, tool) // "mcp-fs-server:8080"
        if err != nil {
            h.AsyncComplete(ctx, nil, err.Error())
            return
        }
        lb.cluster.PostToMainThread(func() {
            lb.cluster.AddHosts([]HostSpec{{Address: endpoint, Hostname: cacheKey}})
        })
        h.AsyncComplete(ctx, h.FindHostByAddress(cacheKey), "")
    }()
    return nil, &pending{cancel: cancel}
}
```

---

### 6. Ordered fallback list

Filter writes an ordered preference list; LB picks the first healthy one without
triggering Envoy's retry loop.

```go
// HTTP filter — OnRequestHeaders
func OnRequestHeaders(w *up.Writer, r *up.Request) {
    model := gjson.GetBytes(r.BufferedBody(), "model").String()
    backends := policy.BackendsFor(model) // ["gpt4o:443","claude-3-5:443","gemini-2:443"]
    w.SetFilterState("llm.backends", strings.Join(backends, ","))
}

// LB Policy — ChooseHost
func (lb *myLB) ChooseHost(h LB, ctx LBContext) (priority, index uint32, ok bool) {
    list, _ := ctx.GetFilterState("llm.backends")
    for _, addr := range strings.Split(list, ",") {
        if h.HostHealthByAddress(addr) != Healthy {
            continue
        }
        idx, found := h.IndexOfHealthyHost(addr)
        if !found {
            continue
        }
        errs := h.HostStat(0, idx, RqError)
        total := h.HostStat(0, idx, RqTotal)
        if total > 100 && float64(errs)/float64(total) > 0.1 {
            continue // >10% error rate — skip
        }
        return 0, idx, true
    }
    return 0, 0, false // all backends down → 503
}
```

---

### 7. Adaptive retries (skip already-failed host)

`ctx.ShouldSelectAnotherHost` tells the module which hosts Envoy already tried
for this request. The module combines that with per-host timeout stats.

```go
// LB Policy — ChooseHost
func (lb *myLB) ChooseHost(h LB, ctx LBContext) (priority, index uint32, ok bool) {
    retries := ctx.GetHostSelectionRetryCount()
    n := h.HealthyHostCount(0)
    for i := uint32(0); i < uint32(n); i++ {
        if ctx.ShouldSelectAnotherHost(h, 0, i) {
            continue // already tried and failed this request
        }
        if retries > 0 {
            timeouts := h.HostStat(0, i, RqTimeout)
            total := h.HostStat(0, i, RqTotal)
            if total > 0 && float64(timeouts)/float64(total) > 0.1 {
                continue // high timeout rate — avoid on retry
            }
        }
        return 0, i, true
    }
    return 0, 0, false
}
```

---

## Go SDK entry point

The Go SDK wraps the C ABI. The cluster and LB interfaces above map directly to
it:

```
github.com/envoyproxy/envoy/source/extensions/dynamic_modules/sdk/go/shared
```

HTTP filter bindings (already used by transit's `up` package) live in the same
module. Cluster and LB bindings follow the same pattern: implement the interface,
register with `shared.RegisterCluster` or `shared.RegisterLBPolicy` in `init()`.

---

## Appendix: Go method → C ABI name

### HTTP filter → LB (writing routing intent)

| Go (on `*up.Writer`) | C ABI |
|---|---|
| `w.SetUpstreamOverrideHost(addr, strict)` | `envoy_dynamic_module_callback_http_set_upstream_override_host` |
| `w.SetFilterState(key, value)` | `envoy_dynamic_module_callback_http_set_filter_state_bytes` |

### `LBContext` / `ClusterLBContext` (reading per-request state in `ChooseHost`)

| Go | C ABI |
|---|---|
| `ctx.GetOverrideHost()` | `envoy_dynamic_module_callback_cluster_lb_context_get_override_host` |
| `ctx.GetFilterState(key)` | `envoy_dynamic_module_callback_cluster_lb_context_get_filter_state_bytes` |
| `ctx.GetHeader(name)` | `envoy_dynamic_module_callback_cluster_lb_context_get_downstream_header` |
| `ctx.GetAllHeaders()` | `envoy_dynamic_module_callback_cluster_lb_context_get_downstream_headers` |
| `ctx.GetDownstreamSNI()` | `envoy_dynamic_module_callback_cluster_lb_context_get_downstream_connection_sni` |
| `ctx.ComputeHashKey()` | `envoy_dynamic_module_callback_cluster_lb_context_compute_hash_key` |
| `ctx.GetHostSelectionRetryCount()` | `envoy_dynamic_module_callback_cluster_lb_context_get_host_selection_retry_count` |
| `ctx.ShouldSelectAnotherHost(lb, p, i)` | `envoy_dynamic_module_callback_cluster_lb_context_should_select_another_host` |

### `LB` / `ClusterLB` (reading host set in `ChooseHost`)

| Go | C ABI |
|---|---|
| `lb.PriorityCount()` | `envoy_dynamic_module_callback_cluster_lb_get_priority_set_size` |
| `lb.HostCount(priority)` | `envoy_dynamic_module_callback_cluster_lb_get_hosts_count` |
| `lb.HealthyHostCount(priority)` | `envoy_dynamic_module_callback_cluster_lb_get_healthy_hosts_count` |
| `lb.DegradedHostCount(priority)` | `envoy_dynamic_module_callback_cluster_lb_get_degraded_hosts_count` |
| `lb.HostAddress(p, i)` | `envoy_dynamic_module_callback_cluster_lb_get_host_address` |
| `lb.HealthyHostAddress(p, i)` | `envoy_dynamic_module_callback_cluster_lb_get_healthy_host_address` |
| `lb.FindHostByAddress(addr)` | `envoy_dynamic_module_callback_cluster_lb_find_host_by_address` |
| `lb.HostHealthByAddress(addr)` | `envoy_dynamic_module_callback_cluster_lb_get_host_health_by_address` |
| `lb.HostHealth(p, i)` | `envoy_dynamic_module_callback_cluster_lb_get_host_health` |
| `lb.HostStat(p, i, stat)` | `envoy_dynamic_module_callback_cluster_lb_get_host_stat` |
| `lb.HostWeight(p, i)` | `envoy_dynamic_module_callback_cluster_lb_get_host_weight` |
| `lb.HostLocality(p, i)` | `envoy_dynamic_module_callback_cluster_lb_get_host_locality` |
| `lb.HostMetadataString(p, i, filter, key)` | `envoy_dynamic_module_callback_cluster_lb_get_host_metadata_string` |
| `lb.SetHostData(p, i, data)` | `envoy_dynamic_module_callback_cluster_lb_set_host_data` |
| `lb.GetHostData(p, i)` | `envoy_dynamic_module_callback_cluster_lb_get_host_data` |

### `Cluster` (host lifecycle — call via `PostToMainThread`)

| Go | C ABI |
|---|---|
| `cluster.AddHosts(hosts)` | `envoy_dynamic_module_callback_cluster_add_hosts` |
| `cluster.RemoveHosts(hosts)` | `envoy_dynamic_module_callback_cluster_remove_hosts` |
| `cluster.UpdateHostHealth(host, h)` | `envoy_dynamic_module_callback_cluster_update_host_health` |
| `cluster.PreInitComplete()` | `envoy_dynamic_module_callback_cluster_pre_init_complete` |
| `cluster.PostToMainThread(fn)` | `envoy_dynamic_module_callback_cluster_post_to_main_thread` |

### Async selection

| Go | C ABI |
|---|---|
| `lb.AsyncComplete(ctx, host, err)` | `envoy_dynamic_module_callback_cluster_lb_async_host_selection_complete` |

### LB Policy (light path) — prefix differs

Replace `cluster_lb` with `lb` in the C ABI names above for the LB Policy
extension point. E.g. `get_host_stat` →
`envoy_dynamic_module_callback_lb_get_host_stat`.
