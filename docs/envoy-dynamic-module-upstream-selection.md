# Envoy 1.39 Dynamic Module Upstream Selection

Tested against: `1.39.0-dev / 4616750da8dfc1e3293b7dc8db9fe5093b3ff242`
ABI version: `v0.1.0`
SDK: `github.com/envoyproxy/envoy/source/extensions/dynamic_modules/sdk/go`

This document describes transit's Go wrapper APIs for Envoy dynamic module
upstream selection. The interfaces below track the upstream host-selection ABI
surface exposed by the Envoy commit listed above.

## Problem This Solves

Request-aware upstream selection used to require one of:

- synthetic headers parsed again by Envoy or by a custom load balancer
- route re-selection after changing route-sensitive headers
- one static or CDS-managed cluster per destination

For dynamic LLM or MCP routing, the last option scales poorly. Envoy 1.39's
dynamic module load balancer and cluster extension points let Go code select
hosts directly. Transit exposes both paths:

- **LB Policy**: Envoy owns the cluster and host set. Go only chooses an index.
- **Cluster Extension**: Go owns host discovery and health, then chooses hosts.

## Extension Points

### Load Balancer Policy

Use an LB policy when Envoy already knows the host set through static config,
EDS, or CDS and Go only needs to choose among healthy hosts.

```go
type LBPolicy interface {
    // ChooseHost writes the selected priority and healthy-host index into the
    // output parameters and returns true. Return false for no host, which yields 503.
    ChooseHost(lb LBHandle, ctx LBContext, priority *uint32, index *uint32) bool

    // OnHostMembershipUpdate receives counts. Use lb.MemberUpdateHostAddress
    // during this callback to inspect each added or removed address.
    OnHostMembershipUpdate(lb LBHandle, numAdded, numRemoved int)

    Close()
}
```

Lifecycle:

```text
up.RegisterLBPolicy(name, factory)
LBPolicyFactory.Create(configBytes)        -> LBPolicyConfigFactory
LBPolicyConfigFactory.NewLBPolicy()        -> LBPolicy per worker
LBPolicy.ChooseHost(...)
LBPolicy.OnHostMembershipUpdate(...)
LBPolicy.Close()
LBPolicyConfigFactory.Close()
```

Example:

```go
type firstHostPolicy struct{ up.EmptyLBPolicy }

func (p *firstHostPolicy) ChooseHost(
    lb up.LBHandle,
    _ up.LBContext,
    priority *uint32,
    index *uint32,
) bool {
    if lb.HealthyHostCount(0) == 0 {
        return false
    }
    *priority = 0
    *index = 0
    return true
}
```

### Cluster Extension

Use a cluster extension when Go must discover, add, remove, or health-check
hosts itself, such as DNS on demand, a model registry, or an MCP server catalog.

```go
type ClusterLB interface {
    // Return (host, nil) for sync success, (nil, nil) for sync failure, or
    // (nil, completion) to suspend the stream until completion.Complete is called.
    ChooseHost(lb ClusterLBHandle, ctx ClusterLBContext) (HostPtr, *ClusterLBCompletion)

    // Called if the stream is torn down before async selection completes.
    CancelHostSelection(completion *ClusterLBCompletion)

    // Receives counts. Use lb.MemberUpdateHostAddress during this callback.
    OnHostMembershipUpdate(lb ClusterLBHandle, numAdded, numRemoved int)

    Close()
}
```

Cluster lifecycle:

```text
up.RegisterCluster(name, factory)
ClusterFactory.Create(configBytes)         -> ClusterConfigFactory
ClusterConfigFactory.NewCluster(handle)    -> Cluster
Cluster.Init(handle)                       -> call handle.PreInitComplete()
Cluster.NewClusterLB()                     -> ClusterLB per worker
Cluster.ServerInitialized(handle)
Cluster.DrainStarted(handle)
Cluster.Shutdown(handle, done)             -> call done when finished
Cluster.Close()
ClusterConfigFactory.Close()
```

## Passing Routing Intent From HTTP Filters

Transit's `up.Writer` exposes two SDK-backed methods for passing routing intent
from an HTTP filter to upstream selection:

