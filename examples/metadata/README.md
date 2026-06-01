# metadata

This example demonstrates metadata-driven routing by combining an HTTP filter, static route metadata, and a Cluster Extension. It shows how to read route metadata, pass it via filter state, and use it for host selection.

## What It Shows

- **HTTP filter** reads static route metadata (`tier`)
- **Filter state** passes data from filters to Cluster Extensions
- **Dynamic metadata** (written to access logs)
- **Cluster Extension** uses filter state to select between tiered hosts (standard vs. premium)

## Scenario

Your routes have a tier level (standard or premium) configured in Envoy. The HTTP filter reads this tier, stores it as filter state, and the Cluster Extension uses it to route to the appropriate backend tier.

```
Envoy Route Config:
  route[0]: /api/standard
    metadata:
      example.routing:
        tier: "standard"
  route[1]: /api/premium
    metadata:
      example.routing:
        tier: "premium"

Cluster Config:
  hosts:
    host[0]: 127.0.0.1:8080  (standard tier)
    host[1]: 127.0.0.1:8081  (premium tier)

Request flow:
  client → /api/premium
    ↓
  HTTP filter reads route metadata
    ↓
  filter-state = "premium"
  dynamic-metadata = "premium" (for access logs)
    ↓
  Cluster Extension reads filter state
    ↓
  resolveTierIndex("premium") = 1
    ↓
  Select host[1] (premium)
```

## Configuration

### Route Metadata (in Envoy config)

```yaml
routes:
  - name: "premium-route"
    match:
      path: "/api/premium"
    route:
      cluster: backend
    metadata:
      filter_metadata:
        example.routing:
          tier: "premium"

  - name: "standard-route"
    match:
      path: "/api/standard"
    route:
      cluster: backend
    metadata:
      filter_metadata:
        example.routing:
          tier: "standard"
```

### Cluster Config

```json
{
  "hosts": [
    {"address": "127.0.0.1:8080"},  # standard tier (index 0)
    {"address": "127.0.0.1:8081"}   # premium tier (index 1)
  ]
}
```

At least two hosts are required (standard and premium).

## Build and Test

```bash
# Build
make -C examples/metadata build

# Unit tests
make -C examples/metadata test

# End-to-end tests (requires Envoy binary)
make -C examples/metadata e2e
```

## How It Works

### HTTP Filter: `metadata-router`

The filter reads route metadata and writes it to filter state and dynamic metadata:

```go
func handle(w *up.Writer, _ *up.Request) {
    tier := ""
    if buf, ok := w.GetMetadataString(up.MetadataSourceRoute, "example.routing", "tier"); ok {
        tier = buf.String()
    }

    if tier == "" {
        tier = "standard"  // default
    }

    // Write to filter state for Cluster Extension
    w.SetFilterState("meta.tier", tier)

    // Write to dynamic metadata for access logs
    w.SetMetadata("example.routing", "tier", tier)

    w.Log(up.LogInfo, "metadata-router: tier=%s", tier)
}
```

Steps:
1. Read `example.routing:tier` from route metadata
2. Default to `"standard"` if not present
3. Store as filter state under `"meta.tier"`
4. Write to dynamic metadata for logging
5. Log the decision

### Cluster Extension: `metadata-hosts`

The extension reads filter state and selects a host index:

```go
func (lb *metadataLB) ChooseHost(h up.ClusterLBHandle, ctx up.ClusterLBContext) (up.HostPtr, *up.ClusterLBCompletion) {
    idx := resolveTierIndex(tierFromCtx(ctx))
    count := h.HostCount(0)
    if count == 0 {
        return nil, nil
    }
    if idx >= count {
        idx = 0
    }
    return h.Host(0, idx), nil
}

func tierFromCtx(ctx up.ClusterLBContext) string {
    if tier, ok := ctx.GetFilterState("meta.tier"); ok {
        return tier
    }
    return ""
}

func resolveTierIndex(tier string) int {
    if tier == "premium" {
        return 1
    }
    return 0  // standard or unknown
}
```

Steps:
1. Read `"meta.tier"` from filter state
2. Map tier to host index: `"premium"` → 1, else → 0
3. Return host at that index
4. Fallback to index 0 if index out of bounds

### Data Flow

```
Envoy Route Config
    ↓ (static metadata)
HTTP Filter (metadata-router)
    ↓ (reads route metadata)
    ├─ SetFilterState("meta.tier", tier)  (for Cluster Extension)
    └─ SetMetadata("example.routing", "tier", tier)  (for access logs)
    ↓
Cluster Extension (metadata-hosts LB)
    ├─ Reads filter state
    └─ Selects host index
    ↓
Upstream Host
```

## Key Files

- `metadata.go` — HTTP filter implementation
- `cluster.go` — Cluster Extension implementation
- `metadata_test.go` — Unit tests (filter logic)
- `cmd/main.go` — Shared library entry point
- `e2e/e2e_test.go` — End-to-end tests with real Envoy
- `Makefile` — Build and test targets

## Metadata Namespaces

The example uses:

| Source | Namespace | Key | Purpose | Read By |
|--------|-----------|-----|---------|---------|
| **Route metadata** | `example.routing` | `tier` | Static route config | HTTP filter |
| **Filter state** | - | `meta.tier` | Request-scoped state | Cluster Extension |
| **Dynamic metadata** | `example.routing` | `tier` | Access log attributes | Envoy access logger |

## Access Logs

With dynamic metadata written, Envoy access logs can include the tier:

```yaml
access_log:
  - name: envoy.access_loggers.stdout
    typed_config:
      "@type": type.googleapis.com/envoy.extensions.access_loggers.stream.v3.StdoutAccessLog
      format: |
        [%START_TIME%] "%REQ(:METHOD)% %REQ(X-ENVOY-ORIGINAL-PATH?:PATH)% %PROTOCOL%"
        %RESPONSE_CODE% %BYTES_RECEIVED% %BYTES_SENT%
        tier=%DYNAMIC_METADATA(example.routing:tier)%
```

## Test Coverage

**Unit tests** cover:
- Metadata reading from routes
- Filter state writing
- Fallback to "standard" when metadata absent

**E2E tests** verify:
- Requests to standard routes reach standard host
- Requests to premium routes reach premium host
- Missing metadata defaults to standard host
- Dynamic metadata appears in access logs
- Both hosts receive traffic appropriately
