---
name: envoy-abi-wrapper
description: Implement Envoy dynamic module ABI wrappers in Go for transit. Use when adding Cluster Extension or LB Policy support to down/abi_impl/, down/, and up/.
---

# Envoy ABI Wrapper — transit

Specialist for extending transit with new Envoy dynamic module extension points.
Follows the exact pattern established by `down/abi_impl/access_logger.go`.

## Project layout

```
down/
  down.go          ← Go interfaces + Register* functions (no CGO)
  abi_impl/
    abi.h          ← vendored C ABI (synced via make update-sdk)
    internal.go    ← CGO helpers: manager[T], stringToModuleBuffer, hostLog, …
    http.go        ← blank-imports the official SDK ABI (HTTP + network)
    access_logger.go ← full access logger implementation — USE AS TEMPLATE
up/
  up.go            ← re-exports Register* for callers
  group.go         ← up.Group: background goroutine lifecycle (NewGroup, AddGoroutine)
```

## The pattern (from access_logger.go)

Every new extension point follows four steps:

### 1. Go interfaces in `down/down.go`

Define user-facing Go interfaces. No CGO, no C types. Example shape:

```go
type ClusterLB interface {
    ChooseHost(ctx ClusterLBContext) (HostPtr, AsyncHandle)
    CancelHostSelection(handle AsyncHandle)
    OnHostMembershipUpdate(hostsAdded, hostsRemoved []string)
    Close()
}

type ClusterLBFactory interface {
    NewClusterLB() ClusterLB
    Close()
}

// Registry + Register function (same pattern as RegisterAccessLoggerConfigFactory)
func RegisterCluster(name string, f ClusterConfigFactory) { … }
func GetClusterFactory(name string) ClusterConfigFactory  { … }
```

### 2. Wrapper structs in `down/abi_impl/cluster.go`

```go
package abi_impl

/*
#include "abi.h"
*/
import "C"
import (
    "unsafe"
    "github.com/dio/transit/down"
)

type clusterConfigWrapper struct{ factory down.ClusterFactory }
type clusterWrapper      struct{ cluster down.Cluster }
type clusterLBWrapper    struct{ lb      down.ClusterLB }

var (
    clusterConfigManager = newManager[clusterConfigWrapper]()
    clusterManager       = newManager[clusterWrapper]()
    clusterLBManager     = newManager[clusterLBWrapper]()
)
```

### 3. Handle types implementing the Go interfaces

Each Envoy pointer type gets a Go handle struct that calls back through C:

```go
type dymClusterLBContext struct {
    envoyPtr C.envoy_dynamic_module_type_cluster_lb_context_envoy_ptr
    lbPtr    C.envoy_dynamic_module_type_cluster_lb_envoy_ptr
}

func (ctx *dymClusterLBContext) GetFilterState(key string) (string, bool) {
    var buf C.envoy_dynamic_module_type_envoy_buffer
    ok := bool(C.envoy_dynamic_module_callback_cluster_lb_context_get_filter_state_bytes(
        ctx.envoyPtr, stringToModuleBuffer(key), &buf,
    ))
    runtime.KeepAlive(key)
    if !ok { return "", false }
    return envoyBufferToStringUnsafe(buf), true
}
```

### 4. `//export` functions called by Envoy

```go
//export envoy_dynamic_module_on_cluster_config_new
func envoy_dynamic_module_on_cluster_config_new(
    configEnvoyPtr C.envoy_dynamic_module_type_cluster_config_envoy_ptr,
    name C.envoy_dynamic_module_type_envoy_buffer,
    config C.envoy_dynamic_module_type_envoy_buffer,
) C.envoy_dynamic_module_type_cluster_config_module_ptr {
    nameStr := envoyBufferToStringUnsafe(name)
    f := down.GetClusterFactory(nameStr)
    if f == nil { return nil }
    // … create wrapper, record in manager, return ptr
}
```

---

## Cluster Extension ABI exports (all must be implemented)

```
// Config lifecycle (main thread)
envoy_dynamic_module_on_cluster_config_new(configEnvoyPtr, name, config) → config_module_ptr
envoy_dynamic_module_on_cluster_config_destroy(config_module_ptr)

// Cluster instance lifecycle (main thread)
envoy_dynamic_module_on_cluster_new(config_module_ptr, cluster_envoy_ptr) → cluster_module_ptr
envoy_dynamic_module_on_cluster_init(cluster_envoy_ptr, cluster_module_ptr)
envoy_dynamic_module_on_cluster_destroy(cluster_module_ptr)
envoy_dynamic_module_on_cluster_server_initialized(cluster_envoy_ptr, cluster_module_ptr)
envoy_dynamic_module_on_cluster_drain_started(cluster_envoy_ptr, cluster_module_ptr)
envoy_dynamic_module_on_cluster_shutdown(cluster_envoy_ptr, cluster_module_ptr, completion_cb)

// Per-worker LB lifecycle
envoy_dynamic_module_on_cluster_lb_new(cluster_envoy_ptr, cluster_module_ptr, lb_envoy_ptr) → lb_module_ptr
envoy_dynamic_module_on_cluster_lb_choose_host(lb_envoy_ptr, lb_module_ptr, context_envoy_ptr, host_out, async_handle_out)
envoy_dynamic_module_on_cluster_lb_cancel_host_selection(lb_envoy_ptr, lb_module_ptr, async_handle)
envoy_dynamic_module_on_cluster_lb_on_host_membership_update(lb_envoy_ptr, lb_module_ptr, num_added, num_removed)
envoy_dynamic_module_on_cluster_lb_destroy(lb_module_ptr)
```

