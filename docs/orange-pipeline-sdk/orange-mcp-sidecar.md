# Orange MCP Streaming Sidecar

Status: design handoff for WS-G MCP integration.

This document fixes the Orange v1 architecture for MCP streamable-HTTP and
SSE-style sessions. It is a sibling of
[`orange-websocket-sidecar.md`](orange-websocket-sidecar.md), not a replacement
for the existing single-request MCP examples.

References:

- SDK plan: [`plan.md`](plan.md)
- MCP fit audit: [`mcp-fit-notes.md`](mcp-fit-notes.md)
- SDK background: [`background.md`](background.md)
- Existing Transit MCP examples:
  [`examples/mcp-profile-gateway`](../../examples/mcp-profile-gateway),
  [`examples/mcp-catalog-router`](../../examples/mcp-catalog-router),
  [`examples/mcp-profile-router`](../../examples/mcp-profile-router)
- Envoy Gateway MCP topology:
  [`integrations/mcp-profile-tiered-router-eg`](../../integrations/mcp-profile-tiered-router-eg)
- Egress-via-Envoy sidecar reference:
  [`integrations/tiered-ws-proxy-eg`](../../integrations/tiered-ws-proxy-eg)
- AI Gateway MCP proxy pattern review:
  `/Users/dio/src/dio/ai-gateway/internal/mcpproxy`

## Motivation

Orange already has MCP examples for the single-request HTTP shape: profile
lookup, fan-out, `tools/list` merge, `tools/call` routing, and L2 server
selection. That is not enough for the MCP streaming transports. Streamable
HTTP and SSE introduce a long-lived client-visible session, reconnect state,
server-to-client requests, notification streams, and backend session IDs that
must be hidden from the client.

Those responsibilities do not belong in Envoy dynamic module callbacks. Envoy
should continue to own ingress, egress, TLS, routing, access logs, stats, and
trace context. The sidecar owns MCP protocol state and translates that state
into Envoy-visible HTTP requests with bounded internal headers.

The MCP sidecar exists for the same reason as the Responses WebSocket sidecar:
when protocol framing is not visible or ergonomic in Envoy filters, the
sidecar shapes the protocol and then loops back through Envoy. It must not
become a direct provider/server egress path.

## Protocol Facts

These are MCP transport facts and compatibility constraints the Orange design
must handle.

- A client session starts with a JSON-RPC `initialize` request.
- The gateway can expose one logical MCP session while holding multiple
  backend MCP sessions.
- The public `mcp-session-id` must not expose raw backend session IDs.
- A client can send follow-up JSON-RPC requests with the public session ID.
- A client can open a GET/SSE notification stream with the public session ID
  and optional `Last-Event-Id`.
- Server-to-client requests, such as `roots/list`, `sampling/createMessage`,
  and `elicitation/create`, require a response path back to the originating
  backend.
- List-style operations such as `tools/list`, `prompts/list`, and
  `resources/list` may fan out to multiple backends and merge results.
- `tools/call` is a 1:1 operation after the gateway resolves the downstream
  tool name to the owning backend.
- Backends may return either application/json JSON-RPC messages or SSE event
  streams.
- Backend SSE streams need heartbeat/reconnect behavior that is independent of
  Envoy route timeouts.

## AI Gateway MCP Proxy Review

The package at `/Users/dio/src/dio/ai-gateway/internal/mcpproxy` is the best
local reference for the MCP protocol mechanics. It is not a drop-in dependency
for Transit.

Patterns to reuse:

- **Session envelope.** AI Gateway encodes route, subject, backend names,
  backend session IDs, last event IDs, and capability flags into an encrypted
  public session/event ID. Orange should use the same concept: stateless from
  Envoy's perspective, opaque to clients, and bound to the authenticated
  subject when present.
- **Initialize fan-out.** `initialize` creates sessions to every backend in
  the selected route, tolerates partial backend failure when at least one
  backend succeeds, merges server capabilities, and returns one public session
  ID.
- **Header-keyed backend routing.** The proxy turns MCP decisions into
  internal headers naming route, backend, request ID, method, and tool name.
  Orange should keep this shape so Envoy can route and log ordinary HTTP
  requests.
- **Tool/resource name prefixing.** Downstream names are rewritten with a
  backend prefix to avoid collisions. `tools/call` strips the prefix and sends
  the call to the owning backend only.
- **Fan-out merge helper.** The generic broadcast-and-aggregate path is the
  right WS-E.fan shape: per-leg result capture, per-leg errors, a user-supplied
  merge function, and one merged response to the client.
- **SSE parser.** The parser handles CR, LF, CRLF, UTF-8 BOM, JSON-RPC in SSE
  `data:` lines, and JSON responses that are mislabeled or non-streaming.
- **Notification multiplexing.** Backend notification streams are merged into
  one client stream. Backend event IDs are repackaged into one public
  `Last-Event-Id`.
