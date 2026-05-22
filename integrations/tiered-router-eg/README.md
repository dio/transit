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

Real LLM providers are internet endpoints and should be reached over TLS. The
local upstreams in this demo are only stand-ins for provider services. We have
proved the first provider-egress shape in `examples/cluster-router`: use a
separate TLS-enabled Envoy cluster for HTTPS provider traffic, keep Transit host
selection unchanged, and let Envoy originate TLS with SNI and certificate
validation.

```json
{
  "scope": "https-provider",
  "timeout_millis": 500,
  "initial": {
    "version": "https-provider",
    "models": {
      "gpt-secure": {
        "target": "provider.local:443",
        "provider": "openai",
        "auth_header": "Bearer provider-token"
      }
    }
  }
}
```

The corresponding Envoy cluster, or EnvoyPatchPolicy in this integration, owns
the TLS transport:

```yaml
transport_socket:
  name: envoy.transport_sockets.tls
  typed_config:
    "@type": type.googleapis.com/envoy.extensions.transport_sockets.tls.v3.UpstreamTlsContext
    sni: provider.local
    common_tls_context:
      validation_context:
        trusted_ca:
          filename: /etc/provider-ca/ca.pem
        match_typed_subject_alt_names:
          - san_type: DNS
            matcher:
              exact: provider.local
```

For a public provider, the same model stays small:

```json
{
  "scope": "https-provider",
  "initial": {
    "version": "internet",
    "models": {
      "httpbin": {
        "target": "httpbin.org:443",
        "provider": "httpbin",
        "auth_header": "Bearer shard-a-httpbin-token"
      }
    }
  }
}
```

The Envoy patch for that provider sets SNI and SAN matching to `httpbin.org`.
Do not put TLS knobs into the Transit host-selection API just to make this
work.

Do not mix plaintext local upstreams and HTTPS internet upstreams in the same
dynamic module cluster. Envoy originates TLS at the upstream cluster level by
adding an `UpstreamTlsContext` transport socket to that cluster. A TLS-enabled
L2 provider cluster should be separate from the local plaintext demo cluster.
That keeps local mock upstreams simple and models real provider egress
correctly.

The proven `cluster-router` shape is:

- one plaintext route and cluster for local shard mock upstreams
- one HTTPS provider route and cluster with an Envoy TLS transport socket
- one `scope` value per cluster config so route snapshots do not collide
- one upstream filter registration per scope so provider headers come from the
  same snapshot the Cluster Extension used for host selection

Transport metadata such as `scheme`, `authority`, `sni`, and `protocol` can
exist in the control plane if it helps generate patches or dumps, but Transit
does not need those fields for ordinary HTTPS egress. The module needs the
provider target and headers. Envoy needs the TLS context.

HTTP/2 should be treated as a later provider-egress step. The first TLS path
now proves ordinary HTTPS with a local provider and generated CA. For the
integration, use the same shape first, then add a public provider smoke test if
network access is acceptable. After that is stable, add an optional HTTP/2 path
by setting ALPN to `h2` in the Envoy TLS context and configuring the cluster
with HTTP/2 protocol options.

## Architecture

```text
client or demo CLI
  |
  | Host: tiered-router.example.com
  | x-transit-tag / x-tenant / x-user-key / x-byok-key-id
  | x-model
  v
Gateway/l1
HTTPRoute/l1-public
  |
  v
EnvoyProxy/l1
Transit cluster-shard-router
  |
  | adds x-transit-l1-shard and x-transit-l1-target
  | selects l2-a or l2-b
  |
  +-------------------------------+
  |                               |
  v                               v
Service/l2-a                  Service/l2-b
  |                               |
  v                               v
Gateway/l2-a                  Gateway/l2-b
EnvoyProxy/l2-a               EnvoyProxy/l2-b
  |                               |
  | cluster-router                | cluster-router
  | local cluster + HTTPS cluster | local cluster + HTTPS cluster
  v                               v
upstream-a                     upstream-c
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
- Two L2 `Gateway` and `EnvoyProxy` pairs, one per shard.
- One `EnvoyPatchPolicy` for each generated L2 backend cluster that needs the
  cluster-router module.
- One TLS-enabled L2 provider route or cluster for internet provider egress.
- One `examples/cluster-router` config extension for provider endpoints,
  including scheme, authority, SNI, and optional protocol.
- One demo control-plane Deployment and Service.
- Upstream A, B, C, and D Deployments and Services.

The first cut should use two Envoy Gateway managed proxy deployments:

```text
EnvoyProxy/l1
EnvoyProxy/l2-a
EnvoyProxy/l2-b
```

That keeps the L1 and L2 responsibilities separate and gives each shard its own
Envoy process for shard-local cluster-router config.

The first-cut resource map should look like this:

```text
transit-system
  Envoy Gateway controller

