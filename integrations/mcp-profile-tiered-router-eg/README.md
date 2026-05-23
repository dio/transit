# MCP Profile Tiered Router Integration

A runnable Envoy Gateway integration that proves the L1/L2 MCP profile fan-out
topology. L1 owns profile auth, fan-out, and tool routing. L2 owns cataloged
server execution and backend host selection via cluster-router.

Topology:

```text
client
  -> L1 Gateway (mcp-profile-gateway filter)
       /mcp/{profile-id}   profile auth, initialize fan-out, tools/list fan-out, tools/call routing
       /mcp/s/{server-slug} public catalog forwarding to owning L2 cluster
  -> L2-A Gateway (catalog-router demo app)
       /mcp/s/kiwi            -> mcp-kiwi backend
       /mcp/s/aws-knowledge   -> mcp-aws-knowledge backend
  -> L2-B Gateway (catalog-router demo app)
       /mcp/s/microsoft       -> mcp-microsoft backend
       /mcp/s/github          -> mcp-github backend
```

L1 uses `examples/mcp-profile-gateway` as an Envoy dynamic module filter.
L2 uses a Go catalog-router demo binary that handles `/mcp/s/{server}` and
injects the `x-mcp-server` header for cluster-router. L2 Envoy carries the
cluster-router extension (.so) to serve `/__cluster-router/config` dump.

## Running locally

Build images and run the full e2e (requires Docker, k3d, kubectl, helm):

```bash
make -C integrations/mcp-profile-tiered-router-eg e2e
```

Reuse images already built:

```bash
SKIP_IMAGE_BUILD=1 \
IMAGE=<envoy-image> \
CONTROL_PLANE_IMAGE=<demo-image> \
make -C integrations/mcp-profile-tiered-router-eg e2e
```

Keep cluster alive after a run for manual inspection:

```bash
KEEP_CLUSTER=1 make -C integrations/mcp-profile-tiered-router-eg e2e
```

## Make targets

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

## Key environment variables

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

The critical boundary is that L2 does not own curated profile membership or
profile auth. L2 executes cataloged server requests. L1 resolves profiles into
one or more calls to the L2 clusters that front those servers.

## What This Should Prove

- L1 resolves `/mcp/{profile-id}` into cataloged server requests.
- L1 resolves public `/mcp/s/{server-slug}` requests to the owning L2 server
  cluster.
- L1 rejects invalid profile credentials before any L2 or backend is reached.
- L1 `tools/list` fans out to multiple L2 server clusters and merges enabled
  tools into the profile-visible view.
- L1 `tools/call` resolves one profile-visible tool to exactly one cataloged
  server.
- L2-A clusters the cataloged Kiwi and AWS Knowledge servers.
- L2-B clusters the cataloged Microsoft and GitHub servers.
- L2 egress uses cluster-router with `route_header: x-mcp-server`.
- Cluster-router selects concrete backend hosts and injects the credential
  material L2 resolved for the request.
- The same cataloged server key can resolve to different concrete hosts in
  different L2 clusters if placement requires it.

## MCPProxy Parity

The behavioral target is Envoy AI Gateway MCPProxy. This integration should
stay close to that method-level behavior unless a difference is explicitly
called out.

Admission behavior:

- `initialize` may omit downstream `mcp-session-id`.
- Non-`initialize` profile requests require a valid profile session.
- Invalid profile sessions fail before any L2 request.
- Unsupported methods and malformed JSON-RPC requests fail before any L2
  request.
- `notifications/initialized` should be accepted locally when implemented, not
  fanned out as a tool operation.

Aggregate behavior:

- `initialize` fans out to all configured profile member servers. Backend
  initialize failures are recorded and omitted from the composite session.
  Downstream initialize succeeds if at least one backend initializes
  successfully and fails if all backend initializations fail.
- `tools/list` fans out only to backends present in the composite session.
  Backend JSON-RPC errors, malformed results, HTTP failures, and collection
  failures are recorded and omitted. Partial success returns merged healthy
  tools. If every backend fails, contributes no result, or all tools are
  filtered out, return a successful empty `{"tools":[]}` result.
- `tools/call` never fans out. It resolves one profile-visible tool to one
  backend, verifies that the backend is enabled and present in the composite
  session, rewrites the public tool name to the backend tool name, and forwards
  one request. Backend 2xx JSON-RPC error responses are proxied; unknown
  backend, disabled/invalid tool, missing backend session, transport failure,
  and non-2xx backend response become immediate downstream errors.

Known differences from AI Gateway MCPProxy for this first integration:

- AI Gateway uses `{backend}__{tool}` tool names. The current example uses
  `{prefix}.{tool}`. Switch to a double-underscore separator before treating the
  integration API as product-stable.
