# cluster-least-conn

This example demonstrates a Cluster Extension that implements a least-connections load balancer. Unlike the `lb-policy` example which chooses an index, this uses the Cluster Extension point to own the host set and implement custom load balancing logic.

## What It Shows

- **Cluster Extension** (vs. LB Policy) for more control over host management
- Parsing static host config from JSON
- Least-connections load balancing: always pick the host with the fewest active requests
- Reading live host statistics via `up.ClusterLBHandle`

## Scenario

You have multiple upstream hosts and want to balance traffic based on active connection count rather than round-robin or random selection. The extension scans all hosts, finds the one with the fewest in-flight requests, and selects it.

## Configuration

```json
{
  "hosts": [
    {"address": "127.0.0.1:8080", "weight": 1},
    {"address": "127.0.0.1:8081", "weight": 1},
    {"address": "127.0.0.1:8082", "weight": 1}
  ]
}
```

Hosts are parsed on startup and configured into Envoy's cluster. The `weight` field is optional.

## Build and Test

```bash
# Build
make -C examples/cluster-least-conn build

# Unit tests
make -C examples/cluster-least-conn test

# End-to-end tests (requires Envoy binary)
make -C examples/cluster-least-conn e2e
```

## How It Works

### Host Selection Algorithm

On every request, the load balancer scans all healthy hosts and returns the one with the fewest active requests:

```go
func (lb *leastConnClusterLB) ChooseHost(h up.ClusterLBHandle, _ up.ClusterLBContext) (up.HostPtr, *up.ClusterLBCompletion) {
    total := h.HostCount(0)
    if total == 0 {
        return nil, nil
    }

    minIdx := 0
    minActive := h.HostStat(0, 0, up.HostStatRqActive)
    for i := 1; i < total; i++ {
        active := h.HostStat(0, i, up.HostStatRqActive)
        if active < minActive {
            minActive = active
            minIdx = i
        }
    }
    return h.Host(0, minIdx), nil
}
```

### Flow

1. **Init**: Parse config hosts and add them to Envoy's cluster
2. **NewClusterLB**: Create per-worker load balancer instance
3. **ChooseHost**: Scan hosts for the one with fewest active requests
4. Return host pointer to Envoy for upstream selection

### Key Differences from LB Policy

| Feature | LB Policy (`lb-policy`) | Cluster Extension (`cluster-least-conn`) |
|---------|------------------------|------------------------------------------|
| Host discovery | Envoy owns host set | Extension owns host set |
| Config parsing | Extension parses | Extension parses |
| Return value | Priority + index | Host pointer |
| Complexity | Simple (index-based) | Can access more host data |
| Use case | Lightweight policies | Complex logic needing full host control |

This example uses Cluster Extension because least-connections requires reading per-host active connection stats, which is more ergonomic with direct host pointers than with index-based lookups.

## Key Files

- `cluster_least_conn.go` — Cluster Extension with least-connections logic
- `cluster_least_conn_test.go` — Unit tests (config parsing)
- `cmd/main.go` — Shared library entry point
- `e2e/e2e_test.go` — End-to-end tests with real Envoy
- `Makefile` — Build and test targets

## Test Coverage

**Unit tests** cover:
- Config parsing (valid/invalid JSON, empty hosts)
- Host address validation

**E2E tests** verify:
- Requests are distributed across configured hosts
- Host with fewest active connections gets more traffic
- Missing hosts return no-host error (503)
- Weight field is parsed correctly
