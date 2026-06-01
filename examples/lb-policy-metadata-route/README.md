# lb-policy-metadata-route

This example demonstrates an LB Policy that routes traffic based on endpoint filter metadata. The policy reads a request header specifying a required capability and selects the first healthy host that has matching metadata.

## What It Shows

- LB Policy extension point
- Reading endpoint (host) metadata from Envoy
- Capability-based routing: match request requirements to host capabilities
- Fallback behavior when no match is found

## Scenario

Your upstream hosts have different capabilities (e.g., `standard`, `premium`, `experimental`). Clients specify which capability they need via a request header. The policy matches the request to a host with that capability.

```
Hosts:
  host[0]: filter_metadata["envoy.lb"]["capability"] = "standard"
  host[1]: filter_metadata["envoy.lb"]["capability"] = "premium"

Request 1: x-required-capability: premium
  └─ Scan hosts for matching metadata
  └─ Found: host[1]
  └─ Select host[1]

Request 2: x-required-capability: standard
  └─ Found: host[0]
  └─ Select host[0]

Request 3: (no header)
  └─ Fall back to host[0]
```

## Build and Test

```bash
# Build
make -C examples/lb-policy-metadata-route build

# Unit tests
make -C examples/lb-policy-metadata-route test

# End-to-end tests (requires Envoy binary)
make -C examples/lb-policy-metadata-route e2e
```

## How It Works

### Selection Algorithm

```go
func (p *metadataRoutePolicy) ChooseHost(lb up.LBHandle, ctx up.LBContext, priority *uint32, index *uint32) bool {
    n := lb.HealthyHostCount(0)
    if n == 0 {
        return false
    }
    *priority = 0
    required, ok := ctx.GetHeader("x-required-capability")
    if !ok || required == "" {
        *index = 0
        return true
    }
    for i := 0; i < n; i++ {
        cap, ok := lb.HostMetadataString(0, i, "envoy.lb", "capability")
        if ok && cap == required {
            *index = uint32(i)
            return true
        }
    }
    // no match — fall back to first
    *index = 0
    return true
}
```

1. Get healthy host count
2. Read `x-required-capability` header (if absent, select index 0)
3. Scan all healthy hosts for matching metadata:
   - Namespace: `"envoy.lb"`
   - Key: `"capability"`
4. Return the first host with matching capability
5. If no match, fall back to index 0

### Envoy Configuration

Hosts expose metadata via cluster config:

```yaml
load_assignment:
  cluster_name: backend
  endpoints:
    - lb_endpoints:
        - endpoint:
            address:
              socket_address:
                address: 127.0.0.1
                port_value: 8080
          metadata:
            filter_metadata:
              envoy.lb:
                capability: "standard"
        - endpoint:
            address:
              socket_address:
                address: 127.0.0.1
                port_value: 8081
          metadata:
            filter_metadata:
              envoy.lb:
                capability: "premium"
```

## Key Files

- `lb_policy_metadata_route.go` — Policy implementation with metadata reading
- `lb_policy_metadata_route_test.go` — Unit tests
- `cmd/main.go` — Shared library entry point
- `e2e/e2e_test.go` — End-to-end tests with real Envoy
- `Makefile` — Build and test targets

## Use Cases

- **Tiered backends**: Route requests to standard or premium hosts
- **Feature flags**: Route to hosts with experimental features enabled
- **Hardware variants**: Route to GPUs, TPUs, or CPU-only hosts based on requirement
- **Geographic regions**: Route to hosts in preferred regions

## Test Coverage

**Unit tests** cover:
- Config parsing (empty config is valid)
- Policy creation

**E2E tests** verify:
- Requests without header fall back to host[0]
- Requests with capability header select matching host
- Multiple hosts with different capabilities are available
- No-match requests fall back to host[0]
- Unknown capabilities fall back to host[0]