```go
func onRequest(w *up.Writer, r *up.Request) {
    w.SetFilterState("llm.target", "api.openai.com:443")

    // Prefer this host. If strict is false, Envoy can fall back to normal
    // load balancing when the host is unavailable.
    ok := w.SetUpstreamOverrideHost("api.openai.com:443", false)
    _ = ok
}
```

`ClusterLBContext` can read both values:

```go
target, ok := ctx.GetFilterState("llm.target")
host, strict := ctx.GetOverrideHost()
```

`LBContext` can read only the override host, downstream headers, hash key, and
retry context. It cannot read filter state or downstream SNI in transit's
current wrapper. If selection depends on arbitrary per-request state, use the
Cluster Extension path.

## Current Context APIs

```go
type LBContext interface {
    GetAllHeaders() [][2]string
    GetOverrideHost() (addr string, strict bool)
    GetHeader(name string) (string, bool)
    ComputeHashKey() (uint64, bool)
    GetHostSelectionRetryCount() uint32
    ShouldSelectAnotherHost(lb LBHandle, priority uint32, index int) bool
}

type ClusterLBContext interface {
    GetAllHeaders() [][2]string
    GetFilterState(key string) (string, bool)
    GetFilterStateTyped(key string) (string, bool)
    GetOverrideHost() (addr string, strict bool)
    GetHeader(name string) (string, bool)
    GetDownstreamSNI() (string, bool)
    ComputeHashKey() (uint64, bool)
    GetHostSelectionRetryCount() uint32
    ShouldSelectAnotherHost(lb ClusterLBHandle, priority uint32, index int) bool
    NewCompletion() *ClusterLBCompletion
}
```

## Current Host-Set APIs

```go
type LBHandle interface {
    ClusterName() string
    PriorityCount() int
    HostCount(priority uint32) int
    HealthyHostCount(priority uint32) int
    DegradedHostCount(priority uint32) int
    HostAddress(priority uint32, index int) (string, bool)
    HealthyHostAddress(priority uint32, index int) (string, bool)
    HostWeight(priority uint32, index int) uint32
    HealthyHostWeight(priority uint32, index int) uint32
    HostHealth(priority uint32, index int) HostHealth
    HostHealthByAddress(addr string) (HostHealth, bool)
    HostStat(priority uint32, index int, stat HostStat) uint64
    MemberUpdateHostAddress(index int, isAdded bool) (string, bool)
    HostLocality(priority uint32, index int) (region, zone, subZone string, ok bool)
    SetHostData(priority uint32, index int, data uintptr) bool
    GetHostData(priority uint32, index int) (uintptr, bool)
    HostMetadataString(priority uint32, index int, filterName, key string) (string, bool)
    HostMetadataNumber(priority uint32, index int, filterName, key string) (float64, bool)
    HostMetadataBool(priority uint32, index int, filterName, key string) (bool, bool)
    LocalityCount(priority uint32) int
    LocalityHostCount(priority uint32, localityIndex int) int
    LocalityHostAddress(priority uint32, localityIndex, hostIndex int) (string, bool)
    LocalityWeight(priority uint32, localityIndex int) uint32
}

type ClusterLBHandle interface {
    ClusterName() string
    PriorityCount() int
    HostCount(priority uint32) int
    HealthyHostCount(priority uint32) int
    DegradedHostCount(priority uint32) int
    Host(priority uint32, index int) HostPtr
    HealthyHost(priority uint32, index int) HostPtr
    HostAddress(priority uint32, index int) (string, bool)
    HealthyHostAddress(priority uint32, index int) (string, bool)
    HostWeight(priority uint32, index int) uint32
    HealthyHostWeight(priority uint32, index int) uint32
    HostHealth(priority uint32, index int) HostHealth
    HostHealthByAddress(addr string) (HostHealth, bool)
    HostStat(priority uint32, index int, stat HostStat) uint64
    FindHostByAddress(addr string) HostPtr
    MemberUpdateHostAddress(index int, isAdded bool) (string, bool)
    HostLocality(priority uint32, index int) (region, zone, subZone string, ok bool)
    SetHostData(priority uint32, index int, data uintptr) bool
    GetHostData(priority uint32, index int) (uintptr, bool)
    HostMetadataString(priority uint32, index int, filterName, key string) (string, bool)
    HostMetadataNumber(priority uint32, index int, filterName, key string) (float64, bool)
    HostMetadataBool(priority uint32, index int, filterName, key string) (bool, bool)
    LocalityCount(priority uint32) int
    LocalityHostCount(priority uint32, localityIndex int) int
    LocalityHostAddress(priority uint32, localityIndex, hostIndex int) (string, bool)
    LocalityWeight(priority uint32, localityIndex int) uint32
}
```