- **Heartbeat and tool-change events.** Heartbeats keep client streams alive,
  and config changes can trigger `notifications/tools/list_changed`.
- **Server-to-client request ID rewriting.** Requests from a backend to the
  client get IDs that encode the originating backend, allowing the later
  client-to-server response to return to the right backend.
- **Forward-header extraction.** Route-level and per-backend forwarded headers
  are extracted from the original client request and applied to backend
  requests.

Patterns to adapt, not copy as-is:

- **Metrics and tracing.** AI Gateway owns its own metrics and tracer
  interfaces. Orange sidecar records must join Envoy access logs and
  Envoy-carried trace context rather than create a parallel telemetry plane.
- **Config loading.** AI Gateway implements `filterapi.ConfigReceiver`.
  Orange should read the shared `PipelineConfig[T]` snapshot from WS-A.
- **Backend addressing.** AI Gateway sends all backend traffic to its
  configured local backend listener. Orange should still do that, but the
  listener and internal headers must be named as Orange MCP internals and
  validated by an egress filter.
- **Authorization.** AI Gateway has a rich authorization layer tied to its
  filter API. Orange v1 should keep auth in existing Envoy/Orange policy
  surfaces and treat per-tool authorization as a later policy plugin unless it
  is needed for the first MCP streaming proof.
- **Response logging.** AI Gateway debug logs can include raw JSON-RPC params
  and results. Orange records must be bounded and secret-redacted by default.

Direct import or codemod decision:

- Do **not** import `/internal/mcpproxy` directly. It is an `internal` package,
  depends on `github.com/envoyproxy/ai-gateway/internal/*`, and would couple
  Transit examples to AI Gateway config, metrics, tracing, and internal header
  types.
- Do **not** subtree-copy the whole package. The package is larger than the
  Orange v1 sidecar needs and would import a second gateway architecture.
- A targeted codemod is acceptable only for narrow protocol utilities after
  the Transit types exist: session envelope encode/decode, SSE parser tests,
  capability merge tests, name prefixing tests, and server-to-client request ID
  rewriting tests. If source is copied rather than reimplemented, preserve the
  Apache-2.0 copyright header and keep it in an example-local package until a
  second Transit consumer needs it.

## Orange Architecture

Orange v1 uses an MCP sidecar plus an Envoy egress listener. The sidecar
terminates the client-visible MCP session, then emits stateless HTTP requests
back through Envoy.

```text
client
  -> Envoy inbound listener
  -> route MCP streamable-HTTP/SSE traffic to orange-mcp loopback
  -> orange-mcp sidecar
       handles initialize/session envelope
       owns backend session IDs and last-event IDs
       fans out list/initialize methods when needed
       resolves tools/call to one backend
       writes internal Orange MCP headers on each egress request
       dials Envoy egress listener
  -> Envoy egress listener
  -> orange-mcp-egress-match upstream filter
       validates and consumes sidecar headers
       writes Orange MCP decision metadata/filter-state
       strips internal headers
  -> existing MCP L2 route/cluster path
  -> mcp catalog/router/cluster extension
  -> MCP server
```

The sidecar is responsible for MCP session state. Envoy remains responsible
for provider/server egress.

### Production Internal Transport

Use the same transport rule as the WebSocket sidecar:

- Prefer UDS for local Envoy-to-sidecar hops when the deployment control plane
  can create and manage the socket path cleanly.
- TCP loopback on `127.0.0.1` is acceptable for Envoy Gateway v1, where
  listener and route resources are port-shaped.
- The sidecar-to-Envoy egress hop must be configured so it can move from TCP
  loopback to UDS later without changing MCP session semantics.

UDS does not change the trust model. The sidecar must overwrite internal
headers, the egress filter must validate and strip them, and all backend
TLS/auth/routing/access logging must still happen through Envoy.

### Inbound

The client speaks the MCP streamable-HTTP surface through the Orange ingress
listener. Envoy authenticates the client, applies normal ingress policy, and
routes only the MCP streaming path to `orange-mcp`.

The inbound filter must not select an L2 backend directly. Backend selection
depends on MCP method semantics:

- `initialize` selects the configured route/profile and fans out to all member
  backends.
- `tools/list`, `prompts/list`, and resource list methods fan out to the
  backends that support the relevant capability.
- `tools/call` resolves the downstream prefixed tool name to one backend.
- client responses to server-to-client requests route back to the backend
  encoded in the rewritten request ID.
- DELETE closes all backend sessions carried by the public session envelope.

### Sidecar

The proposed sidecar filter name is `orange-mcp`.

`orange-mcp` starts an embedded server using the WS-G sidecar lifecycle
helper: bind, readiness, graceful shutdown, session deadline, trace
propagation, and session record hook.

For each logical client session, the sidecar must:

