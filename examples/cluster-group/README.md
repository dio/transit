# cluster-group

This example demonstrates how to use `up.BaseCluster`, `up.ClusterGroup`, and `up.RunRetry` together to build a Cluster Extension that discovers upstream hosts dynamically from an external HTTP endpoint.

## What It Shows

- **BaseCluster** eliminates lifecycle boilerplate (Init, DrainStarted, Shutdown, Close)
- **ClusterGroup** manages background goroutines with proper lifecycle
- **RunRetry** automatically retries failed operations with exponential backoff
- Cold-start fetch on `ServerInitialized` before Envoy workers accept traffic
- Background refresh at configurable intervals
- Round-robin load balancing across discovered hosts

## Scenario

You have a service discovery endpoint that returns a JSON list of healthy host addresses:

```json
{
  "hosts": ["127.0.0.1:8080", "127.0.0.1:8081", "127.0.0.1:8082"]
}
```

The Cluster Extension discovers these hosts on startup and periodically refreshes the list without requiring Envoy restarts.

## Configuration

```json
{
  "discovery_url": "http://127.0.0.1:9000/hosts",
  "refresh_ms": 5000
}
```

- `discovery_url`: HTTP endpoint returning `{"hosts": [...]}` JSON
- `refresh_ms`: Poll interval in milliseconds (default: 5000ms)

## Build and Test

```bash
# Build
make -C examples/cluster-group build

# Unit tests
make -C examples/cluster-group test

# End-to-end tests (requires Envoy binary)
make -C examples/cluster-group e2e
```

## How It Works

### Lifecycle

1. **Create**: Parse config (discovery URL and refresh interval)
2. **Init**: Override `PreInitComplete()` to start with empty host list
3. **ServerInitialized**:
   - Synchronous cold-start fetch before Envoy workers start
   - Spawn background `ClusterGroup` for continuous refresh
   - `RunRetry` ensures transient errors don't stop discovery
4. **Shutdown**: Stop background goroutines before cluster teardown
5. **Close**: Cleanup (inherited from BaseCluster)

### Cold-Start Fetch

On startup, the extension fetches hosts synchronously before Envoy begins accepting traffic:

```go
if hosts, err := fetchDiscovery(url); err != nil {
    log.Printf("cold-start failed: %v", err)
} else {
    c.applyHostsDirect(hosts)
}
```

### Background Refresh

A background goroutine polls the discovery endpoint at the configured interval:

```go
c.bg.Go(func(ctx context.Context) {
    up.RunRetry(ctx, "discovery-poll", func(ctx context.Context) error {
        ticker := time.NewTicker(refreshInterval)
        defer ticker.Stop()
        for {
            select {
            case <-ctx.Done():
                return ctx.Err()
            case <-ticker.C:
                hosts, err := fetchDiscovery(url)
                if err != nil {
                    return err // RunRetry logs and retries
                }
                c.scheduleApplyHosts(hosts)
            }
        }
    })
})
c.bg.Start()
```

### Host Application

- **Direct** (from main thread): `applyHostsDirect()` adds/removes hosts without scheduling
- **Scheduled** (from goroutine): `scheduleApplyHosts()` dispatches via `ClusterHandle.Schedule()` to ensure thread safety

Updates add new hosts and remove hosts no longer in the discovery list, keeping the cluster current.

### Load Balancing

Simple round-robin across healthy hosts using an atomic counter.

## Key Files

- `cluster.go` — Cluster Extension implementation with discovery
- `cluster_test.go` — Unit tests (config parsing, discovery)
- `cmd/main.go` — Shared library entry point
- `e2e/e2e_test.go` — End-to-end tests with real Envoy
- `Makefile` — Build and test targets

## Comparison with `cluster` Example

This example differs from the basic `cluster` example in two ways:

| Aspect | cluster | cluster-group |
|--------|---------|---------------|
| Host discovery | Static JSON config | Dynamic HTTP discovery |
| Lifecycle boilerplate | Full (Init, DrainStarted, Shutdown, Close) | Minimal (embedded BaseCluster) |
| Background work | None | ClusterGroup with RunRetry |
| Host list updates | Single initial list | Refreshed on interval |

## Test Coverage

**Unit tests** cover:
- Config parsing (valid/invalid discovery URLs)
- Cold-start fetch and apply
- Background refresh with host list changes

**E2E tests** verify:
- Initial hosts are discovered on startup
- Hosts are updated when discovery endpoint changes
- Request load-balances across discovered hosts
- Transient discovery failures don't stop polling (RunRetry)