Host health values:

```go
up.HostUnhealthy
up.HostDegraded
up.HostHealthy
```

Host stat values:

```go
up.HostStatCxConnectFail
up.HostStatCxTotal
up.HostStatRqError
up.HostStatRqSuccess
up.HostStatRqTimeout
up.HostStatRqTotal
up.HostStatCxActive
up.HostStatRqActive
```

`HostStat`, `HostHealth`, `HostAddress`, `HostWeight`, `HostLocality`,
`HostMetadata*`, and `HostData` methods use indexes in the all-host list.
`HealthyHost*` methods use indexes in the healthy-host list. `ShouldSelectAnotherHost`
also takes the candidate index that the load balancer is considering; in normal
healthy-host iteration, pass the healthy-host index.

## Cluster Host Lifecycle

The `ClusterHandle` is passed to `NewCluster`, `Init`, `ServerInitialized`,
`DrainStarted`, and `Shutdown`.

```go
type ClusterHandle interface {
    AddHosts(hosts []HostSpec) []HostPtr
    RemoveHosts(hosts []HostPtr)
    UpdateHostHealth(host HostPtr, health HostHealth)
    FindHostByAddress(addr string) HostPtr
    PreInitComplete()

    // Schedules fn on Envoy's cluster main thread.
    Schedule(fn func())
}
```

`AddHosts`, `RemoveHosts`, `UpdateHostHealth`, `FindHostByAddress`, and
`PreInitComplete` are main-thread cluster operations. Use `Schedule` from
goroutines or worker callbacks:

```go
handle.Schedule(func() {
    ptrs := handle.AddHosts([]up.HostSpec{
        {Address: "10.0.0.10:443", Weight: 1},
    })
    _ = ptrs
})
```

`RemoveHosts` takes the `HostPtr` values returned from `AddHosts` or
`FindHostByAddress`, not `HostSpec` values.

## Async Cluster Selection

Async selection uses a completion object created from the request context. The
module returns the completion to Envoy, performs work in a goroutine, then calls
`completion.Complete(host, errDetail)` unless cancellation wins. `Complete`
returns `false` if the selection was already completed or cancelled.

```go
type dynamicLB struct {
    up.EmptyClusterLB
    cluster *dynamicCluster
    mu      sync.Mutex
    cancel  map[*up.ClusterLBCompletion]context.CancelFunc // initialize in NewClusterLB
}

func (lb *dynamicLB) ChooseHost(
    h up.ClusterLBHandle,
    ctx up.ClusterLBContext,
) (up.HostPtr, *up.ClusterLBCompletion) {
    target, ok := ctx.GetFilterState("llm.target")
    if !ok || target == "" {
        return nil, nil
    }
    if ptr := h.FindHostByAddress(target); ptr != nil {
        return ptr, nil
    }

    completion := ctx.NewCompletion()
    reqCtx, cancel := context.WithCancel(lb.cluster.ctx)
    lb.mu.Lock()
    lb.cancel[completion] = cancel
    lb.mu.Unlock()

    go func() {
        defer func() {
            lb.mu.Lock()
            delete(lb.cancel, completion)
            lb.mu.Unlock()
        }()

        host, port, err := net.SplitHostPort(target)
        if err != nil {
            _ = completion.Complete(nil, err.Error())
            return
        }
        addrs, err := net.DefaultResolver.LookupHost(reqCtx, host)
        if err != nil {
            _ = completion.Complete(nil, err.Error())
            return
        }
        resolved := net.JoinHostPort(addrs[0], port)

        lb.cluster.handle.Schedule(func() {
            ptr := lb.cluster.handle.FindHostByAddress(resolved)
            if ptr == nil {
                ptrs := lb.cluster.handle.AddHosts([]up.HostSpec{{Address: resolved}})
                if len(ptrs) > 0 {
                    ptr = ptrs[0]
                }
            }
            _ = completion.Complete(ptr, "")
        })
    }()

    return nil, completion
}

func (lb *dynamicLB) CancelHostSelection(completion *up.ClusterLBCompletion) {
    lb.mu.Lock()
    cancel := lb.cancel[completion]
    delete(lb.cancel, completion)
    lb.mu.Unlock()

    if cancel != nil {
        cancel()
    }
}
```