1. Accept the inbound MCP request through Envoy.
2. On `initialize`, resolve the Orange MCP route/profile from the active
   pipeline snapshot and initialize all configured backends through Envoy.
3. Store no process-local session table required for correctness. Encode the
   route, subject, backend session IDs, capability flags, and last-event IDs in
   an encrypted public session/event envelope.
4. On follow-up requests, decode and validate the public envelope.
5. Build one or more stateless backend HTTP requests, each with internal
   Orange MCP headers.
6. Dial the local Envoy egress listener for every backend request.
7. Merge responses when the MCP method requires fan-out.
8. Stream backend notifications to the client with heartbeat and event-ID
   rewriting.
9. Emit bounded session and per-request records through the sidecar hook.

The sidecar should parse MCP JSON-RPC messages because protocol behavior
depends on method and params. It should not log raw params/results by default,
and it should not interpret provider/server-specific payloads beyond the MCP
method shapes needed for routing and merge.

### Egress

The proposed egress filter name is `orange-mcp-egress-match`.

Envoy egress receives ordinary HTTP requests from the sidecar on loopback.
The egress filter validates sidecar headers, writes the same logical MCP
decision metadata that the existing dynamic-module MCP path uses, and strips
all internal headers before the request leaves the local Envoy boundary.

The existing MCP L2 path should continue to own backend behavior:

- catalog routing and per-server route metadata;
- cluster-router host selection for concrete MCP server hosts;
- backend TLS/SNI/authority handling through the chosen WS-H transport path;
- access logs, stats, and trace propagation.

## Internal Headers

The proposed internal header names are:

- `x-orange-mcp-route`
- `x-orange-mcp-backend`
- `x-orange-mcp-method`
- `x-orange-mcp-request-id`
- `x-orange-mcp-tool`
- `x-orange-mcp-session`
- `x-orange-mcp-last-event-id`

Rules:

- These headers are internal to the sidecar-to-Envoy loopback hop.
- The sidecar must overwrite them unconditionally on every egress request.
- The egress filter must reject missing, malformed, or config-inconsistent
  values.
- The egress filter must strip every `x-orange-mcp-*` header before backend
  egress.
- Client auth, backend credentials, and bearer tokens must not be represented
  by these headers.
- Trace headers should be forwarded from inbound Envoy to sidecar to egress
  Envoy so records can be joined.

## Session Envelope

The public `mcp-session-id` should be an encrypted envelope with this logical
payload:

```text
route
subject
backend_sessions[]:
  backend
  backend_session_id
  capability_flags
```

The public `Last-Event-Id` should be an encrypted envelope with this logical
payload:

```text
backend_events[]:
  backend
  last_event_id
```

Requirements:

- Include the authenticated subject when present to reduce session hijacking
  risk.
- Encode backend session IDs and event IDs so separator characters in backend
  values cannot corrupt parsing.
- Support key rotation with primary/fallback decryptors.
- Treat decode/decrypt failures as invalid session errors.
- Do not store provider credentials in the envelope.
- Do not require a process-local session table for normal request routing.

## Method Plan

### `initialize`

- Decode params and record client capabilities.
- Resolve the Orange MCP route/profile.
- Initialize every configured backend through Envoy in parallel.
- Tolerate partial backend initialization failure when at least one backend
  succeeds.
- Fail the logical session if every backend fails.
- Merge backend capabilities with OR semantics.
- Return one public encrypted `mcp-session-id`.

### `tools/list`, `prompts/list`, Resource List Methods

- Decode the public session envelope.
- Fan out only to backends whose capability flags indicate support.
- Apply configured include/exclude selectors before exposing names.
- Prefix downstream-visible tool/resource names with the backend slug.
- Merge successful leg responses.
- Record per-leg failures and surface aggregate partial-failure policy.

### `tools/call`

- Decode the public session envelope.
- Split the downstream tool name into backend slug and original tool name.
- Validate the backend exists in the session and the tool is allowed.
- Send exactly one backend request through Envoy.
- Forward the backend response.
- Treat tool-level `isError=true` as an application failure for records, not
  as transport failure.

### Server-to-Client Requests

- For backend-originated requests that expect a client response, rewrite the
  JSON-RPC ID to encode backend name and original ID type.
- When the client later POSTs a JSON-RPC response, restore the original ID and
  forward the response to the encoded backend.
- Sign or encrypt rewritten IDs before production if the client can tamper
  with backend routing.

### GET / SSE Notification Stream

- Decode public session and optional public last-event envelope.
- Open backend notification streams through Envoy.
- Merge backend SSE events into one client stream.
- Rewrite event IDs into one public encrypted `Last-Event-Id`.
- Emit heartbeats when no backend event arrives before the heartbeat interval.
- Emit tool-list-changed notifications when the active pipeline snapshot
  changes the exposed tool set.

