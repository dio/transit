# MCP Profile Tiered Router Integration

A runnable Envoy Gateway integration that proves the L1/L2 MCP profile fan-out
topology. L1 owns profile auth, fan-out, and tool routing. L2 owns cataloged
server execution and backend host selection via cluster-router.

L1 uses `examples/mcp-profile-gateway` as an Envoy dynamic module filter.
L2 uses a Go catalog-router binary that handles `/mcp/s/{server}` and injects
the `x-mcp-server` header for cluster-router. L2 Envoy carries the
cluster-router extension (.so) to serve `/__cluster-router/config` dump.

## Scenario

Four fake MCP backends are organized into two L2 clusters. L1 exposes one
profile endpoint that fans out across both.

```text
L1 profile /mcp/9b3f7d0a80c4aa6d-67261ca9ea3dadb2:
  kiwi            -> L2-A /mcp/s/kiwi
  aws-knowledge   -> L2-A /mcp/s/aws-knowledge
  microsoft       -> L2-B /mcp/s/microsoft
  github          -> L2-B /mcp/s/github

L2-A server cluster: kiwi, aws-knowledge
L2-B server cluster: microsoft, github
```

The demo proves:

- `initialize` fans out to all four backends and returns a composite session.
- `tools/list` returns merged tools from both L2 clusters.
- `tools/call github.search` reaches L2-B and the GitHub backend only.
- `tools/call kiwi.search-flight` reaches L2-A and the Kiwi backend only.
- Invalid profile auth is rejected before any L2 or backend is reached.
- Public `/mcp/s/aws-knowledge` through L1 reaches L2-A only.
- Public `/mcp/s/github` through L1 reaches L2-B only.
- Direct `/mcp/s/github` at L2-A is rejected (cross-shard).
- Direct `/mcp/s/kiwi` at L2-B is rejected.
- L2 cluster-router dumps show `route_header: x-mcp-server`. No bearer tokens
  appear in any dump surface.

## Architecture

```text
client
  |
  v
Gateway/l1  (mcp-profile-gateway filter)
  profile auth, initialize fan-out, tools/list fan-out, tools/call routing
  |                                  |
  v                                  v
Gateway/l2-a                     Gateway/l2-b
  (mcp-catalog-router)             (mcp-catalog-router)
  (cluster-router egress)          (cluster-router egress)
  |               |                 |                |
  v               v                 v                v
mcp-kiwi  mcp-aws-knowledge   mcp-microsoft     mcp-github
```

L1 selects the owning L2 cluster from an explicit server-slug→L2 ownership map
built from Envoy-Gateway-generated cluster names discovered at setup time. L1
does not know concrete backend host placement. Concrete placement lives inside
L2 and cluster-router.

L2 translates `/mcp/s/{server-slug}` to an outbound backend request with
`x-mcp-server: <slug>`. Cluster-router reads that header and selects the
concrete backend host.

## Components

The integration creates in the `transit-dataplane` namespace:

- Envoy Gateway installed by Helm.
- `EnvoyProxy/l1`, `EnvoyProxy/l2-a`, `EnvoyProxy/l2-b`, each pointing at the
  custom Envoy image with distinct environment config.
- `GatewayClass`, `Gateway/l1`, `Gateway/l2-a`, `Gateway/l2-b`.
- `HTTPRoute` resources for profile, public catalog, L2 routing, and init
  cluster discovery.
- `EnvoyPatchPolicy` resources patching listener and cluster config for each
  tier.
- Demo Deployment and Service (catalog-router process + four fake MCP backends
  in one binary).

The custom Envoy image contains:

- `/etc/envoy/dynamic-modules/libmcp-profile-gateway.so`
- `/etc/envoy/dynamic-modules/libmcp-catalog-router.so`
- `/etc/envoy/dynamic-modules/libcluster-router.so`
- Debian bookworm slim as the glibc userspace.

The demo image contains one static Go binary. It runs the L2 catalog-router
server and four fake MCP backends (kiwi, aws-knowledge, microsoft, github), each
returning deterministic `tools/list` and `tools/call` responses.

