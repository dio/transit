# Cluster Shard Router Example

This example shows the L1 side of a tiered router. It uses Envoy's
cluster-provided load balancing path from Go, but the routing decision is
shard placement instead of model/provider selection.

`cluster-router` answers this question:

```text
Which provider upstream should handle this model?
```

`cluster-shard-router` answers this question:

```text
Which L2 shard should own this user's state for this request?
```

The intended tiered deployment is:

```text
client
  |
  | x-transit-tag / x-byok-key-id / x-user-key / x-tenant
  | x-model
  v
cluster-shard-router
  |
  | selects l2-a or l2-b
  v
cluster-router
  |
  | selects provider upstream inside that shard
  v
provider upstream
```

## Problem This Solves

BYOK keys, user profiles, tenant policy, quota ownership, and rollout cohorts
often need stable placement before model routing happens. Keeping all of that
state in one global model router makes the config large and weakens shard
ownership.

This example keeps L1 focused on shard selection:

- derive a tag from request headers
- match the tag against configured shard prefixes
- return the L2 shard host directly from Go
- inject decision headers so downstream services can observe the selected shard

The operator or control plane is responsible for publishing the shard table.
The module does not discover L2s from Kubernetes by itself.

## Request Headers

The router derives a tag in this priority order:

```text
x-transit-tag present  -> use it directly
x-byok-key-id present  -> sha256(key id), hex encoded
x-user-key present     -> sha256(user key), hex encoded
x-tenant present       -> normalized tenant id
missing identity       -> default shard
```

The first implementation should use explicit `x-transit-tag` in demos because
it makes the topology easy to explain. Hash-based routing is still covered by
unit tests and can be used by the tiered integration when needed.

## Config

The cluster config can embed initial shard state and optionally point at a live
config URL:

```json
{
  "config_url": "http://127.0.0.1:18080/shards.json",
  "refresh_millis": 200,
  "timeout_millis": 500,
  "initial": {
    "version": "bootstrap",
    "default_shard": "a",
    "shards": {
      "a": {
        "target": "localhost:18081",
        "prefixes": ["a"],
        "shard": "a",
        "status": "active"
      },
      "b": {
        "target": "localhost:18082",
        "prefixes": ["b"],
        "shard": "b",
        "status": "active"
      }
    }
  }
}
```

Runtime config has the same shape as `initial`:

```json
{
  "version": "v2",
  "default_shard": "a",
  "shards": {
    "a": {
      "target": "l2-a.default.svc.cluster.local:80",
      "prefixes": ["a"],
      "shard": "a",
      "status": "active"
    },
    "b": {
      "target": "l2-b.default.svc.cluster.local:80",
      "prefixes": ["b"],
      "shard": "b",
      "status": "active"
    }
  }
}
```

Targets are resolved by Go before they are added to Envoy. Invalid updates do
not replace the active snapshot.

## Decision Headers

The upstream filter injects these headers before Envoy forwards the request to
the selected L2 shard:

```text
x-transit-tag
x-transit-tag-source
x-transit-l1-shard
x-transit-l1-target
x-cluster-shard-router-version
```

These headers make the decision observable by the L2 router, demo upstreams,
and e2e assertions.

## What The Example Proves

The unit tests cover:

- config parsing defaults
- target resolution
- tag priority
- BYOK/user-key hashing
- longest-prefix shard matching
- default shard fallback
- snapshot copy behavior

The e2e test covers:

- `x-transit-tag: a-demo` routes to L2 A
- `x-transit-tag: b-demo` routes to L2 B
- `x-tenant: B-Tenant` derives a normalized tenant tag
- unknown tags use the default shard
- the active config can be dumped
- a config refresh can add a new L2 shard without changing Envoy routes

## Run

Run unit tests:

```sh
make -C examples/cluster-shard-router test
```

Run the Envoy e2e:

```sh
make -C examples/cluster-shard-router e2e
```

Build the shared library:

```sh
make -C examples/cluster-shard-router build
```

Run the static local Envoy config:

```sh
make -C examples/cluster-shard-router run
```

For `run`, start simple listeners on `localhost:18081` and `localhost:18082`
first, then send requests to `http://127.0.0.1:10000` with
`x-transit-tag: a-demo` or `x-transit-tag: b-demo`.