- AI Gateway aggregate responses are SSE. The current example returns ordinary
  JSON for the first topology pass. Add buffered final-event SSE parity before
  claiming close MCPProxy compatibility or testing real Streamable HTTP clients.

## Initial Scenario

Use deterministic fake MCP backends only. Do not call real hosted MCP servers
from CI.

```text
L1 profile:
  /mcp/9b3f7d0a80c4aa6d-67261ca9ea3dadb2
    kiwi            -> L2-A /mcp/s/kiwi
    aws-knowledge   -> L2-A /mcp/s/aws-knowledge
    microsoft       -> L2-B /mcp/s/microsoft
    github          -> L2-B /mcp/s/github

L2-A server cluster:
  kiwi
  aws-knowledge

L2-B server cluster:
  microsoft
  github
```

Expected behavior:

```text
POST /mcp/9b3f7d0a80c4aa6d-67261ca9ea3dadb2 method=tools/list
  -> L1 validates profile auth
  -> L1 fans out to L2-A and L2-B server cluster endpoints
  -> merged profile-visible tools:
       kiwi.search-flight
       aws-knowledge.aws____read_documentation
       microsoft.search_docs
       github.search
```

Tool calls route to the owning cataloged server:

```text
tools/call github.search
  -> L1 resolves github.search to catalog server github
  -> L1 calls L2-B /mcp/s/github
  -> L2-B sets x-mcp-server: github
  -> cluster-router reaches the GitHub backend only

tools/call disabled.github.search
  -> L1 rejects with a controlled JSON-RPC "unknown tool" or "disabled tool"
  -> no L2 or backend request is made
```

Cataloged single-server requests also enter through L1:

```text
POST /mcp/s/aws-knowledge method=tools/list
  -> L1 resolves aws-knowledge to L2-A
  -> L1 forwards to L2-A /mcp/s/aws-knowledge
  -> L2-A sets x-mcp-server: aws-knowledge
  -> cluster-router reaches the AWS Knowledge backend only
```

## Topology

```text
Gateway/l1
  EnvoyProxy/l1
  mcp-profile-gateway (examples/mcp-profile-gateway)
    profile fetch/auth/policy/session
    fan-out to L2 server cluster endpoints
    explicit catalog-server ownership map
    kiwi          -> l2-a
    aws-knowledge -> l2-a
    microsoft     -> l2-b
    github        -> l2-b

Gateway/l2-a
  EnvoyProxy/l2-a
  mcp-catalog-router (examples/mcp-catalog-router)
  cluster-router egress (examples/cluster-router)
  fake MCP backends:
    kiwi
    aws-knowledge

Gateway/l2-b
  EnvoyProxy/l2-b
  mcp-catalog-router (examples/mcp-catalog-router)
  cluster-router egress (examples/cluster-router)
  fake MCP backends:
    microsoft
    github
```

The L1 router may know profile membership, which L2 cluster owns each cataloged
server, and how a profile-bound credential should be transported to that L2
cluster. It should not know concrete backend host placement. Concrete host
placement remains inside L2 and cluster-router.

Each L2 cluster owns:

- cataloged server membership
- backend credential application and injection
- credential reference or encrypted-envelope resolution
- `/mcp/s/{server-slug}` to `x-mcp-server` translation
- cluster-router config for cluster-local backend placement
- backend health and capability metadata
- redacted dump/debug surfaces

## Credential Binding

Profile-owned server credentials need a split ownership model. For example, when
a user adds GitHub to profile
`/mcp/9b3f7d0a80c4aa6d-a05ab78c38fc99dd`, the profile stores the fact that the
GitHub cataloged server is enabled and bound to that user's GitHub token. L1
owns that profile binding and policy decision, but L2-B must receive or resolve
the credential needed to execute `/mcp/s/github`.

Supported implementation shapes:

- L1 sends a credential reference such as `profile/server/user`; L2 resolves it
  from a credential service.
- L1 sends an encrypted, audience-bound credential envelope; L2 or a local
  service decrypts it for this request.
- L1 exchanges the stored credential for a short-lived backend token; L2 injects
  that token toward the backend.

All shapes should bind credential material to profile, server, subject, L2
audience, and expiry. L2 may apply and inject backend credentials, but it must
not log, dump, or persist raw user-provided tokens.

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

Later, static config can be replaced by dynamic profile fetching from an
L1-owned profile service:

- L1 fetches and validates `/mcp/{profile-id}` profile configuration.
- L1 caches profile data with explicit TTL, stale-if-error, and auth failure
  behavior.