## Ownership Model

L1 owns user-facing MCP profiles:

- `/mcp/{profile-id}` lookup
- public `/mcp/s/{server-slug}` server endpoint routing
- profile API key or auth enforcement
- enabled tool policy
- profile-to-catalog-server membership
- profile credential binding and credential transport policy
- composite session envelope encode/decode
- fan-out for `initialize` and `tools/list`
- tool-to-server resolution for `tools/call`
- selection of the L2 server cluster that owns each server request

L2 owns clusters of cataloged MCP servers:

- `/mcp/s/{server-slug}` semantics
- backend auth injection
- request credential resolution from a reference or encrypted envelope
- server health and capabilities
- cluster-local catalog membership
- cluster-router config for concrete backend placement
- redacted dump/debug surfaces

The critical boundary: L2 does not own curated profile membership or profile
auth. L2 executes cataloged server requests. L1 resolves profiles into one or
more calls to the L2 clusters that front those servers.

## Endpoint Shape

```text
/mcp/{profile-id}
  = L1 curated profile endpoint

/mcp/s/{server-slug}
  = public L1 cataloged single-server endpoint
  = internal L2 cataloged single-server endpoint after L1 ownership routing
```

Clients call both endpoint shapes through L1. L1 forwards
`/mcp/s/aws-knowledge` to L2-A and `/mcp/s/github` to L2-B based on cataloged
server ownership. L2 handles the same path only after L1 has selected the owning
cluster.

The integration does not use `cluster-shard-router` for L1 catalog server
routing. The shard router selects from request headers such as `x-transit-tag`,
not from `/mcp/s/{server-slug}`. L1 uses an explicit ownership map of server
slug to L2 cluster.

L2 HTTPRoutes match `PathPrefix: /mcp`, not `/mcp/`, so all catalog requests
reach the L2 catalog front end. The generated frontend route cluster must not be
replaced with cluster-router — that would bypass the component that derives
`x-mcp-server` from `/mcp/s/{server-slug}`. Cluster-router patches only a
separate catalog egress cluster used by the L2 front end after it has set
`x-mcp-server` and rewritten the backend path.

Cluster-router config for L2-A:

```json
{
  "route_header": "x-mcp-server",
  "initial": {
    "version": "l2-a",
    "models": {
      "kiwi": {
        "target": "mcp-kiwi.transit-dataplane.svc.cluster.local:8080",
        "provider": "mcp",
        "auth_header": "Bearer kiwi-token"
      },
      "aws-knowledge": {
        "target": "mcp-aws-knowledge.transit-dataplane.svc.cluster.local:8080",
        "provider": "mcp",
        "auth_header": "Bearer aws-token"
      }
    }
  }
}
```

The `models` field comes from the existing cluster-router example. Treat those
keys as logical MCP backend server keys. The config is parameterized by
`route_header` so LLM paths use `x-model` while MCP paths use `x-mcp-server`.

## Profile Configuration

L1 gets profile JSON through `MCP_PROFILE_GATEWAY_CONFIG` set by the
`EnvoyProxy` resource. The integration builds the config at cluster setup time
using the Envoy-Gateway-generated cluster names for the L1→L2 callout routes.

The `mcp-profile-gateway` module implements:
- public catalog forwarding
- profile `initialize` session fan-out
- profile `tools/list` fan-out
- profile `tools/call` routing
- redacted dump surfaces

Static config can later be replaced by dynamic profile fetching from an
L1-owned profile service. L1 would fetch and cache `/mcp/{profile-id}` with
explicit TTL, stale-if-error, and auth failure behavior. All dump and debug
surfaces must redact profile API keys, credential refs and envelopes, session
values, catalog URL userinfo, and catalog URL query strings.

L2 should not fetch curated profiles. L2 may fetch or cache cataloged server
metadata, server health, and credential policy needed to execute a cataloged
server request.

## Credential Binding

Profile-owned server credentials need a split ownership model. When a user adds
GitHub to a profile, the profile stores the fact that the GitHub cataloged server
is enabled and bound to that user's GitHub token. L1 owns that binding and policy
decision, but L2-B must receive or resolve the credential needed to execute
`/mcp/s/github`.