Transit calls the completion's internal cancel guard before invoking
`CancelHostSelection`, so `completion.Complete` returns `false` after
cancellation.

## Common Patterns

### Prefer an Override Host

An HTTP filter can set a host affinity decision without changing headers or
forcing route re-selection:

```go
func onRequest(w *up.Writer, r *up.Request) {
    endpoint := modelRegistry.Lookup(r.Header("x-model"))
    _ = w.SetUpstreamOverrideHost(endpoint, false)
}

func (p *policy) ChooseHost(
    h up.LBHandle,
    ctx up.LBContext,
    priority *uint32,
    index *uint32,
) bool {
    if host, strict := ctx.GetOverrideHost(); host != "" {
        if idx, ok := indexOfHealthyHost(h, host); ok {
            *priority = 0
            *index = uint32(idx)
            return true
        }
        if strict {
            return false
        }
    }

    if h.HealthyHostCount(0) == 0 {
        return false
    }
    *priority = 0
    *index = 0
    return true
}

func indexOfHealthyHost(h up.LBHandle, addr string) (int, bool) {
    for i := 0; i < h.HealthyHostCount(0); i++ {
        got, ok := h.HealthyHostAddress(0, i)
        if ok && got == addr {
            return i, true
        }
    }
    return 0, false
}
```

### Skip Already-Tried Hosts On Retry

```go
func (p *policy) ChooseHost(
    h up.LBHandle,
    ctx up.LBContext,
    priority *uint32,
    index *uint32,
) bool {
    for i := 0; i < h.HealthyHostCount(0); i++ {
        if ctx.ShouldSelectAnotherHost(h, 0, i) {
            continue
        }
        *priority = 0
        *index = uint32(i)
        return true
    }
    return false
}
```

### Mark Hosts Unhealthy From Background Work

Use `up.Group` or another owned lifecycle to stop background checks during
cluster shutdown. Schedule the Envoy host update onto the cluster main thread:

```go
g := up.NewGroup()
g.AddGoroutine(func(ctx context.Context) {
    ticker := time.NewTicker(30 * time.Second)
    defer ticker.Stop()

    for {
        select {
        case <-ctx.Done():
            return
        case <-ticker.C:
            cluster.handle.Schedule(func() {
                cluster.handle.UpdateHostHealth(hostPtr, up.HostUnhealthy)
            })
        }
    }
})
g.Start()
```

## Current Go Method To ABI Mapping

HTTP filter routing intent:

| Transit Go | ABI |
|---|---|
| `w.SetFilterState(key, value)` | `envoy_dynamic_module_callback_http_set_filter_state_bytes` |
| `w.SetUpstreamOverrideHost(host, strict)` | `envoy_dynamic_module_callback_http_set_upstream_override_host` |

Cluster LB context:

| Transit Go | ABI |
|---|---|
| `ctx.GetAllHeaders()` | `envoy_dynamic_module_callback_cluster_lb_context_get_downstream_headers` |
| `ctx.GetFilterState(key)` | `envoy_dynamic_module_callback_cluster_lb_context_get_filter_state_bytes` |
| `ctx.GetFilterStateTyped(key)` | `envoy_dynamic_module_callback_cluster_lb_context_get_filter_state_typed` |
| `ctx.GetOverrideHost()` | `envoy_dynamic_module_callback_cluster_lb_context_get_override_host` |
| `ctx.GetHeader(name)` | `envoy_dynamic_module_callback_cluster_lb_context_get_downstream_header` |
| `ctx.GetDownstreamSNI()` | `envoy_dynamic_module_callback_cluster_lb_context_get_downstream_connection_sni` |
| `ctx.ComputeHashKey()` | `envoy_dynamic_module_callback_cluster_lb_context_compute_hash_key` |
| `ctx.GetHostSelectionRetryCount()` | `envoy_dynamic_module_callback_cluster_lb_context_get_host_selection_retry_count` |
| `ctx.ShouldSelectAnotherHost(lb, p, i)` | `envoy_dynamic_module_callback_cluster_lb_context_should_select_another_host` |
| `completion.Complete(host, err)` | `envoy_dynamic_module_callback_cluster_lb_async_host_selection_complete` |