- L1 debug and dump surfaces must redact profile API keys, credential refs and
  envelopes, session values, catalog URL userinfo, and catalog URL query
  strings.
- L1 profile data points at cataloged server keys and owning L2 clusters, not
  concrete backend hosts. It may include credential references or encrypted
  credential envelopes needed for L2 execution.

L2 should not fetch curated profiles. L2 may fetch or cache cataloged server
metadata, server health, and credential policy needed to execute a cataloged
server request.

## Endpoint Shape

The integration should keep the product endpoint model:

```text
/mcp/{profile-id}
  = L1 curated profile endpoint

/mcp/s/{server-slug}
  = public L1 cataloged single-server endpoint
  = internal L2 cataloged single-server endpoint after L1 ownership routing
```

Clients should call both endpoint shapes through L1. L1 forwards
`/mcp/s/aws-knowledge` to L2-A and `/mcp/s/github` to L2-B based on cataloged
server ownership. L2 handles the same path only after L1 has selected the owning
server cluster.

The integration intentionally does not use `cluster-shard-router` for L1 catalog
server routing. The shard router selects from request headers such as
`x-transit-tag`, not from `/mcp/s/{server-slug}`. L1 uses an explicit ownership
map of server slug to L2 cluster, built from the Envoy-Gateway-generated cluster
names discovered at setup time.

L2 must have a catalog front end before cluster-router. That front end translates
`/mcp/s/{server-slug}` into an outbound backend request with an explicit MCP
routing header:

```text
x-mcp-server: kiwi
x-mcp-server: aws-knowledge
```

L2 HTTPRoutes match `PathPrefix: /mcp`, not `/mcp/`, so exact catalog requests
and catalog paths reach the L2 catalog front end. The generated frontend route
cluster must not be replaced with cluster-router, because that would bypass the
component that derives `x-mcp-server` from `/mcp/s/{server-slug}`. Cluster-router
should patch only a separate catalog egress cluster used by the L2 front end
after it has set `x-mcp-server` and rewritten the backend path as needed.

Cluster-router config should set:

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

The `models` field name comes from the existing cluster-router example. For
this integration, treat those keys as logical MCP backend server keys. The
config is intentionally parameterized by `route_header`, so LLM paths can keep
using `x-model` while MCP paths use `x-mcp-server`.

## Test Matrix

Required e2e cases:

- L1 profile auth failure reaches no L2 and no backend.
- profile `initialize` partial backend failure succeeds and the returned
  composite session contains only successful backends.
- profile `initialize` all-backend failure returns an error.
- L1 `tools/list` returns Kiwi, AWS, Microsoft, and GitHub tools from one
  profile response.
- L1 `tools/list` preserves enabled-tool filtering.
- L1 `tools/list` with one failing backend returns healthy tools from the other
  backends.
- L1 `tools/list` with all failing or filtered backends returns successful
  `{"tools":[]}`.
- L1 public `/mcp/s/aws-knowledge` reaches L2-A and the AWS Knowledge backend
  only.
- L1 public `/mcp/s/github` reaches L2-B and the GitHub backend only.
- L1 `tools/call github.search` reaches L2-B and the GitHub backend only.
- L1 `tools/call kiwi.search-flight` reaches L2-A and the Kiwi backend only.
- L1 `tools/call` for a disabled or unknown tool returns a controlled JSON-RPC
  error.
- L1 `tools/call` for a backend omitted from the partial-initialize session
  reaches no L2.
- L1 `tools/call` proxies backend 2xx JSON-RPC error responses.
- L1 `tools/call` converts backend transport or non-2xx failures to downstream
  errors.
- L2-A direct `/mcp/s/github` is unknown or unavailable.
- L2-B direct `/mcp/s/kiwi` is unknown or unavailable.
- cluster-router debug dump shows `route_header: x-mcp-server`.
- dumps and debug endpoints do not expose profile API keys or bearer tokens.

Nice-to-have later cases:

- omitted catalog server prefix falls back to server ID in the egress routing
  header.
- same server key can resolve to different concrete hosts in L2-A and L2-B.

## Non-Goals For First Pass

- No real MCP servers in CI.
- No streaming MCP transport.
- No encrypted/authenticated composite session token yet.
- No public internet provider egress.
- No shared global profile store.
- No L2 curated profile fetching.
- No L1 awareness of concrete backend hosts.

## Remaining Work

The integration now follows the same local image build shape as
`integrations/tiered-router-eg`: `make -C integrations/mcp-profile-tiered-router-eg
e2e` builds the custom Envoy image and demo image unless `SKIP_IMAGE_BUILD=1`
is set. The remaining work is:

- Keep the image build path healthy for both local k3d import and ttl.sh publish
  flows.
