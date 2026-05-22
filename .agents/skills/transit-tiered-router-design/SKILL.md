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

## MCP fan-out

MCP fan-out is not ordinary Cluster Extension host selection. Cluster Extension
chooses one host. Fan-out creates one logical response from several upstream
MCP servers.

Treat MCP fan-out as an L2 capability. L1 should only select the shard that
owns the user's state. The L2 shard owns the MCP profile, backend server list,
credential refs, tool namespace mapping, and partial failure policy.

Start fan-out above the cluster layer as a small Go MCP aggregator service:

- Fan out `tools/list` first because it is discovery and can be merged.
- Do not fan out `tools/call` by default. Route it to the one server that owns
  the selected namespaced tool.
- Namespace tool names as `<server-id>.<tool-name>` for the first demo.
- Treat an MCP profile as the user-facing unit: one proxy URL bundles several
  backend MCP servers under one profile auth policy.
- Keep profile auth separate from backend credentials. The proxy API key or
  OAuth token authorizes access to the profile; backend credentials are
  delivered per MCP server using the same BYOK/static/no-auth model as LLM
  provider routing.
- Redact backend credentials in active dumps, logs, and JSON-RPC errors. Dumps
  may show credential refs and whether a credential is configured.
- Keep timeout budgets, partial failure accounting, result ordering, and merge
  policy in the aggregator until the semantics are stable.
- Use Cluster Extension only to route to the shard-local aggregator or to one
  selected MCP server for non-fan-out calls.
- Profile auth is not backend auth. The proxy API key or OAuth token authorizes
  the profile URL. Backend credentials are delivered separately to each MCP
  server.
- The first aggregator endpoint should be `POST /mcp/profiles/{profile}` with
  ordinary JSON-RPC request and response bodies. Leave streaming transport,
  session resumption, and server-initiated notifications for later.
- Adopt the Envoy AI Gateway MCP proxy session lesson: initialize each backend,
  return one client-facing composite `mcp-session-id`, require that session ID
  on later POST requests, and forward the right per-backend session ID when
  calling each backend. The simple example may keep this plaintext; integration
  or production work should sign or encrypt it.
- For the first demo, accept only ordinary JSON responses from backend MCP
  servers. Preserve JSON-RPC IDs, reject unsupported notifications clearly, and
  keep SSE/streamable response handling for a later transport-focused step.
- Dot-separated names such as `github.search` are valid for the initial
  namespace policy. Keep aliases out until namespaced routing is proven.
- The active dump should include profile names, server IDs, public prefixes,
  timeout budgets, last error summaries, credential refs, and
  `credential_configured` booleans. It must not include API keys, OAuth tokens,
  bearer tokens, or raw backend error bodies.

Only consider a Transit HTTP-filter implementation after the service-level
aggregator proves the JSON-RPC semantics and Transit has an outbound subrequest
API with clear body buffering limits.

## Testing priority

Prove the topology incrementally:

1. L1-only: `a-demo` reaches upstream A and `b-demo` reaches upstream C through
   the public Gateway.
2. Physical L2: L1 sends `a` traffic to `Service/l2-a` and `b` traffic to
   `Service/l2-b`, where each service selects a separate L2 EnvoyProxy.
3. Shard-local config: the same model resolves differently in shard A and B.
4. Dynamic config: add a shard or model without changing Gateway API.
5. Provider egress: route one model to a TLS internet endpoint.
6. MCP fan-out: aggregate `tools/list` across shard-local MCP servers, then
   route `tools/call` for a namespaced tool to exactly one server.

For MCP fan-out, verify in this order:

1. Demo MCP backend servers return deterministic `tools/list` results.
2. L2 aggregator merges namespaced tools from at least two servers.
3. Missing or wrong profile auth is rejected before backend calls.
4. Each backend receives only its own configured credential.
5. Dumps redact credentials and expose only refs/configured booleans.
6. `tools/call <server>.<tool>` reaches exactly one backend.
7. One failed or slow backend is visible in the dump and does not hide healthy
   tools when the profile policy is fail-open.

Implementation order:

1. Add `examples/mcp-profile-router` to prove local MCP profile behavior.
2. Add demo MCP backend servers with deterministic `tools/list` and echoing
   `tools/call`.
3. Add an aggregator service with profile auth, composite session handling,
   credential delivery, bounded fan-out, namespaced tool merge, and
   single-server `tools/call`.
4. Add CLI commands that build JSON-RPC requests for `mcp tools-list` and
   `mcp tools-call`.
5. Add unit and in-process HTTP tests before k3d wiring.
6. Add shard-local MCP profile config under tiered-router `L2Config`.
7. Add control-plane read/update APIs and redacted dumps for MCP profiles.
8. Only after local behavior is green, wire `/mcp/profiles/*` through the
   tiered Envoy Gateway topology and add black-box k3d e2e assertions.