Cluster LB host set:

| Transit Go | ABI |
|---|---|
| `lb.ClusterName()` | `envoy_dynamic_module_callback_cluster_lb_get_cluster_name` |
| `lb.PriorityCount()` | `envoy_dynamic_module_callback_cluster_lb_get_priority_set_size` |
| `lb.HostCount(priority)` | `envoy_dynamic_module_callback_cluster_lb_get_hosts_count` |
| `lb.HealthyHostCount(priority)` | `envoy_dynamic_module_callback_cluster_lb_get_healthy_host_count` |
| `lb.DegradedHostCount(priority)` | `envoy_dynamic_module_callback_cluster_lb_get_degraded_hosts_count` |
| `lb.Host(p, i)` | `envoy_dynamic_module_callback_cluster_lb_get_host` |
| `lb.HealthyHost(p, i)` | `envoy_dynamic_module_callback_cluster_lb_get_healthy_host` |
| `lb.HostAddress(p, i)` | `envoy_dynamic_module_callback_cluster_lb_get_host_address` |
| `lb.HealthyHostAddress(p, i)` | `envoy_dynamic_module_callback_cluster_lb_get_healthy_host_address` |
| `lb.HostWeight(p, i)` | `envoy_dynamic_module_callback_cluster_lb_get_host_weight` |
| `lb.HealthyHostWeight(p, i)` | `envoy_dynamic_module_callback_cluster_lb_get_healthy_host_weight` |
| `lb.HostHealth(p, i)` | `envoy_dynamic_module_callback_cluster_lb_get_host_health` |
| `lb.HostHealthByAddress(addr)` | `envoy_dynamic_module_callback_cluster_lb_get_host_health_by_address` |
| `lb.HostStat(p, i, stat)` | `envoy_dynamic_module_callback_cluster_lb_get_host_stat` |
| `lb.FindHostByAddress(addr)` | `envoy_dynamic_module_callback_cluster_lb_find_host_by_address` |
| `lb.MemberUpdateHostAddress(i, added)` | `envoy_dynamic_module_callback_cluster_lb_get_member_update_host_address` |
| `lb.HostLocality(p, i)` | `envoy_dynamic_module_callback_cluster_lb_get_host_locality` |
| `lb.SetHostData(p, i, data)` | `envoy_dynamic_module_callback_cluster_lb_set_host_data` |
| `lb.GetHostData(p, i)` | `envoy_dynamic_module_callback_cluster_lb_get_host_data` |
| `lb.HostMetadataString(p, i, filter, key)` | `envoy_dynamic_module_callback_cluster_lb_get_host_metadata_string` |
| `lb.HostMetadataNumber(p, i, filter, key)` | `envoy_dynamic_module_callback_cluster_lb_get_host_metadata_number` |
| `lb.HostMetadataBool(p, i, filter, key)` | `envoy_dynamic_module_callback_cluster_lb_get_host_metadata_bool` |
| `lb.LocalityCount(priority)` | `envoy_dynamic_module_callback_cluster_lb_get_locality_count` |
| `lb.LocalityHostCount(priority, locality)` | `envoy_dynamic_module_callback_cluster_lb_get_locality_host_count` |
| `lb.LocalityHostAddress(priority, locality, host)` | `envoy_dynamic_module_callback_cluster_lb_get_locality_host_address` |
| `lb.LocalityWeight(priority, locality)` | `envoy_dynamic_module_callback_cluster_lb_get_locality_weight` |

Cluster lifecycle:

| Transit Go | ABI |
|---|---|
| `handle.AddHosts(hosts)` | `envoy_dynamic_module_callback_cluster_add_hosts` |
| `handle.RemoveHosts(hostPtrs)` | `envoy_dynamic_module_callback_cluster_remove_hosts` |
| `handle.UpdateHostHealth(host, health)` | `envoy_dynamic_module_callback_cluster_update_host_health` |
| `handle.PreInitComplete()` | `envoy_dynamic_module_callback_cluster_pre_init_complete` |
| `handle.Schedule(fn)` | `envoy_dynamic_module_callback_cluster_scheduler_commit` |

LB policy callbacks use the `lb` ABI prefix instead of `cluster_lb` for the
equivalent LB policy context and host-set callbacks.
