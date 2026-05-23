# MCP Profile Router

Status: rewrite plan. The existing implementation in this example should be
replaced rather than extended.

This example should prove a concrete MCP profile routing shape:

- `/mcp/s/{server-slug}` is a cataloged single MCP server.
- `/mcp/{profile-id}` is a curated MCP profile made from one or more cataloged
  servers.
- A profile owns the enabled tool set, profile authentication, backend
  credential policy, timeout policy, and composite session policy.
- `tools/list` on a profile fans out to its member servers and returns only the
  enabled tools.
- `tools/call` on a profile routes to exactly one owning server and rejects
  unknown or disabled tools before reaching any backend.
- Backend sessions are carried in one client-facing `mcp-session-id` instead of
  stored in proxy process memory.

The goal is a small local example that is useful before a larger tiered router
or Envoy Gateway integration. Automated tests should use deterministic fake MCP
servers. Optional manual commands can point at real MCP servers.

## Endpoint Model

### Cataloged Server

```text
POST /mcp/s/{server-slug}
```

This is the primitive endpoint for one MCP server, for example:

```text
https://proxy.staging.transit.so/mcp/s/aws-knowledge
```

It behaves like a direct MCP server:

- `initialize` opens one backend MCP session.
- `tools/list` returns that server's tools.
- `tools/call` forwards directly to that server.
- No profile fan-out or profile tool filtering is applied.

### Profile

```text
POST /mcp/{profile-id}
```

This is a curated collection of cataloged servers, for example:

```text
https://proxy.staging.transit.so/mcp/9b3f7d0a80c4aa6d-67261ca9ea3dadb2
```

A profile is not a different backend protocol. It is configuration over
cataloged MCP servers:

```text
/mcp/{profile-id}
  = collection of /mcp/s/{server-slug} members
    + enabled tool set
    + profile auth/session policy
    + backend credential policy
    + timeout and partial failure policy
```

For the concrete local demo, model the profile shown in the product UI:

```text
profile id:   9b3f7d0a80c4aa6d-67261ca9ea3dadb2
profile name: kiwi
members:
  - kiwi-flight-search
  - aws-knowledge
```

The AWS catalog endpoint remains independently addressable as:

```text
/mcp/s/aws-knowledge
```

## Request Semantics

### initialize on a profile

The profile router should:

1. Authenticate the client against the profile API key.
2. Initialize each enabled member backend with that backend's configured
   credential.
3. Collect each backend's `mcp-session-id`.
4. Return one client-facing composite `mcp-session-id`.

The client should never see backend credentials or raw backend session headers.

### tools/list on a profile

The profile router should:

1. Decode and validate the composite session.
2. Fan out `tools/list` to the profile member servers.
3. Forward the correct per-backend `mcp-session-id` to each backend.
4. Merge returned tools.
5. Return only tools enabled by the profile.

Tool names should be stable and unambiguous. The initial example can use a
simple prefix convention such as:

```text
aws-knowledge.aws____read_documentation
kiwi-flight-search.search-flight
```

The exact separator can change during implementation, but the tests should prove
that two backends cannot claim the same profile-visible tool name silently.

### tools/call on a profile

The profile router should:

1. Decode and validate the composite session.
2. Resolve the requested profile-visible tool to exactly one member server.
3. Reject disabled or unknown tools without calling any backend.
4. Strip the profile namespace before forwarding if the backend expects the
   native tool name.
5. Forward only to the owning backend with that backend's session ID and
   credential.

This is fan-out for discovery, not fan-out for every tool call.

## Stateless Composite Session

The final model should not store profile sessions in server-side process memory.
The session token is passed between request and response.

For the first local implementation, use a simple encoded token. A minimal shape:

```json
{
  "profile_id": "9b3f7d0a80c4aa6d-67261ca9ea3dadb2",
  "subject": "demo-client",
  "iat": 1779490000,
  "exp": 1779493600,
  "backend_sessions": {
    "kiwi-flight-search": "kiwi-session-id",
    "aws-knowledge": "aws-session-id"
  }
}
```

Base64 JSON is acceptable for the first local proof, but HMAC signing is a small
and useful next step. If backend session IDs are sensitive, encrypt the payload
rather than only signing it.

The production-shaped token should include:

- `kid`, so keys can rotate.
- `iat` and `exp`, so stale sessions expire.
- profile binding, so a token for one profile cannot be replayed on another.
- subject or tenant binding, so one user's profile token cannot be replayed by
  another user.
- backend session map, keyed by catalog server slug.

## L1 and L2 Placement Hypothesis

This example is an L2-style profile router. It owns profile contents and knows
how to talk to the profile's member MCP servers.

In a larger deployment:

- L1 chooses the right L2 shard for a tenant, profile, session, or placement
  policy.
- L2 owns the MCP profile, enabled tools, backend credentials, tool namespace,
  and composite session state.
- A cluster router or Transit Cluster Extension can choose one concrete host for
  each outbound subrequest.

The cluster router is not the profile fan-out engine. It resolves the target for
one outbound call. The L2 profile router decides when fan-out is needed and which
catalog server owns a tool call.

## Data Model Sketch

```go
type CatalogServer struct {
    Slug       string
    URL        string
    Credential string
}

type Profile struct {
    ID      string
    Name    string
    APIKey  string
    Servers []ProfileServer
}

type ProfileServer struct {
    ServerSlug   string
    Prefix       string
    EnabledTools map[string]bool
    Credential   string
}
```