### DELETE

- Decode public session.
- Send DELETE through Envoy to every backend session that carries a backend
  session ID.
- Treat stateless backends and known unsupported DELETE responses as closed.
- Return success once best-effort close has completed and failures have been
  recorded.

## Records And Observability

Envoy access logs remain the source for HTTP-level ingress and egress facts:
route, cluster, response code, bytes, duration, TLS ownership, and trace IDs.
The sidecar record hook supplies MCP protocol facts that Envoy cannot derive
cleanly from stateless requests alone.

Required per-request fields:

- public session ID hash, trace ID, request ID, method, route, backend set, and
  selected backend when 1:1.
- fan-out leg count, successful legs, failed legs, and partial-failure policy.
- initialized backend session count and merged capability flags.
- downstream tool/resource name and resolved backend/original name.
- outcome, error class, started/completed time, and duration.

Records must not include raw prompts, tool arguments, JSON-RPC params/results,
provider credentials, authorization headers, raw backend session IDs, or
unredacted internal sidecar headers.

## Implementation Handoff

Build the MCP sidecar as a WS-G consumer.

Proposed components:

- `orange-mcp`: sidecar lifecycle plus MCP streamable-HTTP/SSE protocol
  handler.
- `orange-mcp-egress-match`: egress-side dynamic module filter that validates
  internal headers, writes Orange MCP decision state, and strips internal
  headers.
- `examples/mcp-streaming-sidecar`: local example proving initialize,
  tools/list, tools/call, GET/SSE, and DELETE.
- `integrations/mcp-streaming-sidecar-eg`: Envoy Gateway integration proving
  sidecar egress via Envoy.

Implementation sequence:

1. Land WS-G lifecycle helper with TCP loopback first and UDS-ready config.
2. Add session envelope encode/decode with test vectors.
3. Add SSE parser and writer with test cases ported from the AI Gateway
   behavior.
4. Add `initialize` fan-out and capability merge.
5. Add list fan-out merge for `tools/list`.
6. Add `tools/call` 1:1 routing.
7. Add GET/SSE notification merge, heartbeat, and event-ID envelope.
8. Add DELETE close.
9. Add `orange-mcp-egress-match`.
10. Add example e2e, then EG e2e.

## Test Checklist

Unit tests:

- Sidecar starts and stops with its `up.Group`.
- Readiness is not reported until the listener is bound.
- Shutdown closes listener and active streams by deadline.
- Session envelope encrypts/decrypts route, subject, backend sessions, and
  capability flags.
- Session envelope rejects malformed, tampered, wrong-key, and wrong-subject
  values.
- Last-event envelope round-trips per-backend event IDs.
- Key rotation decrypts with fallback and encrypts with primary.
- `initialize` fans out in parallel, merges capabilities, tolerates partial
  failure, and fails when every backend fails.
- Tool selectors implement deny-wins include/exclude semantics.
- Downstream prefixed names round-trip to backend and original name.
- `tools/list` merges successful legs and records failed legs.
- `tools/call` routes to exactly one backend and strips the downstream prefix.
- Server-to-client request ID rewriting preserves string, integer, and numeric
  IDs and returns responses to the originating backend.
- SSE parser handles CR, LF, CRLF, UTF-8 BOM, JSON response bodies, and SSE
  `data:` JSON-RPC messages.
- Notification stream rewrites backend event IDs into one public
  `Last-Event-Id`.
- Heartbeats flush on idle streams.
- Sidecar overwrites any client-supplied `x-orange-mcp-*` values on egress.
- `orange-mcp-egress-match` validates and strips internal headers.
- Records are bounded and redact params, results, credentials, session IDs,
  and internal headers.

End-to-end tests:

- Client `initialize` reaches the sidecar through Envoy and initializes
  backends through the Envoy egress listener.
- Provider/server egress is visible in Envoy access logs.
- Backend receives no `x-orange-mcp-*` headers.
- Public `mcp-session-id` works across follow-up POST, GET/SSE, and DELETE.
- `tools/list` returns prefixed tools from multiple backends.
- `tools/call` for a prefixed tool reaches only the owning backend.
- GET/SSE streams backend notifications and heartbeat events.
- `Last-Event-Id` reconnect resumes per-backend event IDs.
- Server-to-client `roots/list` or `sampling/createMessage` request round
  trips back to the originating backend.
- Trace/request IDs join sidecar records with Envoy ingress and egress access
  logs.
- EG integration proves the double-loopback egress-via-Envoy path.

## Out Of Scope

- Direct MCP server dial as a production Orange mode.
- Stdio bridge implementation.
- Full AI Gateway authorization parity.
- Remote durable session storage.
- Pagination correctness for every MCP list method in v1.
- Persisting notification streams across sidecar restart.
- Importing AI Gateway `internal/mcpproxy` as a library.
