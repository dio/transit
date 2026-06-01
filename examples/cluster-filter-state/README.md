# cluster-filter-state

This example demonstrates how an HTTP filter and a Cluster Extension work together via filter state to enable dynamic host selection based on request headers.

## What It Shows

- **HTTP filter** (`filter-state-writer`) reads the `x-target-host` request header and stores it as filter state under the key `transit.target_host`
- **Cluster Extension** (`filter-state-router`) reads that filter state in `ChooseHost` and selects a matching upstream host
- Falls back to the first healthy host when no match is found or the header is absent

## High-Level Flow

```
Client request with header: x-target-host: 192.168.1.5
     ↓
HTTP Filter (filter-state-writer)
  └─ Reads x-target-host header
  └─ Stores as filter state: transit.target_host = "192.168.1.5"
     ↓
Cluster Extension (filter-state-router)
  └─ ChooseHost reads filter state
  └─ Scans config hosts for matching address
  └─ Returns matching host or falls back to index 0
     ↓
Upstream host: 192.168.1.5
```

## Configuration

The Cluster Extension expects a JSON config with a list of hosts:

```json
{
  "hosts": [
    {"address": "127.0.0.1:8080", "weight": 1},
    {"address": "127.0.0.1:8081", "weight": 1},
    {"address": "127.0.0.1:8082", "weight": 1}
  ]
}
```

## Build and Test

```bash
# Build
make -C examples/cluster-filter-state build

# Unit tests
make -C examples/cluster-filter-state test

# End-to-end tests (requires Envoy binary)
make -C examples/cluster-filter-state e2e
```

## How It Works

### HTTP Filter: `filter-state-writer`

Reads the `x-target-host` header and stores it as filter state:

```go
func handler(w *up.Writer, r *up.Request) {
    target := r.Header("x-target-host")
    if target != "" {
        w.SetFilterState("transit.target_host", target)
    }
}
```

### Cluster Extension: `filter-state-router`

Implements the Cluster Extension interface to choose hosts based on filter state:

1. **Init**: Parses config and adds hosts
2. **ChooseHost**: Reads filter state and matches against known hosts
3. Returns first healthy host when no match or no header

### Request Flow

1. Client sends request with `x-target-host: 192.168.1.5`
2. HTTP filter stores `"192.168.1.5"` in filter state
3. Cluster Extension's `ChooseHost` reads filter state
4. Scans configured hosts for matching address
5. Returns the matching host pointer to Envoy
6. Envoy routes the request to that host

When no match is found or header is absent, the extension returns the first healthy host (index 0).

## Key Files

- `cluster_filter_state.go` — HTTP filter and Cluster Extension implementation
- `cluster_filter_state_test.go` — Unit tests (config parsing, host matching)
- `cmd/main.go` — Shared library entry point
- `e2e/e2e_test.go` — End-to-end tests with real Envoy
- `Makefile` — Build and test targets

## Test Coverage

**Unit tests** cover:
- Config parsing (valid/invalid JSON)
- Host list validation
- Filter state writing

**E2E tests** verify:
- Requests with `x-target-host: A` reach host A
- Requests with `x-target-host: B` reach host B
- Requests without the header fall back to the first host
- Invalid host targets fall back to the first host