Supported implementation shapes:

- L1 sends a credential reference (`profile/server/user`); L2 resolves it from a
  credential service.
- L1 sends an encrypted, audience-bound credential envelope; L2 or a local
  service decrypts it for this request.
- L1 exchanges the stored credential for a short-lived backend token; L2 injects
  that token toward the backend.

All shapes bind credential material to profile, server, subject, L2 audience,
and expiry. L2 may apply and inject backend credentials, but must not log, dump,
or persist raw user-provided tokens.

## MCPProxy Parity

The behavioral target is Envoy AI Gateway MCPProxy.

Admission behavior:

- `initialize` may omit downstream `mcp-session-id`.
- Non-`initialize` profile requests require a valid profile session.
- Invalid profile sessions fail before any L2 request.
- Unsupported methods and malformed JSON-RPC requests fail before any L2 request.
- `notifications/initialized` should be accepted locally, not fanned out.

Aggregate behavior:

- `initialize` fans out to all configured profile member servers. Backend
  initialize failures are recorded and omitted from the composite session.
  Downstream initialize succeeds if at least one backend succeeds and fails if
  all backends fail.
- `tools/list` fans out only to backends present in the composite session.
  Backend errors are recorded and omitted. Partial success returns merged healthy
  tools. If every backend fails or all tools are filtered out, return a
  successful empty `{"tools":[]}` result.
- `tools/call` never fans out. It resolves one profile-visible tool to one
  backend, verifies the backend is enabled and in the composite session, rewrites
  the public tool name to the backend tool name, and forwards one request.
  Backend 2xx JSON-RPC error responses are proxied. Unknown backend,
  disabled/invalid tool, missing backend session, transport failure, or non-2xx
  backend response become immediate downstream errors.

Known differences from AI Gateway MCPProxy for this integration:

- AI Gateway uses `{backend}__{tool}` tool names. The current example uses
  `{prefix}.{tool}`. Switch to double-underscore separator before treating the
  integration API as product-stable.
- AI Gateway aggregate responses are SSE. The current example returns ordinary
  JSON for the first topology pass. Add buffered final-event SSE parity before
  claiming close MCPProxy compatibility or testing real Streamable HTTP clients.

## k3d Shape

The suite uses a single-node k3d cluster named `transit-mcp-profile-eg` with
Kubernetes context `k3d-transit-mcp-profile-eg`. Single-node works because the
e2e uses port-forwarding for the Gateway and Envoy admin endpoints.

Images are local by default and imported into k3d with:

```sh
k3d image import -c transit-mcp-profile-eg --mode direct ...
```

Use `make publish` only when a remote cluster needs to pull from ttl.sh. CI
publishes short-lived ttl.sh images, passes those tags into the e2e suite, and
sets `K3D_SKIP_IMAGE_IMPORT=1` so the k3d cluster pulls from ttl.sh instead of
importing local Docker images from the runner. Local demos should keep the
default direct import path — it is faster and avoids an external registry round
trip.

## Context Safety

All Go helper calls inject:

```sh
kubectl --context k3d-transit-mcp-profile-eg ...
helm --kube-context k3d-transit-mcp-profile-eg ...
```

The helpers refuse non-k3d contexts. Never run bare `kubectl` against this demo
cluster.

## Build And Test

Run the fast integration unit tests:

```sh
make -C integrations/mcp-profile-tiered-router-eg test
```

Run the full e2e (requires Docker, k3d, kubectl, helm):

```sh
make -C integrations/mcp-profile-tiered-router-eg e2e
```

That target builds:

- Transit dynamic modules (`.so` files via Zig cross-compile)
- Custom Envoy image with all three modules
- Linux static demo binary (catalog-router + fake MCP backends)
- Demo container image

Then creates k3d, installs Envoy Gateway, applies Gateway resources, patches
Envoy xDS, and runs the topology assertions.

The underlying Go test is gated by `RUN_MCP_PROFILE_TIERED_ROUTER_EG_E2E=1`. The
`e2e` Make target sets it for you. A broad `go test ./...` compiles the package
but skips the local-cluster workflow.

