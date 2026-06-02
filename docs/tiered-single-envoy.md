# tiered-single-envoy — Design Note

Status: doc only. Implementation tracked separately.

## What this is for

`integrations/tiered-router-eg` proves L1 → L2 routing in Kubernetes with
Envoy Gateway orchestrating two physical proxy deployments. That setup is
correct for production but expensive to iterate on: k3d cluster, two
`EnvoyProxy` CRs, `EnvoyPatchPolicy` patches, image builds.

This document describes the same L1 → L2 contract verified inside a single
Envoy process with multiple static listeners. No cluster, no Gateway API, no
k8s. L2 addresses are embedded directly in L1's shard config. The result is
a reproducible local scratchpad for testing module interaction before changes
graduate to an integration.

## Architecture

```
client (test)
    │
    │ HTTP  :10000  (L1 listener)
    ▼
┌──────────────────────────────────────────────┐
│  Envoy (single process)                      │
│                                              │
│  L1 listener :10000                          │
│    filter: cluster-shard-router              │
│    cluster: l1 (CLUSTER_PROVIDED)            │
│      shard a → 127.0.0.1:10001              │
│      shard b → 127.0.0.1:10002              │
│                                              │
│  L2-a listener :10001                        │
│    filter: cluster-router                    │
│    cluster: l2-a (CLUSTER_PROVIDED)          │
│      gpt-fast → 127.0.0.1:18081             │
│      gpt-slow  → 127.0.0.1:18081            │
│                                              │
│  L2-b listener :10002                        │
│    filter: cluster-router                    │
│    cluster: l2-b (CLUSTER_PROVIDED)          │
│      claude-safe → 127.0.0.1:18082          │
│      claude-fast → 127.0.0.1:18082          │
│                                              │
│  Admin :9901                                 │
└──────────────────────────────────────────────┘
    │                        │
    ▼ :18081                 ▼ :18082
mock-backend-a          mock-backend-b
(httptest.Server)       (httptest.Server)
```

Traffic path for a request carrying `x-transit-tag: a` and `x-model: gpt-fast`:

1. Hits L1 listener `:10000`.
2. `cluster-shard-router` upstream filter reads the tag, selects shard `a`, writes
   `x-transit-l1-shard: a` and `x-transit-l1-target: 127.0.0.1:10001`.
3. L1 dynamic cluster connects to `127.0.0.1:10001` (L2-a listener, loopback).
4. `cluster-router` upstream filter at L2-a reads `x-model: gpt-fast`, selects
   `127.0.0.1:18081`.
5. Response flows back through the same chain.

## Module wiring

### L1 — `cluster-shard-router`

HTTP filter: `cluster-shard-router-debug` (reads tag, writes shard headers via `SendLocalResponse` on the debug path only; pass-through otherwise).

Upstream filter: `cluster-shard-router-upstream` (writes `x-transit-l1-shard` and `x-transit-l1-target` per the shard decision before the request leaves Envoy).

Cluster type: `envoy.clusters.dynamic_modules` → `cluster-shard-router`.

`cluster_config` JSON (embedded in `cluster_type.typed_config`):

```json
{
  "initial": {
    "version": "local",
    "default_shard": "a",
    "shards": {
      "a": {
        "target": "127.0.0.1:10001",
        "prefixes": ["a"],
        "shard": "a",
        "status": "active"
      },
      "b": {
        "target": "127.0.0.1:10002",
        "prefixes": ["b"],
        "shard": "b",
        "status": "active"
      }
    }
  }
}
```

Tag derivation order (from `deriveTag`): `x-transit-tag` → `x-byok-key-id`
(hashed) → `x-user-key` (hashed) → `x-tenant`. Falls back to `default_shard`.

### L2-a / L2-b — `cluster-router`

Each L2 listener has its own named cluster (`l2-a`, `l2-b`) so their module
instances carry separate model registries. The `cluster_name` field inside
`ClusterConfig` must match across the cluster definition and the HTTP/upstream
filter `filter_name` for the module to find the right instance.

Example `cluster_config` for L2-a:

```json
{
  "initial": {
    "version": "local",
    "models": {
      "gpt-fast": {
        "target": "127.0.0.1:18081",
        "provider": "openai",
        "auth_header": "Bearer mock-token-a"
      },
      "gpt-slow": {
        "target": "127.0.0.1:18081",
        "provider": "openai",
        "auth_header": "Bearer mock-slow-token"
      }
    }
  }
}
```