transit-dataplane
  EnvoyProxy/l1
  Gateway/l1
  Service/l1
  HTTPRoute/l1-public
  EnvoyPatchPolicy/l1-cluster-shard-router

  EnvoyProxy/l2-a
  Gateway/l2-a
  Service/l2-a
  HTTPRoute/l2-a
  EnvoyPatchPolicy/l2-a-cluster-router

  EnvoyProxy/l2-b
  Gateway/l2-b
  Service/l2-b
  HTTPRoute/l2-b
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
  Gateway/l2-a
  Gateway/l2-b
  EnvoyProxy/l1
  EnvoyProxy/l2-a
  EnvoyProxy/l2-b
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
`transit-dataplane`. If Gateway Namespace Mode is enabled through the runtime
config instead of Helm values, add a RoleBinding in `transit-dataplane` for the
Envoy Gateway service account from `transit-system`. Without that RoleBinding,
the controller can accept Gateways but cannot create the generated Envoy
Deployment and Service.

The controller also needs cluster-scope `tokenreviews.authentication.k8s.io`
`create` permission so it can validate the service-account JWT used by Envoy
data planes when they connect back to xDS. If this is missing, Envoy pods are
created but stay unready because xDS authentication fails.

For tighter demos, configure Envoy Gateway to watch only the namespaces we
need. Include both `transit-system` and `transit-dataplane`; the controller
still needs to see its own TLS secret in `transit-system`:

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
          - transit-system
          - transit-dataplane
```

The integration manifests and e2e use `transit-system` for the controller and
`transit-dataplane` for Gateway and Envoy data-plane resources.

## L2 Deployment Shape

### Rationale

The first cut deliberately uses separate physical L2 proxies:
`EnvoyProxy/l2-a` and `EnvoyProxy/l2-b`.

L1 still needs stable shard targets. A config entry such as
`l2-a.transit-dataplane.svc.cluster.local:80` is easy to reason about, easy to
dump, and does not depend on Envoy Gateway generated resource names. It also
matches the production mental model: L1 routes to a shard entrypoint, not to a
random proxy pod.

The separate L2 deployments are worth the extra resources because each L2 shard
can hold a different cluster-router config for the same model. `Service/l2-a`
selects pods labeled `transit.dio/proxy: l2-a`, and `Service/l2-b` selects pods
labeled `transit.dio/proxy: l2-b`. L1 config does not need to change as shards
move from demo to production because the stable service names stay the same.

Gateway attachment is explicit. The dataplane namespace is labeled
`transit.dio/gateway-routes: "true"`, and the L1 and L2 Gateway listeners use
`allowedRoutes.namespaces.from: Selector` with that label. That keeps the demo
honest about ownership: only namespaces the operator marks for this dataplane
can attach `HTTPRoute` resources to these Gateways.

### First Cut Shape

The first cut uses one L2 EnvoyProxy per shard:

```text
Gateway/l2-a
  |
  +-- HTTPRoute/l2-a
  v
EnvoyProxy/l2-a
  |
  v
L2 A Envoy Deployment

Gateway/l2-b
  |
  +-- HTTPRoute/l2-b
  v
EnvoyProxy/l2-b
  |
  v
