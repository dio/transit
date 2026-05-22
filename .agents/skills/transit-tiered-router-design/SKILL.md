---
name: transit-tiered-router-design
description: Design Transit tiered router examples and integrations with an L1 shard router, L2 model/provider routers, Envoy Gateway resources, stable shard Services, BYOK/profile placement, and operator-owned physical shard deployment.
---

# Transit Tiered Router Design

Use this skill when planning or changing a Transit L1/L2 router scenario,
especially `examples/cluster-shard-router` or
`integrations/tiered-router-eg`.

## Mental model

L1 answers: which shard owns this request's state?

L2 answers: which provider upstream handles this model inside that shard?

Keep those decisions separate. L1 should not know provider routing rules. L2
should not decide tenant, BYOK, user profile, or cohort placement.

## Stable shard targets

L1 config should target stable shard DNS names:

```text
l2-a.<namespace>.svc.cluster.local:80
l2-b.<namespace>.svc.cluster.local:80
```

Use `Service/l2-a` and `Service/l2-b` with one physical L2 EnvoyProxy per
shard. The service names carry shard identity for L1 config, dumps, demos, and
operator reconciliation. Separate L2 processes also avoid process-global
cluster-router config collisions when shard A and shard B return different
model maps.

Each shard Service should select its own EnvoyProxy pod label:

```yaml
metadata:
  labels:
    transit.dio/proxy: l2-a
    transit.dio/shard: a
spec:
  selector:
    transit.dio/proxy: l2-a
```

Use separate `EnvoyProxy/l2-a` and `EnvoyProxy/l2-b`, then select per-shard pod
labels such as `transit.dio/proxy: l2-a` and `transit.dio/proxy: l2-b`. Keep
the stable service names so L1 config does not change.

## Gateway attachment boundary

Make Gateway route attachment explicit in integration demos. Label namespaces
that may attach routes, for example:

```yaml
metadata:
  labels:
    transit.dio/gateway-routes: "true"
```

Then set each L1 and L2 listener to:

```yaml
allowedRoutes:
  namespaces:
    from: Selector
    selector:
      matchLabels:
        transit.dio/gateway-routes: "true"
  kinds:
  - group: gateway.networking.k8s.io
    kind: HTTPRoute
```

This is not only a security detail. It documents the operator boundary: only
namespaces intentionally marked for the dataplane may attach routes to the demo
Gateways.

## Operator boundary

Transit config describes logical routes. It should not create Kubernetes
resources.

When a new shard is added, a controller or operator should reconcile the
physical L2 resources first:

- `Gateway` or listener attachment for the shard entrypoint
- `HTTPRoute` for the shard service name or hostname
- `EnvoyProxy` if the shard needs its own rollout or failure domain
- stable `Service` names that L1 can target
- `EnvoyPatchPolicy` for generated backend clusters

After physical resources exist, publish the logical shard entry to L1 config.

## BYOK and provider egress

BYOK key IDs, profiles, tenant policy, and provider account mapping are
shard-local state. L1 places the request near that state; L2 injects the
provider, profile, BYOK key ID, and auth headers for the selected model.

Real provider targets are usually HTTPS internet endpoints. Do not mix local
plaintext upstreams and HTTPS provider endpoints in one Cluster Extension
cluster. Use a separate TLS-enabled provider route or cluster and let Envoy
originate TLS with `UpstreamTlsContext`. Keep SNI, authority, and optional H2
protocol metadata in the model/provider config.

## Testing priority

Prove the topology incrementally:

1. L1-only: `a-demo` reaches upstream A and `b-demo` reaches upstream C through
   the public Gateway.
2. Physical L2: L1 sends `a` traffic to `Service/l2-a` and `b` traffic to
   `Service/l2-b`, where each service selects a separate L2 EnvoyProxy.
3. Shard-local config: the same model resolves differently in shard A and B.
4. Dynamic config: add a shard or model without changing Gateway API.
5. Provider egress: route one model to a TLS internet endpoint.