L2-b carries a disjoint model set (`claude-safe`, `claude-fast`) pointing at
`:18082`.

## Envoy config shape

Four top-level sections in `static_resources`:

```
listeners:
  - name: l1          port 10000   cluster-shard-router filters
  - name: l2-a        port 10001   cluster-router filters (l2-a instance)
  - name: l2-b        port 10002   cluster-router filters (l2-b instance)

clusters:
  - name: l1          lb_policy CLUSTER_PROVIDED  cluster-shard-router module
  - name: l2-a        lb_policy CLUSTER_PROVIDED  cluster-router module
  - name: l2-b        lb_policy CLUSTER_PROVIDED  cluster-router module

admin:
  port 9901
```

The L1 listener routes `{ prefix: "/" }` to `cluster: l1`. Each L2 listener
routes `{ prefix: "/" }` to its own cluster (`l2-a` / `l2-b`). The loopback
connection from the l1 cluster to `:10001`/`:10002` re-enters Envoy through
the corresponding L2 listener — no explicit static `STATIC` cluster needed for
the L1→L2 hop because the shard-router module owns host selection directly.

## Test program structure

Lives at `examples/tiered-single-envoy/` (not under `integrations/`, no k8s).

```
examples/tiered-single-envoy/
  e2e/
    testdata/
      envoy.tmpl.yaml   (template, ports injected by test)
    e2e_test.go
  cmd/main.go           (optional: manual-run entry point)
```

`e2e_test.go` pattern (matches every other example e2e):

1. Start two `httptest.Server` instances: `backendA` (`:18081` or random) and
   `backendB` (`:18082` or random). Each backend echoes `x-model` and the
   `Authorization` header in the response for assertion.
2. Pick free ports for L1, L2-a, L2-b, admin.
3. Render `envoy.tmpl.yaml` with ports + backend addresses into a temp file.
4. Start Envoy against that config; poll admin `/ready`.
5. Run gate assertions (below).
6. Defer: stop Envoy, stop backends.

## Gate assertions

**Gate 1 — shard routing**

```
POST :10000  x-transit-tag: a  x-model: gpt-fast
→ response body echoes port :18081 (backend-a)
→ response carries x-transit-l1-shard: a
```

```
POST :10000  x-transit-tag: b  x-model: claude-safe
→ response body echoes port :18082 (backend-b)
→ response carries x-transit-l1-shard: b
```

**Gate 2 — L2 model selection**

```
POST :10000  x-transit-tag: a  x-model: gpt-fast
→ Authorization header reaching backend-a == "Bearer mock-token-a"

POST :10000  x-transit-tag: a  x-model: gpt-slow
→ Authorization header reaching backend-a == "Bearer mock-slow-token"
```

**Gate 3 — unknown tag falls back to default shard**

```
POST :10000  (no tag header)  x-model: gpt-fast
→ reaches backend-a  (default_shard = "a")
```

**Gate 4 — unknown model returns non-200**

```
POST :10000  x-transit-tag: a  x-model: does-not-exist
→ non-200 from L2-a cluster-router
```

## Limitations and graduation path

Single-Envoy is intentionally a proof-of-concept vehicle. The production
architecture requires physical separation: L1 and L2 run in distinct pods so
that resource-heavy L2 workloads (large model inference, streaming buffers)
can be scheduled and scaled independently of L1 coordination work.

| Concern | Status |
|---|---|
| Physical L1/L2 resource isolation | **Not provided** — single process shares CPU/memory budget; graduate to integrations for this |
| Independent L2 scaling | Not testable here; requires separate deployments |
| TLS on the L1→L2 hop | Not wired; loopback is plaintext |
| Credential injection at egress | Out of scope; covered by `cluster-router-eg` |
| Runtime config reload (L1 shard add/remove) | Not in scope here; `config_url` path covered separately |
| HTTP/2 between L1 and L2 | Can be added; not required for gate assertions |

Graduate to `integrations/tiered-router-eg` when:
- Physical resource isolation or independent scaling of L1/L2 needs to be
  validated, or
- The L1→L2 TLS hop needs to be tested, or
- Module changes affect cross-process behavior (e.g., header survivability
  across EG translation).
