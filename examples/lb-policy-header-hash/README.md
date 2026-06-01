# lb-policy-header-hash

This example demonstrates an LB Policy that hashes a request header value to consistently route traffic to the same upstream host. Common use case: sticky sessions based on a session ID header.

## What It Shows

- LB Policy extension point (vs. Cluster Extension)
- Header-based request routing
- FNV hash for consistent distribution
- Simple index-based host selection

## Scenario

You have multiple upstream hosts and want requests with the same session ID to always go to the same host (sticky sessions). The policy hashes the `x-session-id` header to pick a host index.

```
Request: x-session-id: abc123
  └─ Hash("abc123") % host_count = 1
  └─ Select host[1]

Request: x-session-id: abc123
  └─ Hash("abc123") % host_count = 1
  └─ Select same host[1] (sticky)
```

## Build and Test

```bash
# Build
make -C examples/lb-policy-header-hash build

# Unit tests
make -C examples/lb-policy-header-hash test

# End-to-end tests (requires Envoy binary)
make -C examples/lb-policy-header-hash e2e
```

## How It Works

### Selection Algorithm

```go
func (p *headerHashPolicy) ChooseHost(lb up.LBHandle, ctx up.LBContext, priority *uint32, index *uint32) bool {
    n := lb.HealthyHostCount(0)
    if n == 0 {
        return false
    }
    *priority = 0
    val, ok := ctx.GetHeader("x-session-id")
    if !ok || val == "" {
        *index = 0
        return true
    }
    h := fnv.New32a()
    h.Write([]byte(val))
    *index = uint32(int(h.Sum32()) % n)
    return true
}
```

1. Get healthy host count
2. Read `x-session-id` header (if absent, select index 0)
3. Hash the header value using FNV-1a
4. Modulo by healthy host count to get index
5. Return priority=0 and the computed index

### Flow

1. **Create**: No config needed
2. **NewLBPolicy**: Create per-worker policy instance
3. **ChooseHost**: Read header, hash it, return index
4. Envoy uses the index to select a host

## Key Files

- `lb_policy_header_hash.go` — Policy implementation with hashing
- `lb_policy_header_hash_test.go` — Unit tests
- `cmd/main.go` — Shared library entry point
- `e2e/e2e_test.go` — End-to-end tests with real Envoy
- `Makefile` — Build and test targets

## Sticky Session Behavior

- **With header**: Same `x-session-id` value always hashes to the same host
- **Without header**: Falls back to host[0]
- **Host changes**: Scaling up/down the backend changes which index a session maps to (expected hash redistribution)

## Test Coverage

**Unit tests** cover:
- Config parsing (empty config is valid)
- Policy creation

**E2E tests** verify:
- Requests with the same `x-session-id` go to the same host
- Requests with different session IDs distribute across hosts
- Requests without the header fall back to host[0]
- Multiple hosts are available and receive traffic