`Credential` is fine for the local example. A production-shaped version should
use a credential reference and resolve it outside dumps and logs.

## Implementation Plan

### Phase 1: Pure Go HTTP proof

Replace the current aggregator behavior first, without Envoy:

- Remove the old `/mcp/profiles/{profile}` route.
- Add `/mcp/{profile-id}` for profile requests.
- Add `/mcp/s/{server-slug}` for cataloged single-server requests.
- Remove server-side profile session storage.
- Encode the backend session map into the returned `mcp-session-id`.
- Decode the composite session on later requests.
- Keep `/dump` for tests, but redact credentials and session payloads.

The fake backend server should remain deterministic and easy to assert against.

### Phase 2: CLI and manual flow

Update the demo CLI around the new URLs:

```sh
./examples/mcp-profile-router/dist/mcp-profile-router tools-list \
  --url http://127.0.0.1:8080/mcp/9b3f7d0a80c4aa6d-67261ca9ea3dadb2 \
  --api-key profile-key

./examples/mcp-profile-router/dist/mcp-profile-router tools-list \
  --url http://127.0.0.1:8080/mcp/s/aws-knowledge
```

Prefer `--url` over `--profile-url` because the same client command should work
for both endpoint classes.

### Phase 3: Transit dynamic module wrapper

Once pure Go behavior is stable, wire the same handler through the Transit
dynamic module:

- Profile endpoint handles client requests.
- Backend egress uses `HTTPCallout`.
- Required callout headers include `:method`, `:path`, and `host`.
- The hot path can later use a small builder API for readability, but the
  builder must keep `host` explicit because Envoy validates it.

### Phase 4: Envoy e2e

Use the example e2e harness to prove:

- profile `initialize` initializes both fake backends.
- profile `tools/list` merges enabled tools from both backends.
- disabled backend tools are absent.
- disabled tool calls are rejected without backend traffic.
- profile `tools/call` reaches only the owning backend.
- catalog endpoint `/mcp/s/aws-knowledge` reaches only AWS.
- backend credentials are forwarded to backends and redacted from dumps.
- tampered composite session is rejected once HMAC signing exists.

### Cluster Router PoC

The e2e suite also includes a combined `mcp-profile-router` + `cluster-router`
PoC. It builds `libmcp-profile-router-combo.so`, which registers:

- the MCP profile HTTP filter.
- the cluster-router Cluster Extension.
- the cluster-router upstream request filter.
- the cluster-router debug filter.

The topology is intentionally parameterized:

- profile membership is passed through `MCP_PROFILE_ROUTER_PROFILE`.
- physical backend placement is passed through the Envoy template as
  `ClusterConfigJSON`.
- each profile backend points at a local Envoy egress listener.
- the profile router adds `x-mcp-server: <server-prefix>` on outbound MCP
  backend calls.
- cluster-router maps that model/server key to a concrete backend host and
  injects the backend credential.

This proves that `/mcp/{profile-id}` can own MCP fan-out semantics while
cluster-router owns physical backend placement.

## Test Plan

Start with pure Go tests before Envoy:

- `TestProfileInitializeReturnsCompositeSession`
- `TestProfileToolsListReturnsEnabledToolsOnly`
- `TestProfileToolsCallRoutesToOwningBackend`
- `TestProfileToolsCallRejectsDisabledTool`
- `TestCatalogEndpointListsSingleServerTools`
- `TestCatalogEndpointCallsSingleServerTool`
- `TestInvalidProfileAuthReachesNoBackend`
- `TestDumpRedactsCredentials`
- `TestTamperedCompositeSessionRejected` once signing is added.

The e2e suite should reuse the same semantic cases, but only after the pure Go
tests make the contract precise.

## Optional Manual Real MCP Servers

Automated tests should not depend on real hosted MCP servers. For manual smoke
testing, allow the aggregator to point at real or staged servers:

```sh
./examples/mcp-profile-router/dist/mcp-profile-router aggregator \
  --addr :8080 \
  --profile-id 9b3f7d0a80c4aa6d-67261ca9ea3dadb2 \
  --profile-name kiwi \
  --api-key profile-key \
  --server kiwi-flight-search=https://proxy.staging.transit.so/mcp/s/kiwi-flight-search=kiwi-flight-search=$KIWI_TOKEN \
  --server aws-knowledge=https://proxy.staging.transit.so/mcp/s/aws-knowledge=aws-knowledge= \
  --server github=https://proxy.staging.transit.so/mcp/s/github=github=$GITHUB_TOKEN
```

The exact `--server` flag format can be improved during implementation. The
important constraints are:

- multiple servers are separated with commas.
- optional enabled tools use `;tools=tool_a|tool_b` after the credential.
- `=` characters inside credentials are preserved.
- credentials come from environment variables or local secret stores.
- tokens are never committed.
- manual output may vary as real server catalogs change.
- CI remains fake-server only.

## Non-Goals

- No k3d or Envoy Gateway in the first rewrite.
- No production secret manager integration.
- No streaming MCP transport in the first pass.
- No server-side profile session store.
- No real hosted MCP dependency in automated tests.

## Migration Notes

Throw away these current-example concepts:

- `/mcp/profiles/{profile}` URL shape.
- in-memory `Aggregator.sessions` as the primary session model.
- profile names as the externally stable routing key.

Keep these concepts:

- small fake backend MCP servers.
- a CLI that can exercise initialize, `tools/list`, and `tools/call`.
- e2e proof that backend egress happens through Envoy once the dynamic module is
  wired back in.