L2 B Envoy Deployment
```

L1 should not point at an Envoy Gateway generated Service name directly if that
name is unstable. L1 should point at stable Kubernetes Service names owned by
the demo or operator:

```text
l2-a.transit-dataplane.svc.cluster.local:80
l2-b.transit-dataplane.svc.cluster.local:80
```

The demo pins Envoy Gateway generated service names through `EnvoyProxy`:
`Service/l1`, `Service/l2-a`, and `Service/l2-b`. It also adds stable pod
labels such as `transit.dio/proxy: l1`, `transit.dio/proxy: l2-a`, and
`transit.dio/proxy: l2-b`. Those labels give tests and operator-owned resources
predictable selectors without depending on every generated label Envoy Gateway
adds.

For the first cut, the shard services are the pinned Envoy Gateway Services.
They select different L2 EnvoyProxy pod labels:

```yaml
apiVersion: v1
kind: Service
metadata:
  name: l2-a
  namespace: transit-dataplane
  labels:
    transit.dio/proxy: l2-a
    transit.dio/shard: a
spec:
  selector:
    transit.dio/proxy: l2-a
  ports:
  - name: http
    port: 80
---
apiVersion: v1
kind: Service
metadata:
  name: l2-b
  namespace: transit-dataplane
  labels:
    transit.dio/proxy: l2-b
    transit.dio/shard: b
spec:
  selector:
    transit.dio/proxy: l2-b
  ports:
  - name: http
    port: 80
```

The exact `targetPort` is generated by Envoy Gateway for each proxy, so the
README keeps it out of the contract. The contract is the stable service name
and the per-shard selector label. That lets L1 target stable shard DNS names
while each L2 shard has its own Envoy process.

When a new shard is added in production, Transit should not create Kubernetes
resources itself. A controller or operator should reconcile the physical L2
shape for that shard by writing Gateway API and Envoy Gateway resources:

- `Gateway` or listener attachment for the shard entrypoint
- `HTTPRoute` for the shard service name or hostname
- `EnvoyProxy` when the shard needs its own rollout or failure domain
- stable `Service` names that L1 can target
- `EnvoyPatchPolicy` for the generated backend cluster

After those resources exist, the control plane can publish the logical shard
entry in the L1 config. That keeps physical deployment ownership separate from
Transit request routing.

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

Provider egress needs one more split: local mock L2 routes can stay plaintext,
but internet provider routes need TLS-enabled Envoy clusters. The working
example uses a dedicated HTTPS provider path instead of attaching TLS to the
same cluster that serves local `upstream-a` through `upstream-d`. The route
uses an Envoy `UpstreamTlsContext` transport socket and sets SNI at the cluster
level.

The `cluster-router` module should keep provider routing explicit in dumps and
tests, but it does not need to become a TLS configuration API. The control plane
can serve a scoped model config like this:

```json
{
  "scope": "https-provider",
  "initial": {
    "version": "https-provider",
    "models": {
      "gpt-secure": {
        "target": "provider.local:443",
        "provider": "openai",
        "auth_header": "Bearer provider-token"
      }
    }
  }
}
```

The integration patch then decides whether that scoped cluster uses local CA
validation, public roots, custom SNI, or later mTLS. The optional H2 variant
should keep the same Transit model config and change only the Envoy cluster
patch: HTTP/2 protocol options plus ALPN set to `h2`. The Transit
host-selection API should not grow transport-level TLS knobs unless Envoy's
dynamic module Cluster Extension API requires it later.

Each L2 logical shard asks the control plane for its own config:

```text
GET /l2/a/routes.json
GET /l2/b/routes.json
```

That lets the control plane return different model, BYOK, provider, and profile
config for L2 A and L2 B. Each shard has its own L2 Envoy process, so
cluster-router config can stay shard-local instead of sharing process-global
state.

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
- `x-transit-tag: h-demo` and `x-model: gpt-secure` reaches an HTTPS provider
  through a TLS-enabled L2 provider cluster.
- The HTTPS provider observes SNI for the provider name.

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

The local demo starts by running the e2e with `KEEP_CLUSTER=1`. The e2e does
the heavy setup: it creates k3d, installs Envoy Gateway, applies the manifests,
patches the generated L1 backend cluster, and leaves the cluster running.

```sh
KEEP_CLUSTER=1 make -C integrations/tiered-router-eg e2e
make -C integrations/tiered-router-eg cli
```

Inspect the physical L2 shape:

```sh
kubectl --context k3d-transit-tiered-router-eg -n transit-dataplane get svc l2-a l2-b --show-labels
kubectl --context k3d-transit-tiered-router-eg -n transit-dataplane get pods -l 'transit.dio/proxy in (l2-a,l2-b)' --show-labels
```

Expected shape:

```text
NAME   TYPE        CLUSTER-IP    EXTERNAL-IP   PORT(S)   AGE   LABELS
l2-a   ClusterIP   10.43.x.y     <none>        80/TCP    1m    transit.dio/proxy=l2-a,transit.dio/shard=a
l2-b   ClusterIP   10.43.x.z     <none>        80/TCP    1m    transit.dio/proxy=l2-b,transit.dio/shard=b

