# Tiered Router Envoy Gateway Integration

This integration is the planned Kubernetes demo for two-stage request routing
with Transit dynamic modules and Envoy Gateway.

The L1 proxy does tenant, cohort, or BYOK-key sharding. The L2 proxies do model
and provider routing inside the selected shard.

The important distinction is that L1 does not decide which model provider to
use. L1 decides where the user's state lives. L2 then applies the normal
cluster-router behavior for that shard.

## Problem This Solves

Dynamic model routing is only one layer of the problem. Real deployments often
also need request affinity for user-adjacent state:

- BYOK key material
- tenant policy
- user profile data
- provider account mapping
- per-cohort rollout state
- quota or rate-limit ownership

Putting all of that into one global LLM router makes the config large and makes
key ownership harder to reason about. This demo splits the concerns:

- **L1 Router**: maps a request to a state shard.
- **L2 Router**: maps the model request to a provider/upstream inside that
  shard.

That lets a deployment keep shard-local BYOK keys and user profile information
near the L2 router that needs them, while keeping the public Gateway API shape
stable.

## Scenario

Requests enter through one public Gateway.

The request may carry one or more identity hints:

- `x-transit-tag`
- `x-tenant`
- `x-user-key`
- `x-byok-key-id`

L1 resolves those hints into a routing tag:

```text
x-transit-tag present  -> use it directly
x-byok-key-id present  -> hash the key id
x-user-key present     -> hash the user key
x-tenant present       -> normalize the tenant id
missing identity       -> default shard
```

The initial shard table is intentionally small:

```json
{
  "version": "bootstrap",
  "tag_routes": {
    "a": "l2-a.default.svc.cluster.local:80",
    "b": "l2-b.default.svc.cluster.local:80",
    "default": "l2-a.default.svc.cluster.local:80"
  }
}
```

L2 A owns shard A state:

```json
{
  "version": "bootstrap",
  "models": {
    "gpt-fast": {
      "target": "upstream-a.default.svc.cluster.local:8080",
      "provider": "openai",
      "auth_header": "Bearer shard-a-openai-token",
      "profile": "profile-a"
    },
    "claude-safe": {
      "target": "upstream-b.default.svc.cluster.local:8080",
      "provider": "anthropic",
      "auth_header": "Bearer shard-a-anthropic-token",
      "profile": "profile-a"
    }
  }
}
```

L2 B owns shard B state:

```json
{
  "version": "bootstrap",
  "models": {
    "gpt-fast": {
      "target": "upstream-c.default.svc.cluster.local:8080",
      "provider": "openai",
      "auth_header": "Bearer shard-b-openai-token",
      "profile": "profile-b"
    },
    "kimi-fast": {
      "target": "upstream-d.default.svc.cluster.local:8080",
      "provider": "moonshot",
      "auth_header": "Bearer shard-b-moonshot-token",
      "profile": "profile-b"
    }
  }
}
```

The same model can resolve differently depending on the user shard:

```text
x-transit-tag: a-demo, x-model: gpt-fast
  -> L1 selects l2-a
  -> L2 A selects upstream-a
  -> upstream sees shard-a provider/profile headers

x-transit-tag: b-demo, x-model: gpt-fast
  -> L1 selects l2-b
  -> L2 B selects upstream-c
  -> upstream sees shard-b provider/profile headers
```

That is the core demo: model selection is still request-aware, but state
placement happens first.

## Architecture

```text
client or demo CLI
  |
  | x-transit-tag / x-tenant / x-user-key / x-byok-key-id
  | x-model
  v
public Gateway API Gateway and HTTPRoute
  |
  v
L1 Envoy Gateway managed Envoy
  |
  | Transit L1 filter derives x-transit-tag
  | Transit L1 Cluster Extension selects l2-a or l2-b
  v
L2 shard Service, such as l2-a or l2-b
  |
  v
shared L2 Envoy Gateway managed Envoy
  |
  | Transit cluster-router selects provider upstream
  | Transit upstream filter injects provider, profile, and auth headers
  v
final upstream
```

The visible response from the demo upstream should identify every routing
decision:

```json
{
  "l1_tag": "a-demo",
  "l1_shard": "a",
  "l1_target": "l2-a",
  "l2_shard": "a",
  "model": "gpt-fast",
  "provider": "openai",
  "profile": "profile-a",
  "upstream": "upstream-a",
  "byok_key_id": "key-a-001",
  "auth": "Bearer shard-a-openai-token"
}
```

The control-plane dump must redact raw bearer tokens. It can show that auth is
configured and can show stable key IDs, but it must not print secret values.

## Components

The integration should create:

- Envoy Gateway, installed by Helm.
- One public `GatewayClass`, `Gateway`, and `HTTPRoute` for L1.
- One `EnvoyProxy` for L1.
- One `EnvoyPatchPolicy` for the L1 generated listener and cluster.
- Two internal L2 routes, `l2-a` and `l2-b`.
- One shared `EnvoyProxy` for L2 in the first cut.
- One `EnvoyPatchPolicy` for each generated L2 backend cluster that needs the
  cluster-router module.
- One demo control-plane Deployment and Service.
- Upstream A, B, C, and D Deployments and Services.

The first cut should use two Envoy Gateway managed proxy deployments:

```text
EnvoyProxy/l1
EnvoyProxy/l2
```

That keeps the topology small while still proving that L1 selects different L2
logical targets. A later isolation cut can split L2 into one `EnvoyProxy` per
shard:

```text
EnvoyProxy/l1
EnvoyProxy/l2-a
EnvoyProxy/l2-b
```

The first-cut resource map should look like this:

```text
transit-system
  Envoy Gateway controller

transit-dataplane
  EnvoyProxy/l1
  Gateway/l1
  HTTPRoute/l1-public
  EnvoyPatchPolicy/l1-cluster-shard-router

  EnvoyProxy/l2
  Gateway/l2
  HTTPRoute/l2-a
  HTTPRoute/l2-b
  Service/l2-a
  Service/l2-b
  EnvoyPatchPolicy/l2-a-cluster-router
  EnvoyPatchPolicy/l2-b-cluster-router

  Deployment/tiered-router-control
  Service/tiered-router-control
  Deployment/upstream-a
  Deployment/upstream-b
  Deployment/upstream-c
  Deployment/upstream-d
```

## Namespace And Deployment Mode

The real deployment should use Envoy Gateway Gateway Namespace Mode. In
standard mode, Envoy Gateway creates data-plane resources in the controller
namespace. Gateway Namespace Mode moves Envoy proxy Deployments, Services, and
ServiceAccounts into each Gateway namespace, while the controller remains in its
own namespace. That matches the desired split:

```text
transit-system
  Envoy Gateway controller

transit-dataplane
  Gateway/l1
  Gateway/l2
  EnvoyProxy/l1
  EnvoyProxy/l2
  Envoy data-plane Deployments and Services
```

Install Envoy Gateway with Gateway Namespace Mode enabled:

```yaml
config:
  envoyGateway:
    provider:
      type: Kubernetes
      kubernetes:
        deploy:
          type: GatewayNamespace
```

The controller also needs permission to manage data-plane resources in
`transit-dataplane`. The Envoy Gateway Helm chart creates the additional RBAC
for Gateway Namespace Mode. For tighter demos, configure Envoy Gateway to watch
only the namespaces we need:

```yaml
envoyGateway:
  provider:
    type: Kubernetes
    kubernetes:
      deploy:
        type: GatewayNamespace
      watch:
        type: Namespaces
        namespaces:
          - transit-dataplane
```

The e2e can still run everything in `default` while bootstrapping. The
integration README and manifests should converge on `transit-system` for the
controller and `transit-dataplane` for Gateway and Envoy data-plane resources.

## L2 Deployment Shape

The first cut uses one shared L2 EnvoyProxy and stable shard Services:

```text
Gateway/l2
  |
  +-- HTTPRoute/l2-a
  |
  +-- HTTPRoute/l2-b
  |
  v
EnvoyProxy/l2
  |
  v
shared L2 Envoy Deployment
```

L1 should not point at an Envoy Gateway generated Service name directly if that
name is unstable. L1 should point at stable Kubernetes Service names owned by
the demo or operator:

```text
l2-a.transit-dataplane.svc.cluster.local:80
l2-b.transit-dataplane.svc.cluster.local:80
```

Those Services map to the L2 Gateway/proxy. In the demo, that can be either:

- alias Services that select the generated L2 Envoy pods, or
- stable Gateway Services if EnvoyProxy configuration lets us control the
  generated service names.

The L1 config then uses those stable service names:

```json
{
  "version": "bootstrap",
  "default_shard": "a",
  "shards": {
    "a": {
      "target": "l2-a.transit-dataplane.svc.cluster.local:80",
      "prefixes": ["a"],
      "shard": "a",
      "status": "active"
    },
    "b": {
      "target": "l2-b.transit-dataplane.svc.cluster.local:80",
      "prefixes": ["b"],
      "shard": "b",
      "status": "active"
    }
  }
}
```

L2 can tell which shard path was selected in two ways:

- L1 injects `x-transit-l1-shard`, `x-transit-tag`, and
  `x-transit-l1-target`.
- L2 routes can also use different hostnames or route metadata for `l2-a` and
  `l2-b` if the Gateway API shape needs it.

For the first implementation, prefer the header path because it keeps L2 shard
identity explicit in requests and in e2e assertions. A later isolation cut can
split L2 into one EnvoyProxy per shard when separate rollout or failure domains
become part of the demo.

The first implementation can keep all demo control-plane APIs in one static Go
binary:

- `tiered-router-demo control`
- `tiered-router-demo upstream`
- `tiered-router-demo request`
- `tiered-router-demo l1 routes`
- `tiered-router-demo l1 routes add-prefix`
- `tiered-router-demo l2 routes`
- `tiered-router-demo l2 models add`
- `tiered-router-demo dump`

## Dynamic Modules

L1 needs its own Transit module because its logic is not model routing:

- derive or normalize a tag from request headers
- select an L2 host from a tag-prefix table
- inject headers that make the L1 decision visible downstream

L1 does not discover L2s from Kubernetes by itself. The operator or demo
control plane publishes the L2 shard table, and L1 consumes that config. The
config API must expose enough state to observe what L1 is currently using:

- active shard-map version
- default shard
- configured shard prefixes
- configured L2 targets
- whether a shard is active or intentionally disabled

The L1 module should live under:

```text
examples/cluster-shard-router
```

L2 should reuse the existing `examples/cluster-router` module where possible.
If the existing L2 response needs more visible shard/profile fields, add them
through config and upstream headers rather than duplicating the router.

The custom Envoy images should carry modules under:

```text
/etc/envoy/dynamic-modules/libcluster-shard-router.so
/etc/envoy/dynamic-modules/libcluster-router.so
```

Keep `ENVOY_DYNAMIC_MODULES_SEARCH_PATH=/etc/envoy/dynamic-modules` on every
generated Envoy pod that uses dynamic modules.

## Envoy Gateway And Patch Shape

Use Gateway API as the stable user-facing API. Envoy Gateway still owns xDS.
Use EnvoyPatchPolicy only to replace the generated backend clusters and insert
the dynamic module filters.

The e2e should not hard-code generated cluster names. It should discover them
from each Envoy admin `/config_dump`, like `cluster-router-eg` does today.

Expected generated names with Envoy Gateway v1.8 and `XDSNameSchemeV2` are
still useful for debugging:

```text
httproute/<namespace>/<route-name>/rule/0
tcp-80
http-80
```

But the test should discover, not assume.

## E2E Assertions

The e2e should prove the two-tier behavior through the public Gateway only.

Bootstrap assertions:

- `x-transit-tag: a-demo` and `x-model: gpt-fast` reaches upstream A.
- `x-transit-tag: b-demo` and `x-model: gpt-fast` reaches upstream C.
- `x-transit-tag: a-demo` sets `x-transit-l1-shard: a`.
- `x-transit-tag: b-demo` sets `x-transit-l1-shard: b`.
- L2 A injects shard A profile/provider/auth metadata.
- L2 B injects shard B profile/provider/auth metadata.

Derived tag assertions:

- request with `x-user-key` and no `x-transit-tag` derives a stable tag.
- request with `x-byok-key-id` and no `x-transit-tag` derives a stable tag.
- the selected shard is deterministic across repeated requests.

Dynamic config assertions:

- adding a new L1 prefix route can move new traffic to another L2 shard without
  changing Gateway API.
- adding a new model to L2 B makes that model routable only for traffic that L1
  sends to shard B.