Reuse images already built:

```sh
SKIP_IMAGE_BUILD=1 \
IMAGE=<envoy-image> \
CONTROL_PLANE_IMAGE=<demo-image> \
make -C integrations/mcp-profile-tiered-router-eg e2e
```

Keep the cluster after a run:

```sh
KEEP_CLUSTER=1 make -C integrations/mcp-profile-tiered-router-eg e2e
```

Reuse a kept cluster without resetting it:

```sh
KEEP_CLUSTER=1 RESET_CLUSTER=0 \
SKIP_IMAGE_BUILD=1 \
IMAGE=<envoy-image> \
CONTROL_PLANE_IMAGE=<demo-image> \
make -C integrations/mcp-profile-tiered-router-eg e2e
```

The default e2e resets the cluster before starting. `KEEP_CLUSTER=1` only
affects teardown.

## Local k3d Demo

Build the images, create k3d, install Envoy Gateway, and run the full scenario:

```sh
KEEP_CLUSTER=1 make -C integrations/mcp-profile-tiered-router-eg e2e
```

Confirm the cluster is the one you expect:

```sh
k3d cluster list
kubectl --context k3d-transit-mcp-profile-eg -n transit-dataplane \
  get gateway,httproute,envoyproxy,envoypatchpolicy,pods,svc
```

Open port-forwards in separate terminals:

```sh
kubectl --context k3d-transit-mcp-profile-eg -n transit-dataplane \
  port-forward service/l1 19081:80
```

```sh
kubectl --context k3d-transit-mcp-profile-eg -n transit-dataplane \
  port-forward service/l2-a 19082:80
```

```sh
kubectl --context k3d-transit-mcp-profile-eg -n transit-dataplane \
  port-forward service/l2-b 19083:80
```

Initialize the profile through L1 and capture the composite MCP session:

```sh
curl -sS -D /tmp/mcp-profile-init.headers \
  -H 'host: mcp-profile-tiered-router.example.com' \
  -H 'content-type: application/json' \
  -H 'x-api-key: profile-key' \
  --data '{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","clientInfo":{"name":"manual","version":"dev"}}}' \
  http://127.0.0.1:19081/mcp/9b3f7d0a80c4aa6d-67261ca9ea3dadb2 | jq

export MCP_SESSION_ID="$(
  awk 'tolower($1)=="mcp-session-id:" {gsub("\r","",$2); print $2}' \
    /tmp/mcp-profile-init.headers
)"
```

List profile tools through L1:

```sh
curl -sS \
  -H 'host: mcp-profile-tiered-router.example.com' \
  -H 'content-type: application/json' \
  -H 'x-api-key: profile-key' \
  -H "mcp-session-id: $MCP_SESSION_ID" \
  --data '{"jsonrpc":"2.0","id":2,"method":"tools/list"}' \
  http://127.0.0.1:19081/mcp/9b3f7d0a80c4aa6d-67261ca9ea3dadb2 | jq
```

Expected profile-visible tool names:

```text
kiwi.search-flight
aws-knowledge.aws____read_documentation
microsoft.search_docs
github.search
```

Call one L2-B tool through the profile endpoint:

```sh
curl -sS \
  -H 'host: mcp-profile-tiered-router.example.com' \
  -H 'content-type: application/json' \
  -H 'x-api-key: profile-key' \
  -H "mcp-session-id: $MCP_SESSION_ID" \
  --data '{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"github.search","arguments":{"query":"transit"}}}' \
  http://127.0.0.1:19081/mcp/9b3f7d0a80c4aa6d-67261ca9ea3dadb2 | jq
```

Call one L2-A tool through the profile endpoint:

```sh
curl -sS \
  -H 'host: mcp-profile-tiered-router.example.com' \
  -H 'content-type: application/json' \
  -H 'x-api-key: profile-key' \
  -H "mcp-session-id: $MCP_SESSION_ID" \
  --data '{"jsonrpc":"2.0","id":4,"method":"tools/call","params":{"name":"kiwi.search-flight","arguments":{"from":"SFO","to":"NRT"}}}' \
  http://127.0.0.1:19081/mcp/9b3f7d0a80c4aa6d-67261ca9ea3dadb2 | jq
```