NAME                      READY   STATUS    LABELS
l2-a-...                  2/2     Running   transit.dio/proxy=l2-a,transit.dio/shard=a,...
l2-b-...                  2/2     Running   transit.dio/proxy=l2-b,transit.dio/shard=b,...
```

Open two port-forwards in separate terminals:

```sh
kubectl --context k3d-transit-tiered-router-eg -n transit-dataplane \
  port-forward service/tiered-router-control 19080:8080
```

```sh
kubectl --context k3d-transit-tiered-router-eg -n transit-dataplane \
  port-forward service/l1 19081:80
```

Dump the control-plane view:

```sh
./integrations/tiered-router-eg/dist/tiered-router-demo l1 routes \
  --control-url http://127.0.0.1:19080 | jq

./integrations/tiered-router-eg/dist/tiered-router-demo l2 routes a \
  --control-url http://127.0.0.1:19080 | jq

./integrations/tiered-router-eg/dist/tiered-router-demo l2 routes b \
  --control-url http://127.0.0.1:19080 | jq
```

Expected excerpts:

```json
{
  "version": "bootstrap",
  "shards": {
    "a": {
      "target": "l2-a.transit-dataplane.svc.cluster.local:80",
      "prefixes": ["a"],
      "shard": "a"
    },
    "b": {
      "target": "l2-b.transit-dataplane.svc.cluster.local:80",
      "prefixes": ["b"],
      "shard": "b"
    }
  }
}
```

```json
{
  "version": "bootstrap",
  "models": {
    "gpt-fast": {
      "target": "upstream-a.transit-dataplane.svc.cluster.local:8080",
      "provider": "openai",
      "auth_header": "Bearer shard-a-openai-token",
      "profile": "profile-a"
    }
  }
}
```

```json
{
  "version": "bootstrap",
  "models": {
    "gpt-fast": {
      "target": "upstream-c.transit-dataplane.svc.cluster.local:8080",
      "provider": "openai",
      "auth_header": "Bearer shard-b-openai-token",
      "profile": "profile-b"
    }
  }
}
```

Drive the request path through the public L1 Gateway with the CLI:

```sh
./integrations/tiered-router-eg/dist/tiered-router-demo request gpt-fast \
  --tag a-demo \
  --gateway-url http://127.0.0.1:19081 \
  --host tiered-router.example.com | jq

./integrations/tiered-router-eg/dist/tiered-router-demo request gpt-fast \
  --tag b-demo \
  --gateway-url http://127.0.0.1:19081 \
  --host tiered-router.example.com | jq
```

Expected result:

```text
a-demo + gpt-fast -> l2-a -> upstream-a -> profile-a
b-demo + gpt-fast -> l2-b -> upstream-c -> profile-b
```

Expected response excerpts:

```json
{
  "upstream": "upstream-a",
  "l1_tag": "a-demo",
  "l1_shard": "a",
  "l1_target": "l2-a.transit-dataplane.svc.cluster.local:80",
  "model": "gpt-fast",
  "provider": "openai",
  "profile": "profile-a",
  "byok_key_id": "key-a-001",
  "l2_version": "bootstrap"
}
```

```json
{
  "upstream": "upstream-c",
  "l1_tag": "b-demo",
  "l1_shard": "b",
  "l1_target": "l2-b.transit-dataplane.svc.cluster.local:80",
  "model": "gpt-fast",
  "provider": "openai",
  "profile": "profile-b",
  "byok_key_id": "key-b-001",
  "l2_version": "bootstrap"
}
```

For a BYOK-focused demo:

```sh
./integrations/tiered-router-eg/dist/tiered-router-demo request gpt-fast \
  --byok-key-id key-a-001 \
  --gateway-url http://127.0.0.1:19081 \
  --host tiered-router.example.com

