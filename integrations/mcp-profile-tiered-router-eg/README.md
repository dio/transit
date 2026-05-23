# MCP Profile Tiered Router Integration

Status: skeleton integration. The topology contract and placeholder Kubernetes
resource shape exist; images and integration e2e assertions are not implemented
yet. The L1 example module now exists and covers catalog forwarding, profile
`initialize` session fan-out, and profile `tools/list` fan-out, but this
integration has not wired it into runnable Envoy Gateway images.

This integration should take the proven local MCP routing semantics and make the
L1/L2 product topology explicit under Envoy Gateway.

Role mapping for this skeleton:

- L1 profile front end: `examples/mcp-profile-gateway`
- L2 catalog front end: `examples/mcp-catalog-router`
- L2 egress router: `examples/cluster-router`

This integration should not copy the local example directly. The example proves
dataplane composition in one Envoy process; this integration proves the product
boundary:

```text
client
  -> L1 Gateway
  -> /mcp/{profile-id} curated profile endpoint
  -> profile fetch, auth, policy, session, and fan-out
  -> per-server request to the owning L2 server cluster
  -> /mcp/s/{server-slug} cataloged server endpoint
  -> cluster-router with route_header: x-mcp-server
  -> concrete cluster-local MCP backend
```

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

## Proposed Topology

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

For the skeleton, L1 gets static profile JSON through a placeholder
`MCP_PROFILE_GATEWAY_PROFILE` environment value. The `mcp-profile-gateway`
module now exists. The current example implements public catalog forwarding,
profile `initialize` session fan-out, and profile `tools/list` fan-out;
profile `tools/call` is still planned.
The Envoy Gateway templates reserve the `libmcp-profile-gateway.so` module name
and path so the integration contract does not drift back into the older local
example naming.

Later, static profile JSON should be replaced by dynamic profile fetching from
an L1-owned profile service:

- L1 fetches and validates `/mcp/{profile-id}` profile configuration.
- L1 caches profile data with explicit TTL, stale-if-error, and auth failure
  behavior.
- L1 debug and dump surfaces must redact profile API keys.
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

The skeleton intentionally does not use `cluster-shard-router` for L1 catalog
server routing. The current shard router selects from request headers such as
`x-transit-tag`, not from `/mcp/s/{server-slug}`. Until a slug-aware selector
exists, the L1 profile/catalog front end should use an explicit ownership map or
explicit L2 service URLs for each cataloged server.

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
- L1 `tools/list` returns Kiwi, AWS, Microsoft, and GitHub tools from one
  profile response.
- L1 `tools/list` preserves enabled-tool filtering.
- L1 public `/mcp/s/aws-knowledge` reaches L2-A and the AWS Knowledge backend
  only.
- L1 public `/mcp/s/github` reaches L2-B and the GitHub backend only.
- L1 `tools/call github.search` reaches L2-B and the GitHub backend only.
- L1 `tools/call kiwi.search-flight` reaches L2-A and the Kiwi backend only.
- L1 `tools/call` for a disabled or unknown tool returns a controlled JSON-RPC
  error.
- L2-A direct `/mcp/s/github` is unknown or unavailable.
- L2-B direct `/mcp/s/kiwi` is unknown or unavailable.
- cluster-router debug dump shows `route_header: x-mcp-server`.
- dumps and debug endpoints do not expose profile API keys or bearer tokens.

Nice-to-have later cases:

- one backend unavailable during `tools/list` exercises partial failure policy.
- omitted catalog server prefix falls back to server ID in the egress routing
  header.
- all tools disabled returns `{"tools":[]}` instead of a JSON-RPC error.
- same server key can resolve to different concrete hosts in L2-A and L2-B.

## Non-Goals For First Pass

- No real MCP servers in CI.
- No streaming MCP transport.
- No encrypted composite session token yet.
- No public internet provider egress.
- No shared global profile store.
- No L2 curated profile fetching.
- No L1 awareness of concrete backend hosts.

## Implementation Phases

1. Use this README as the contract for the first implementation pass.
2. Add k8s templates for L1, L2-A, L2-B, and fake MCP backends. Done as a
   skeleton under `k8s/`.
3. Wire the existing `examples/mcp-profile-gateway` L1 front end into the
   integration image/resource shape. Its current implemented behavior is public
   `/mcp/s/{server-slug}` catalog forwarding, profile `initialize` session
   fan-out, and profile `tools/list` fan-out; `tools/call` routing remains a
   later example slice.
4. Use `examples/mcp-catalog-router` as the L2 server-cluster front end for
   `/mcp/s/{server-slug}`. It sets `x-mcp-server` and calls a separate
   cluster-router-patched egress cluster.
5. Build custom Envoy images that include the required Transit modules.
6. Add a minimal demo/fake-backend binary if reusing the example backend is not
   enough.
7. Add e2e tests for the required matrix.
8. Add dynamic L1 profile fetching after the static profile-env topology is
   working.
9. Only after the fake topology is stable, consider optional real MCP server
   manual smoke tests.

## Reuse From Examples

Reuse the semantics already proven by:

- `examples/mcp-catalog-router`
- `examples/cluster-router`
- `integrations/tiered-router-eg`
- `integrations/cluster-router-eg`

Do not copy the recursive local Envoy egress shape from the example as the
integration architecture. In Kubernetes, make L1 profile ownership, L2
server-cluster ownership, and L2 cluster-router egress explicit.