- config dumps show model names, key IDs, shard IDs, and versions.
- config dumps do not leak raw bearer tokens.

Failure assertions:

- missing model returns a controlled 404 or 503 response from L2.
- unknown tag falls back to the configured default shard.
- missing identity uses the default shard and marks the tag source as default.

## Local Demo Flow

The local demo should follow the same ergonomics as `cluster-router-eg`:

```sh
KEEP_CLUSTER=1 make -C integrations/tiered-router-eg e2e
make -C integrations/tiered-router-eg cli
```

Then forward the control plane and public Gateway, and drive the scenario with
the CLI instead of raw curl:

```sh
./integrations/tiered-router-eg/dist/tiered-router-demo request \
  --tag a-demo \
  --model gpt-fast \
  --gateway-url http://127.0.0.1:19081 \
  --host tiered-router.example.com

./integrations/tiered-router-eg/dist/tiered-router-demo request \
  --tag b-demo \
  --model gpt-fast \
  --gateway-url http://127.0.0.1:19081 \
  --host tiered-router.example.com
```

Expected result:

```text
a-demo + gpt-fast -> l2-a -> upstream-a -> profile-a
b-demo + gpt-fast -> l2-b -> upstream-c -> profile-b
```

For a BYOK-focused demo:

```sh
./integrations/tiered-router-eg/dist/tiered-router-demo request \
  --byok-key-id key-a-001 \
  --model gpt-fast \
  --gateway-url http://127.0.0.1:19081 \
  --host tiered-router.example.com

./integrations/tiered-router-eg/dist/tiered-router-demo request \
  --byok-key-id key-b-001 \
  --model gpt-fast \
  --gateway-url http://127.0.0.1:19081 \
  --host tiered-router.example.com
```

The output should prove that each key ID lands on its owning shard and that the
raw auth secret does not appear in the dump.

## CI Shape

Use the same CI strategy as `cluster-router-eg`:

- build Linux `.so` artifacts locally
- build minimal Envoy images
- build a static demo control-plane binary with `CGO_ENABLED=0`
- publish short-lived images to ttl.sh in CI
- run the k3d suite with `K3D_SKIP_IMAGE_IMPORT=1`
- keep local demos on direct `k3d image import --mode direct`

All e2e runs must use `-count=1`.

## Implementation Phases

1. Write the L1 module under `examples/cluster-shard-router`.
   - derive tags from headers
   - route tag prefixes to L2 hosts
   - inject visible L1 decision headers
   - unit test tag derivation and prefix matching

2. Add the integration skeleton.
   - Makefile
   - Dockerfiles
   - k8s templates
   - demo control-plane binary
   - host-side CLI

3. Bring up L1 only.
   - one L1 Gateway
   - L1 routes directly to echo upstreams
   - prove tag prefix selection

4. Add shared L2 proxy with logical shard entrypoints.
   - reuse `examples/cluster-router`
   - create `Gateway/l2` and shared `EnvoyProxy/l2`
   - create `HTTPRoute/l2-a` and `HTTPRoute/l2-b`
   - create stable `Service/l2-a` and `Service/l2-b`
   - patch each generated L2 backend cluster to use `cluster-router`
   - prove L1 selects the right L2 logical shard service

5. Add shard-local BYOK/profile metadata.
   - each L2 has different auth/profile config for the same model
   - dump output redacts secrets

6. Add CI.
   - separate workflow job
   - ttl.sh image publishing
   - `K3D_SKIP_IMAGE_IMPORT=1`

## Open Questions

- Should L1 derive the shard from `x-byok-key-id` first, then `x-user-key`, then
  `x-tenant`, or should the priority be configurable?
- Should L1 expose a debug endpoint for the active tag table, or should the
  control plane own all dump APIs?
- Should L2 model configs include explicit `byok_key_id`, or should key ID be a
  separate shard-local profile lookup?
- Should the first e2e assert real hashing, or use explicit `x-transit-tag`
  first and add hashing after the topology is stable?

## Recommended First Cut

Start with explicit `x-transit-tag`.

That proves the tiered topology, EnvoyPatchPolicy shape, and L1 Cluster
Extension behavior without mixing in hashing semantics too early. Once the
topology is green, add `x-byok-key-id` and `x-user-key` derivation as a focused
second step.