./integrations/tiered-router-eg/dist/tiered-router-demo request gpt-fast \
  --byok-key-id key-b-001 \
  --gateway-url http://127.0.0.1:19081 \
  --host tiered-router.example.com
```

The output should prove that each key ID lands on its owning shard and that the
raw auth secret does not appear in the dump.

To inspect the active L2 module config, forward the L2 services and hit the
debug path:

```sh
kubectl --context k3d-transit-tiered-router-eg -n transit-dataplane \
  port-forward service/l2-a 19082:80
```

```sh
kubectl --context k3d-transit-tiered-router-eg -n transit-dataplane \
  port-forward service/l2-b 19083:80
```

```sh
curl -s http://127.0.0.1:19082/__cluster-router/config | jq
curl -s http://127.0.0.1:19083/__cluster-router/config | jq
```

Expected excerpt for L2 A:

```json
{
  "version": "bootstrap",
  "models": {
    "gpt-fast": {
      "target": "upstream-a.transit-dataplane.svc.cluster.local:8080",
      "provider": "openai",
      "profile": "profile-a",
      "byok_key_id": "key-a-001"
    }
  }
}
```

Expected excerpt for L2 B:

```json
{
  "version": "bootstrap",
  "models": {
    "gpt-fast": {
      "target": "upstream-c.transit-dataplane.svc.cluster.local:8080",
      "provider": "openai",
      "profile": "profile-b",
      "byok_key_id": "key-b-001"
    }
  }
}
```

The active module dumps must not contain `Bearer`, `auth_header`, or raw token
values such as `shard-a-openai-token` and `shard-b-openai-token`.

When the demo is done:

```sh
k3d cluster delete transit-tiered-router-eg
```

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

- [x] Write the L1 module under `examples/cluster-shard-router`.
   - derive tags from headers
   - route tag prefixes to L2 hosts
   - inject visible L1 decision headers
   - unit test tag derivation and prefix matching

- [x] Add the integration skeleton.
   - Makefile
   - Dockerfiles
   - k8s templates
   - demo control-plane binary
   - host-side CLI

- [x] Bring up L1 only.
   - one L1 Gateway
   - L1 routes directly to echo upstreams
   - prove tag prefix selection

- [x] Add physical L2 proxies with stable shard services.
   - reuse `examples/cluster-router`
   - create `Gateway/l2-a`, `Gateway/l2-b`, `EnvoyProxy/l2-a`, and
     `EnvoyProxy/l2-b`
   - create `HTTPRoute/l2-a` and `HTTPRoute/l2-b`
   - create stable `Service/l2-a` and `Service/l2-b`
   - first prove L1 selects the right L2 logical shard service
   - then patch each generated L2 backend cluster to use `cluster-router`
   - keep shard-local configs in separate L2 Envoy processes

- [x] Add shard-local BYOK/profile metadata to the request path.
   - each L2 has different auth/profile config for the same model

- [x] Add redacted active dump assertions.
   - dump output redacts secrets

- [x] Prove HTTPS provider egress in `examples/cluster-router`.
   - use a dedicated TLS-enabled provider route and cluster
   - keep plaintext local upstreams in their own cluster
   - scope the cluster-router config per Envoy cluster
   - assert Envoy originates TLS, validates the provider certificate, and sends
     SNI

- [ ] Wire HTTPS provider egress into this Envoy Gateway integration.
   - add a dedicated TLS-enabled L2 provider path
   - start with a local HTTPS provider and generated CA for deterministic CI
   - optionally add a public provider smoke test later
   - keep TLS/SNI/validation_context in the Envoy cluster patch

- [ ] Optionally add HTTP/2 provider egress.
   - keep this separate from the first HTTPS proof
   - keep provider protocol in control-plane or patch metadata
   - patch the Envoy cluster with HTTP/2 protocol options
   - set TLS ALPN to `h2`

- [ ] Add CI.
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
- Should provider protocol be only `http/1.1` and `h2`, or should the config use
  a more explicit enum such as `http1` and `http2`?

## Recommended First Cut

Start with explicit `x-transit-tag`.

That proves the tiered topology, EnvoyPatchPolicy shape, and L1 Cluster
Extension behavior without mixing in hashing semantics too early. Once the
topology is green, add `x-byok-key-id` and `x-user-key` derivation as a focused
second step.