- Keep the L1 config path wired to Envoy Gateway generated backend cluster
  names. With `XDSNameSchemeV2`, discovered cluster names can look like
  `httproute/transit-dataplane/l1-l2a-catalog/rule/0`, so L1 validation must
  allow those values.
- Finish fake-MCP failure injection so the skipped MCPProxy parity cases can
  run: partial initialize failure, all initialize failure, tools/list partial
  failure, tools/list all failure, backend JSON-RPC errors, and backend
  transport or non-2xx failures.
- Add a restricted `enabled_tools` profile fixture so enabled-tool filtering and
  disabled-tool `tools/call` behavior are covered in the Envoy Gateway suite.
- Switch public tool naming from `{prefix}.{tool}` to MCPProxy-style
  `{backend}__{tool}` before treating the endpoint behavior as product-stable.
- Add buffered final-event SSE responses before testing real Streamable HTTP
  clients or claiming close MCPProxy wire compatibility.
- Add dynamic L1 profile fetching after the static environment-config topology
  is green and debuggable.
- Keep real hosted MCP servers as optional manual smoke only. CI should stay on
  deterministic fake backends.

## Status and next steps

Implemented and running in CI:

- Custom Envoy image with `libmcp-profile-gateway.so`, `libmcp-catalog-router.so`,
  `libcluster-router.so`
- L1 mcp-profile-gateway filter: profile auth, initialize fan-out, tools/list
  fan-out, tools/call routing, redacted dump
- L2 catalog-router demo app: `/mcp/s/{server}` handling with direct backend calls
- L2 cluster-router extension: initialized via dedicated init cluster, serves
  `/__cluster-router/config` dump
- Full e2e passing in CI with fake MCP backends
- All active test matrix cases from the plan covered

Next:

- Switch tool name separator from `{prefix}.{tool}` to `{prefix}__{tool}` (MCPProxy
  parity) before treating the endpoint shape as product-stable
- Add buffered final-event SSE parity before testing real Streamable HTTP clients
- Add dynamic L1 profile fetching to replace static `MCP_PROFILE_GATEWAY_CONFIG`
- Extend fake-mcp with failure injection to cover the skipped partial-failure cases
- Keep real hosted MCP servers as optional manual smoke only

## Reuse from examples

Built from:

- `examples/mcp-catalog-router`
- `examples/cluster-router`
- `integrations/tiered-router-eg`
- `integrations/cluster-router-eg`

Do not copy the recursive local Envoy egress shape from the example as the
integration architecture. In Kubernetes, make L1 profile ownership, L2
server-cluster ownership, and L2 cluster-router egress explicit.

## Manual Verification

All commands in this section should run from the repository root and outside the
Codex sandbox. The e2e suite creates the k3d cluster, installs Envoy Gateway,
applies the resources, discovers Envoy Gateway generated cluster names, patches
the dynamic-module config, and can leave the cluster running for inspection.

Run the fast checks first:

```sh
make -C integrations/mcp-profile-tiered-router-eg test
make -C integrations/mcp-profile-tiered-router-eg eg-install
```

The full topology suite builds local images by default. The Envoy image contains
the three Transit dynamic modules used by the topology:

```text
libmcp-profile-gateway.so
libmcp-catalog-router.so
libcluster-router.so
```

The demo image contains the placeholder backend, L2 catalog-router process, and
fake MCP backends. The fake backends return deterministic `tools/list` and
`tools/call` responses for `kiwi`, `aws-knowledge`, `microsoft`, and `github`.

Start the full topology and keep it alive:

```sh
KEEP_CLUSTER=1 make -C integrations/mcp-profile-tiered-router-eg e2e
```

To reuse prebuilt or published images:

```sh
IMAGE=<envoy-image-with-transit-modules> \
CONTROL_PLANE_IMAGE=<demo-image> \
SKIP_IMAGE_BUILD=1 \
KEEP_CLUSTER=1 \
make -C integrations/mcp-profile-tiered-router-eg e2e
```

The cluster name is `transit-mcp-profile-eg` and the Kubernetes context is
`k3d-transit-mcp-profile-eg`. Inspect the resource shape:

```sh
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

Expected profile-visible tool names for the first pass:

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

- L1 `/dump` does not expose `profile-key`, credential refs as usable secrets,
  raw credential envelopes, or `mcp-session-id` values.
- L2 cluster-router dumps show `route_header: x-mcp-server`.
- L2 dumps do not expose raw bearer token values such as `kiwi-token`,
  `aws-token`, `microsoft-token`, or `github-token`.

When the demo is done:

```sh
k3d cluster delete transit-mcp-profile-eg
```