Verify public catalog routing through L1:

```sh
curl -sS \
  -H 'host: mcp-profile-tiered-router.example.com' \
  -H 'content-type: application/json' \
  --data '{"jsonrpc":"2.0","id":5,"method":"tools/list"}' \
  http://127.0.0.1:19081/mcp/s/aws-knowledge | jq

curl -sS \
  -H 'host: mcp-profile-tiered-router.example.com' \
  -H 'content-type: application/json' \
  --data '{"jsonrpc":"2.0","id":6,"method":"tools/list"}' \
  http://127.0.0.1:19081/mcp/s/github | jq
```

Verify cross-shard rejection directly at L2:

```sh
curl -sS \
  -H 'content-type: application/json' \
  --data '{"jsonrpc":"2.0","id":7,"method":"tools/list"}' \
  http://127.0.0.1:19082/mcp/s/github | jq

curl -sS \
  -H 'content-type: application/json' \
  --data '{"jsonrpc":"2.0","id":8,"method":"tools/list"}' \
  http://127.0.0.1:19083/mcp/s/kiwi | jq
```

Inspect redacted debug surfaces:

```sh
curl -sS http://127.0.0.1:19081/dump | jq
curl -sS http://127.0.0.1:19082/__cluster-router/config | jq
curl -sS http://127.0.0.1:19083/__cluster-router/config | jq
```

Expected checks:

- L1 `/dump` does not expose `profile-key`, credential refs, raw credential
  envelopes, or `mcp-session-id` values.
- L2 cluster-router dumps show `route_header: x-mcp-server`.
- L2 dumps do not expose raw bearer token values (`kiwi-token`, `aws-token`,
  `microsoft-token`, `github-token`).

When the demo is done:

```sh
k3d cluster delete transit-mcp-profile-eg
```

## Make Targets

| Target | Description |
|---|---|
| `build-modules` | Cross-compile `.so` files with Zig |
| `image` | Build custom Envoy image with Transit modules |
| `control-plane-binary` | Build demo binary (catalog-router + fake MCP backends) |
| `control-plane-image` | Wrap demo binary in a container image |
| `publish` | Build and push images to `ttl.sh` (CI flow) |
| `eg-install` | Smoke test: install Envoy Gateway in k3d, no custom images needed |
| `e2e` | Full topology e2e: build images, create k3d cluster, run assertions |
| `test` | Unit tests only (no cluster required) |
| `clean` | Remove built artifacts |

`eg-install` only creates k3d, installs Envoy Gateway, enables EnvoyPatchPolicy,
and verifies CRDs. It does not build images or run the MCP scenario.

## Key Environment Variables

| Variable | Default | Description |
|---|---|---|
| `IMAGE` | auto-tagged | Custom Envoy image |
| `CONTROL_PLANE_IMAGE` | auto-tagged | Demo image (catalog-router + fake backends) |
| `SKIP_IMAGE_BUILD` | `0` | Set to `1` to skip image build steps |
| `KEEP_CLUSTER` | `0` | Set to `1` to keep k3d cluster after the suite |
| `RESET_CLUSTER` | `1` | Set to `0` to reuse an existing cluster |
| `K3D_SKIP_IMAGE_IMPORT` | `0` | Set to `1` to pull images from a registry |
| `K3D_IMAGE_IMPORT_MODE` | `direct` | k3d image import mode |
| `ENVOY_GATEWAY_VERSION` | `v1.8.0` | Envoy Gateway Helm chart version |

## E2E Assertions

Active (all pass in CI):

- L1 rejects invalid profile credentials. No L2 or backend request is made.
- `initialize` fans out to all four backends. Composite session contains all
  four.
- `tools/list` returns merged Kiwi, AWS, Microsoft, and GitHub tools from one
  profile response.