## LB Policy ABI exports

```
envoy_dynamic_module_on_lb_config_new(config_envoy_ptr, name, config) → lb_config_module_ptr
envoy_dynamic_module_on_lb_config_destroy(lb_config_module_ptr)
envoy_dynamic_module_on_lb_new(lb_envoy_ptr, lb_config_module_ptr) → lb_module_ptr
envoy_dynamic_module_on_lb_choose_host(lb_envoy_ptr, lb_module_ptr, context_envoy_ptr, priority_out, index_out) → bool
envoy_dynamic_module_on_lb_on_host_membership_update(lb_envoy_ptr, lb_module_ptr, num_added, num_removed)
envoy_dynamic_module_on_lb_destroy(lb_module_ptr)
```

## Key C callbacks available on cluster_lb_envoy_ptr / lb_envoy_ptr

```c
// Host set inspection (identical prefix for both: cluster_lb or lb)
envoy_dynamic_module_callback_{prefix}_get_hosts_count(lb, priority)
envoy_dynamic_module_callback_{prefix}_get_healthy_hosts_count(lb, priority)
envoy_dynamic_module_callback_{prefix}_get_host_address(lb, priority, index, result)
envoy_dynamic_module_callback_{prefix}_find_host_by_address(lb, address) → host_ptr
envoy_dynamic_module_callback_{prefix}_get_host_health(lb, priority, index) → health
envoy_dynamic_module_callback_{prefix}_get_host_stat(lb, priority, index, stat) → uint64
envoy_dynamic_module_callback_{prefix}_get_host_metadata_string(lb, priority, index, filter, key, result)
envoy_dynamic_module_callback_{prefix}_set_host_data(lb, priority, index, data)
envoy_dynamic_module_callback_{prefix}_get_host_data(lb, priority, index, data_out)

// Context inspection
envoy_dynamic_module_callback_cluster_lb_context_get_filter_state_bytes(ctx, key, result)
envoy_dynamic_module_callback_cluster_lb_context_get_override_host(ctx, addr, strict)
envoy_dynamic_module_callback_cluster_lb_context_get_downstream_header(ctx, key, result, idx, size)
envoy_dynamic_module_callback_cluster_lb_context_get_host_selection_retry_count(ctx) → uint32
envoy_dynamic_module_callback_cluster_lb_context_should_select_another_host(lb, ctx, priority, index) → bool

// Async completion (Cluster Extension only)
envoy_dynamic_module_callback_cluster_lb_async_host_selection_complete(lb, ctx, host, details)

// Host lifecycle (main thread only — use PostToMainThread)
envoy_dynamic_module_callback_cluster_add_hosts(cluster, hosts, count)
envoy_dynamic_module_callback_cluster_remove_hosts(cluster, hosts, count)
envoy_dynamic_module_callback_cluster_update_host_health(cluster, host, health)
envoy_dynamic_module_callback_cluster_pre_init_complete(cluster)
```

## Goroutine lifecycle

All background goroutines (TTL sweep, health check, registry subscription) use
`up.Group`:

```go
g := up.NewGroup()
g.AddGoroutine(func(ctx context.Context) {
    // runs until ctx.Done
})
g.Start()
// g.Stop() in cluster Close()
```

Async per-request goroutines in `ChooseHost` derive context from `g.Ctx()`:

```go
reqCtx, cancel := context.WithCancel(cluster.g.Ctx())
// pass cancel as AsyncHandle; CancelHostSelection calls cancel()
```

`AddHosts`/`RemoveHosts`/`UpdateHostHealth` must run on the cluster's main
thread — use `cluster.PostToMainThread(fn)`.

## Implementation checklist

- [ ] `down/down.go` — interfaces: `ClusterConfigFactory`, `ClusterFactory`, `ClusterLB`, `ClusterLBContext`, `LBPolicyConfigFactory`, `LBPolicy`, `LBContext`, `HostPtr`, `AsyncHandle`, `HostHealth`, `HostStat`
- [ ] `down/down.go` — registries: `RegisterCluster`, `GetClusterFactory`, `RegisterLBPolicy`, `GetLBPolicyFactory`
- [ ] `down/abi_impl/cluster.go` — wrapper structs, handle types, all `//export` functions for Cluster Extension
- [ ] `down/abi_impl/lb_policy.go` — wrapper structs, handle types, all `//export` functions for LB Policy
- [ ] `up/cluster.go` — `RegisterCluster(name, g *Group, f ClusterConfigFactory)` re-export
- [ ] `up/lb.go` — `RegisterLBPolicy(name, f LBPolicyConfigFactory)` re-export

## Rules

- Never put CGO in `down/down.go` or `up/` — only `down/abi_impl/` touches C
- Stack-allocate context handles (like `dymAccessLoggerHandle`) — they must not escape the callback
- Always `runtime.KeepAlive` Go strings passed to C via `stringToModuleBuffer`
- Register functions must panic on duplicate names (same as `RegisterAccessLoggerConfigFactory`)
- Use `manager[T]` for all Envoy↔Go pointer round-trips — never cast Go pointers to C directly
