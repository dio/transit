# Envoy Dynamic Module Upstream Selection

The ABI version, SDK module, and SDK commit this document tracks are defined in
`down/abi_impl/VERSION`. The Envoy binary used for local runs and e2e tests is
built by [dio/envoy-builder](https://github.com/dio/envoy-builder) and tagged
`envoy-{8-char SDK commit}`. The Makefile derives the download URL from
`VERSION` automatically — `make download-envoy` always fetches the binary that
matches the pinned SDK.

Current pin (from `down/abi_impl/VERSION`):

```
SDK_MODULE=github.com/envoyproxy/envoy/source/extensions/dynamic_modules
SDK_VERSION=v0.0.0-20260521055639-0d6e3c60aa55
SDK_COMMIT=0d6e3c60aa55
```

ABI version: `v0.1.0`

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
EDS, or CDS and Go only needs to choose among healthy hosts. Your Go type
implements `up.LBPolicy`: `ChooseHost` writes priority+index and returns
`true`, or returns `false` to yield 503. `OnHostMembershipUpdate` notifies
when the host set changes (use `lb.MemberUpdateHostAddress` to inspect
addresses during this callback). `Close` releases per-worker resources.

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

Example — always pick the first healthy host:

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
hosts itself, such as DNS on demand, a model registry, or an MCP server
catalog. Your Go type implements `up.ClusterLB`: `ChooseHost` can return a
host synchronously, return `nil, nil` for immediate failure, or return a
`ClusterLBCompletion` to suspend the stream while a goroutine does async work.
`CancelHostSelection` fires if the stream tears down before the goroutine
finishes — call the stored `cancel` func there. `OnHostMembershipUpdate`
mirrors the LB Policy callback.

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

**LB Policy vs Cluster Extension for per-request state**: `LBContext` can read
the override host, downstream headers, and hash key, but it cannot read filter
state or downstream SNI. If selection depends on arbitrary per-request state
(e.g. a model ID written by an earlier filter), use the Cluster Extension path
and read it via `ctx.GetFilterState`.

## Cluster Host Lifecycle

`ClusterHandle` is the write side for host management (passed to `NewCluster`,
`Init`, `ServerInitialized`, `DrainStarted`, and `Shutdown`). The key rule:
`AddHosts`, `RemoveHosts`, `UpdateHostHealth`, and `PreInitComplete` are
main-thread cluster operations. Always use `Schedule` when calling them from a
goroutine or worker callback:

```go
handle.Schedule(func() {
    ptrs := handle.AddHosts([]up.HostSpec{
        {Address: "10.0.0.10:443", Weight: 1},
    })
    _ = ptrs
})
```

`RemoveHosts` takes the `HostPtr` values returned from `AddHosts` or
`FindHostByAddress`, not `HostSpec` values — keep the pointers from `AddHosts`
if you need to remove hosts later.

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
g.Go(func(ctx context.Context) {
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
```

## Go Method To ABI Mapping

Cross-reference for contributors modifying `down/` or debugging ABI drift.
These tables map each transit Go call to the underlying C ABI symbol.

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