- `tools/call github.search` reaches L2-B and the GitHub backend only.
- `tools/call kiwi.search-flight` reaches L2-A and the Kiwi backend only.
- `tools/call` for a disabled or unknown tool returns a controlled JSON-RPC error.
- Public `/mcp/s/aws-knowledge` through L1 reaches L2-A only.
- Public `/mcp/s/github` through L1 reaches L2-B only.
- Direct `/mcp/s/github` at L2-A returns an error.
- Direct `/mcp/s/kiwi` at L2-B returns an error.
- L2-A and L2-B cluster-router dumps show `route_header: x-mcp-server`.
- Dumps do not expose bearer token values.

Skipped (require fake-MCP failure injection):

- Profile `initialize` partial backend failure: composite session contains only
  successful backends.
- Profile `initialize` all-backend failure: downstream returns an error.
- `tools/list` with one failing backend: healthy tools from other backends
  returned.
- `tools/list` with all backends failing: successful `{"tools":[]}`.
- `tools/list` with enabled-tool filtering applied.
- `tools/call` for a backend omitted from partial-initialize session reaches
  no L2.
- `tools/call` proxies backend 2xx JSON-RPC error responses.
- `tools/call` converts backend transport or non-2xx failures to downstream
  errors.

## Debugging

Use only pinned k3d context commands:

```sh
kubectl --context k3d-transit-mcp-profile-eg ...
```

Check pod and resource state:

```sh
kubectl --context k3d-transit-mcp-profile-eg -n transit-dataplane \
  get gateway,httproute,envoyproxy,envoypatchpolicy,pods,svc
kubectl --context k3d-transit-mcp-profile-eg -n transit-dataplane \
  get envoypatchpolicy -o yaml
```

If L1 fan-out returns 503 for some backends, check the L1 profile config cluster
names via the redacted dump surface, then compare against active cluster names in
Envoy admin `/config_dump`.

Verify `EnvoyPatchPolicy` status conditions (the e2e polls these directly instead
of using `kubectl wait`):

```sh
kubectl --context k3d-transit-mcp-profile-eg -n transit-dataplane \
  get envoypatchpolicy -o jsonpath='{.items[*].status.ancestors[*].conditions}'
```

Verify the `.so` files are mapped into L1 Envoy:

```sh
kubectl --context k3d-transit-mcp-profile-eg -n envoy-gateway-system \
  exec deploy/envoy-transit-dataplane-l1-<hash> -c envoy -- \
  ls /etc/envoy/dynamic-modules/
```

Port-forward the L1 Envoy admin endpoint and inspect config:

```sh
kubectl --context k3d-transit-mcp-profile-eg -n envoy-gateway-system \
  port-forward deploy/envoy-transit-dataplane-l1-<hash> 19000:19000

curl -fsS http://127.0.0.1:19000/config_dump | jq
```

## Status

Implemented and running in CI:

- Custom Envoy image with `libmcp-profile-gateway.so`, `libmcp-catalog-router.so`,
  `libcluster-router.so`
- L1 mcp-profile-gateway filter: profile auth, initialize fan-out, tools/list
  fan-out, tools/call routing, redacted dump
- L2 catalog-router demo app: `/mcp/s/{server}` handling with direct backend calls
- L2 cluster-router extension: initialized via dedicated init cluster, serves
  `/__cluster-router/config` dump
- Full e2e passing in CI with fake MCP backends
- All active assertions above covered

Not in scope for this integration:

- Real MCP servers in CI
- Streaming MCP transport
- Encrypted/authenticated composite session token
- Public internet provider egress
- Shared global profile store
- L2 curated profile fetching
- L1 awareness of concrete backend hosts

Next:

- Switch tool name separator from `{prefix}.{tool}` to `{prefix}__{tool}`
  (MCPProxy parity) before treating the endpoint shape as product-stable
- Add buffered final-event SSE parity before testing real Streamable HTTP clients
- Add dynamic L1 profile fetching to replace static `MCP_PROFILE_GATEWAY_CONFIG`
- Extend fake-mcp with failure injection to cover the skipped assertions above
- Keep real hosted MCP servers as optional manual smoke only
