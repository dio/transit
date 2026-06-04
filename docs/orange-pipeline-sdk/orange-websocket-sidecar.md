# Orange WebSocket Responses Sidecar

Status: design handoff for WS-G implementation.

This document fixes the Orange v1 architecture for OpenAI-compatible
Responses WebSocket traffic. It is not a general WebSocket proxy design. It is
the Orange-specific sidecar shape an implementation agent should build.

References:

- OpenAI WebSocket mode guide:
  https://developers.openai.com/api/docs/guides/websocket-mode
- OpenAI engineering note:
  https://openai.com/index/speeding-up-agentic-workflows-with-websockets/
- Existing Transit WebSocket proxy reference:
  [`examples/ws-proxy`](../../examples/ws-proxy)
- Egress-via-Envoy integration reference:
  [`integrations/tiered-ws-proxy-eg`](../../integrations/tiered-ws-proxy-eg)

## Motivation

Coding-agent and orchestration workflows repeatedly alternate between model
turns and tool outputs. With plain HTTP `POST /v1/responses`, each turn pays a
new request path and usually resends the conversation material needed to
continue the run. The longer the tool-call chain, the more this overhead
compounds.

OpenAI's WebSocket mode targets that exact workload. The official guide says
the Responses API supports persistent WebSocket connections to `/v1/responses`
and continuation by sending only new input items plus `previous_response_id`.
It also states that the mode is compatible with both `store=false` and Zero
Data Retention. For rollouts with 20 or more tool calls, OpenAI documents up
to roughly 40% faster end-to-end execution, caused by the persistent
connection and incremental input path.

Reported early benchmark, external motivation only: OpenAI's engineering note
reports that Cline's multi-file workflows were 39% faster after adopting
Responses WebSocket mode. Treat this as a partner-reported benchmark, not a
Transit guarantee and not a general provider guarantee. Orange should use it
only to justify building the sidecar path for long coding-agent sessions; the
Orange implementation must still be benchmarked against its own workloads.

Orange needs a sidecar because Envoy dynamic modules can inspect the HTTP
upgrade request, but after `101 Switching Protocols` Envoy forwards WebSocket
frames as a tunnel. The dynamic module ABI does not provide per-frame
callbacks. The sidecar is therefore the component that can read
`response.create` frames, extract the model, maintain session records, and
forward frames.

The sidecar does not relax the central Orange rule: all provider egress, TLS,
auth, routing, and access logging still go through Envoy.

## Protocol Facts

These are OpenAI protocol facts, not Orange decisions.

- Client connections target `wss://api.openai.com/v1/responses`.
- Each generated turn starts with a client `response.create` WebSocket event.
  The payload mirrors the normal Responses create body. Transport-specific
  HTTP fields such as `stream` and `background` are not used in WebSocket
  mode.
- The first turn may include the full input and no `previous_response_id`.
- Continuation sends another `response.create` with `previous_response_id` set
  to the prior response ID and `input` containing only new items, such as tool
  outputs and the next user message.
- `generate: false` can warm request state. It returns a response ID that a
  later generated turn can chain from with `previous_response_id`.
- The service keeps one previous-response state in a connection-local
  in-memory cache: the most recent response. That is the source of the
  low-latency continuation path.
- With `store=false` or ZDR, there is no persisted fallback if the referenced
  response ID is not in that connection-local cache; the request can fail with
  `previous_response_not_found`.
- A single WebSocket connection may receive multiple `response.create`
  messages, but they run sequentially. There is one in-flight response per
  connection and no multiplexing support.
- Connection duration is limited to 60 minutes. Clients must reconnect before
  or when that limit is reached.

## Orange Architecture

Orange v1 uses an inbound sidecar plus an Envoy egress listener. The sidecar
terminates the client WebSocket session for frame inspection, then dials back
into Envoy for provider egress.

```text
client
  -> Envoy inbound listener
  -> route /v1/responses WebSocket upgrades to orange-ws loopback
  -> orange-ws sidecar
       reads first response.create
       extracts model
       resolves provider/backend through orange.yaml
       overwrites internal Orange WS headers on the egress upgrade
       dials Envoy egress listener
  -> Envoy egress listener
  -> orange-ws-egress-match upstream filter
       validates and consumes sidecar headers
       writes Orange decision metadata/filter-state
       strips internal headers
  -> existing orange_default route/cluster
  -> existing orange-pick and orange-adapt path
  -> provider
```

The sidecar is responsible for WebSocket protocol handling and session records.
Envoy remains responsible for provider egress.

### Production Internal Transport

Production deployments should prefer Unix domain sockets (UDS) for local
Envoy-to-sidecar hops when the deployment control plane can create and manage
the socket path cleanly. UDS is a better same-pod primitive than a numbered
loopback port: it avoids port collisions, avoids accidental exposure on a
mis-bound TCP listener, and can be constrained with filesystem ownership and
mode bits.

UDS is not a correctness requirement for Orange v1. The correctness invariant
is egress-via-Envoy, not TCP versus UDS. A TCP loopback listener bound to
`127.0.0.1` is acceptable when UDS would make Envoy Gateway wiring brittle or
unportable.

Use this deployment rule:

- Self-managed Envoy or custom xDS: prefer UDS for both local hops if the
  listener and cluster resources can be emitted directly.
- Envoy Gateway v1 path: keep the sidecar-to-Envoy egress listener as TCP
  loopback unless the controller can reliably create a UDS listener. Gateway
  API listeners are port-shaped, and EnvoyPatchPolicy cannot create a new
  top-level listener from scratch.
- The inbound Envoy-to-sidecar cluster may move from `127.0.0.1:<port>` to a
  UDS endpoint earlier than the egress listener because it is only a cluster
  endpoint replacement.

UDS does not change the trust model. The sidecar must still overwrite internal
headers, `orange-ws-egress-match` must still validate and strip them, and all
provider TLS/auth/routing/access logging must still happen through Envoy.

### L1/L2 Session Affinity Concern

If Orange deploys this sidecar behind an L1-to-L2 tier, L1 must not treat L2
selection as ordinary per-connection service load balancing without an explicit
session-affinity rule.

For a single upgraded WebSocket connection, L1 chooses an L2 target during the
HTTP upgrade and then becomes an opaque TCP tunnel. That pins the live
connection to one L2 instance, but it does not solve logical session continuity
across reconnects, client retries, or pre-upgrade failures. The sidecar keeps
session records in process, and OpenAI-compatible `previous_response_id`
continuation can depend on connection-local provider state when `store=false`
or ZDR is used. Randomly landing a continuation on a different L2 instance can
lose sidecar-local session context and can make failure modes look like
provider continuation errors rather than routing errors.

The L1/L2 design therefore needs a stable hash key before production rollout.
Candidate keys are an Orange-issued WebSocket session ID, authenticated tenant
plus client session ID, or another control-plane-approved affinity key carried
on the initial upgrade. Do not rely on source IP, TCP five-tuple, or
`previous_response_id` alone: the first turn has no previous response, NATs can
collapse many clients, and reconnects change connection identity.

Required decision before implementation:

- define the session-affinity key that L1 hashes when selecting an L2 target;
- define whether affinity is to an L2 shard service or to an individual L2
  sidecar instance;
- define reconnect behavior when the preferred L2 target is unavailable;
- document whether `store=false` and ZDR sessions are allowed to reconnect to
  a different L2 target, and what client-visible error is returned if not.

Until that decision is made, the safe v1 assumption is that one logical
WebSocket session must stay on the same L2 sidecar instance for its lifetime,
including controlled reconnects.

Concrete v1 suggestion:

- Require clients, or the Orange inbound auth layer, to provide an
  `x-orange-ws-session` value on the initial upgrade. If the client does not
  provide one, L1 should mint one before L2 selection and return it to the
  client in the accepted upgrade metadata or an immediately following
  sidecar-originated session event.
- Hash `tenant_id + x-orange-ws-session` at L1 with rendezvous hashing over
  the available L2 sidecar instances in the selected shard. Hashing to an
  individual instance, not only the shard service, is the conservative default
  while sidecar session state is process-local.
- Have L1 write the selected L2 instance ID into an internal header such as
  `x-orange-l2-affinity-target` before forwarding the upgrade. L2 should record
  that value in the sidecar session summary so reconnect/debug records can show
  whether affinity was preserved.
- On reconnect, require the same `x-orange-ws-session` value. If the hashed L2
  target is healthy, route back to it. If it is unavailable, return a clear
  retryable Orange error for `store=false` and ZDR sessions instead of silently
  routing to a different instance. For persisted sessions where provider
  continuation does not depend on connection-local state, a later design may
  allow failover to a different L2 target.
- Add an L1 integration test with two L2 sidecar instances that opens a
  session, reconnects with the same affinity key, and verifies the second
  upgrade lands on the same L2 instance. Add a negative test that removes that
  instance and verifies the documented reconnect error for `store=false` or
  ZDR.

### Inbound

The client performs a normal WebSocket upgrade on `/v1/responses` through the
Orange ingress listener.

Envoy should route only OpenAI Responses WebSocket upgrades for this path to
the `orange-ws` sidecar loopback. Non-upgrade HTTP `/v1/responses` requests
continue to use the existing Orange HTTP pipeline.

The inbound HTTP filter may authenticate the client and normalize external
headers, but it must not select the provider for WebSocket sessions. Provider
selection depends on the first `response.create` frame, which is not visible to
Envoy after the upgrade.

### Sidecar

The proposed sidecar filter name is `orange-ws`.

`orange-ws` starts an embedded server using the WS-G sidecar lifecycle helper:
bind, readiness, graceful shutdown, per-session deadline, trace propagation,
and session record hook.

For each session, the sidecar must:

1. Accept the inbound client WebSocket upgrade.
2. Read the first client text frame.
3. Require a JSON object with `"type": "response.create"`.
4. Extract `model`.
5. Resolve the Orange provider, provider kind, backend model, route, auth
   profile, and egress target from the active `orange.yaml` snapshot.
6. Open a second WebSocket connection to the local Envoy egress listener, not
   to the provider.
7. Overwrite the internal Orange WS headers on that egress upgrade.
8. Forward the first frame and then pump frames bidirectionally until close,
   error, deadline, or 60-minute reconnect boundary.

The sidecar should parse only the frames needed for Orange behavior:

- client `response.create` frames for model/provider/session metadata;
- provider completion or error frames needed for usage and outcome records.

Other frames should be forwarded without full JSON parsing after cheap
type-marker checks.

### Egress

The proposed egress filter name is `orange-ws-egress-match`.

Envoy egress receives a WebSocket upgrade from the sidecar on loopback. The
upgrade should route through the existing `orange_default` route/cluster path,
so the already-planned Orange components still own provider behavior:

- `orange-pick` owns host selection and dynamic provider host/TLS identity.
- `orange-adapt` owns provider auth, schema adaptation, and backend-specific
  request/response translation where applicable.
- Envoy owns upstream TLS origination, retries/timeouts where valid for the
  upgrade path, access logging, stats, and trace propagation.

`orange-ws-egress-match` exists to bridge frame-derived sidecar decisions back
into Envoy-visible request metadata before the upstream route proceeds. It
must validate sidecar headers, write the same logical Orange decision that the
HTTP classify path would write, and strip all internal sidecar headers before
egress leaves the local Envoy boundary.

## Internal Headers

The proposed internal header names are:

- `x-orange-ws-provider`
- `x-orange-ws-kind`
- `x-orange-ws-model`
- `x-orange-ws-backend-model`

Rules:

- These headers are internal to the sidecar-to-Envoy loopback hop.
- The sidecar must overwrite them unconditionally on the egress upgrade. It
  must not preserve client-supplied values.
- `orange-ws-egress-match` must reject or local-reply if required values are
  missing, malformed, or inconsistent with the active Orange config.
- `orange-ws-egress-match` must strip these headers before routing to any
  provider.
- Provider auth must not be represented by these headers. Existing Orange auth
  handling remains responsible for provider credentials.
- If trace context is present on the client side, the sidecar should forward
  Envoy-carried trace headers on the egress upgrade so the egress path belongs
  to the same trace.

## Decision Record

1. **Egress-via-Envoy is mandatory for Orange v1.**

   The sidecar must dial a local Envoy egress listener. It must not dial
   OpenAI or any OpenAI-compatible provider directly. This preserves the Orange
   principle that provider egress, TLS, auth, routing, observability, and
   access logging are Envoy-owned.

2. **No direct-dial Orange WebSocket mode.**

   The existing `examples/ws-proxy` direct-dial mode is useful as a reference
   and break-glass demonstration, but Orange v1 must not expose it as a normal
   mode. If a future implementation requires direct dial, it needs a separate
   break-glass design record naming the missing Envoy capability and the
   operational cost.

3. **V1 supports OpenAI-compatible Responses WebSocket providers only.**

   The sidecar handles the Responses WebSocket shape: `response.create`,
   `previous_response_id`, OpenAI-compatible stream events, and compatible
   usage/error records. Realtime API, audio sessions, MCP streaming, and
   provider-specific non-OpenAI WebSocket protocols are out of scope for this
   Orange v1 sidecar.

4. **The sidecar owns frame-level usage/session records.**

   Envoy dynamic modules cannot inspect WebSocket frames after `101 Switching
   Protocols`. The sidecar must record frame-derived facts such as first model,
   response IDs, usage, close reason, deadline, and error class. Envoy access
   logs still own connection/request-level egress records; sidecar session
   records supplement them rather than replacing them.

5. **The HTTP and WebSocket Orange paths converge before provider egress.**

   WebSocket model selection happens in the sidecar, not in the HTTP classify
   filter. After `orange-ws-egress-match` writes the decision metadata, the
   path must rejoin the same Orange provider-selection and adaptation pipeline
   used by HTTP where the protocol allows it.

## Metering Plan

The WebSocket path should reuse the HTTP meter's token vocabulary, but it
cannot reuse the HTTP meter's execution point. `orange-meter` observes HTTP
response chunks before end-of-stream and writes Envoy counters plus
`orange_meter` dynamic metadata. After a WebSocket upgrade, Envoy treats the
connection as a tunnel and the dynamic module ABI cannot inspect provider
frames. For Responses WebSocket sessions, the sidecar is therefore the
frame-level source of truth for token usage.

Metering should use this split:

- Envoy access logs remain the source for connection/request-level facts:
  egress route, upstream cluster, response code for the upgrade, TLS/provider
  egress ownership, duration, bytes, and trace/request IDs.
- The sidecar metering records emitted through the session hook are the source
  for frame-level facts: requested model, resolved provider/backend model,
  response IDs, per-turn usage, `generate:false` warmups, close reason,
  deadline, and frame/protocol error class.
- Billing or quota systems must consume sidecar per-turn usage records, not
  only the aggregate WebSocket session record. A single WebSocket session can
  carry multiple sequential `response.create` turns.

### Usage Extraction

The sidecar should meter only provider-originated completion/error frames. It
must not trust client-supplied usage fields, estimate tokens from prompt text,
or parse every delta frame.

For OpenAI-compatible Responses WebSocket providers, extract usage from
completion frames that carry a response object with `usage`, such as
`response.completed`. Map the fields to the same logical `TokenUsage` shape
used by the HTTP meter:

- `input_tokens` -> `Input`
- `output_tokens` -> `Output`
- `input_tokens_details.cached_tokens` -> `CachedInput`
- `input_tokens_details.cache_creation_input_tokens` ->
  `CacheCreationInput`
- `output_tokens_details.reasoning_tokens` -> `ReasoningOutput`

If a provider emits a compatible final usage frame with different event names,
support it behind a provider-kind strategy selected from the resolved Orange
backend. Unknown provider-specific frames should be forwarded and recorded as
`usage_missing`, not interpreted heuristically.

### Record Shape

Emit one metering record per completed generated turn, plus one final session
summary. Per-turn records are the billable ledger; the session summary is for
debugging and reconciliation.

Required per-turn fields:

- `session_id`, `trace_id`, `request_id`, and egress request ID if it differs.
- `response_id` and `previous_response_id` when present.
- `model`, `provider`, `provider_kind`, `backend_model`, and route/backend ID.
- `generate` value, with `generate:false` marked as a warmup record.
- token usage fields from the shared `TokenUsage` vocabulary.
- `started_at`, `completed_at`, `duration_ms`, and `outcome`.
- `usage_status`: `reported`, `missing`, `parse_error`, or `not_applicable`.

Required session summary fields:

- session identifiers and routing decision.
- number of `response.create` turns, generated turns, warmups, and failed
  turns.
- aggregate token totals by field.
- close reason, close code, deadline state, duration, and error class.

Records must be bounded and secret-redacted. They must not include raw prompts,
tool outputs, full frames, provider credentials, authorization headers, or
unredacted internal sidecar headers.

### Emission Path

Use the WS-G session record hook as the first implementation surface. The
example can write JSON lines for e2e assertions, but the hook should be typed
so production deployments can plug in the real Orange billing/telemetry sink.

Do not pretend these usage records are ordinary `orange_meter` dynamic
metadata: the sidecar observes usage after the egress upgrade is already a
tunnel, and there is no per-frame `Writer` available to update Envoy stream
metadata. Instead:

- propagate trace and request IDs from inbound Envoy to the sidecar and then
  to the egress upgrade;
- include those IDs in both sidecar records and Envoy egress access logs;
- mirror aggregate counter names from HTTP with a WebSocket prefix if the
  sidecar helper grows an Envoy stats-compatible metric hook, for example
  `orange_ws_input_tokens`, `orange_ws_output_tokens`,
  `orange_ws_sessions`, and `orange_ws_turns`;
- keep `orange_meter` reserved for the HTTP meter namespace unless a later SDK
  primitive provides a supported way for sidecars to write Envoy metadata.

### Failure And Reconciliation Rules

If the session closes before a completion usage frame arrives, emit a failed
turn with `usage_status=missing` and zero token fields. Do not bill from an
estimate. If the provider returns an error frame with structured usage, record
the usage and mark the outcome as provider error; otherwise record the error
class without usage.

For `store=false` or ZDR continuations, do not attempt to reconstruct prior
context for metering. Meter each generated turn from the provider-reported
usage on that turn. If `previous_response_not_found` occurs, record the failed
turn and the referenced `previous_response_id`.

Future reconciliation can compare sidecar per-turn records with provider
billing exports when available. That is an offline audit path, not part of the
data plane and not a reason to add token estimation to the sidecar.

## Implementation Handoff

Build the sidecar as a WS-G consumer, not as a one-off server.

Proposed components:

- `orange-ws`: sidecar lifecycle plus WebSocket frame pump.
- `orange-ws-egress-match`: egress-side dynamic module filter that validates
  internal headers, writes Orange decision state, and strips internal headers.
- Envoy config: inbound `/v1/responses` upgrade route to `orange-ws` loopback;
  egress listener route through `orange_default`; `upgrade_configs:
  [{upgrade_type: websocket}]` on both inbound and egress HTTP connection
  managers and route entries; route `timeout: 0s` for WebSocket tunnels.
- Production loopback transport: support TCP loopback first and design the
  sidecar dial/listen configuration so UDS can be selected where the control
  plane supports it. Do not make UDS mandatory for Envoy Gateway v1.

Required config behavior:

- The sidecar reads the same active Orange pipeline config snapshot as the
  HTTP classify/translate/hostpick path.
- Provider resolution uses the client-requested `model` from the first
  `response.create` frame.
- The sidecar writes provider, kind, requested model, and backend model into
  internal headers only on the sidecar-to-Envoy egress upgrade.
- `orange-ws-egress-match` treats those headers as hints that must still match
  active config. They are not a trust boundary.
- `orange-ws-egress-match` strips all `x-orange-ws-*` headers before provider
  egress.
- Auth stays in the existing Orange auth/adapt path. Do not inject provider
  authorization in the sidecar.

Required runtime behavior:

- One client WebSocket session maps to one egress WebSocket session.
- The sidecar does not multiplex multiple OpenAI runs over one upstream
  connection.
- The sidecar respects the OpenAI 60-minute connection limit with a session
  deadline and clear close/error record.
- Backpressure must be handled by the two pump goroutines. Avoid shared mutable
  frame state between directions.
- Session records must be bounded and secret-redacted. They may include model,
  provider, backend model, response IDs, token usage, close reason, duration,
  and error class. They must not include raw prompts, tool outputs, provider
  credentials, or full frames.

## Test Checklist

Unit tests:

- `orange-ws` starts and stops with its `up.Group`.
- Readiness is not reported until the sidecar listener is bound.
- Shutdown closes the listener and active sessions by deadline.
- First-frame parsing accepts valid `response.create` and extracts `model`.
- First-frame parsing rejects non-JSON, wrong event type, missing model, and
  unsupported model.
- Provider lookup maps requested model to provider, kind, and backend model
  from a test `orange.yaml` snapshot.
- Sidecar overwrites any client-supplied `x-orange-ws-*` values on egress.
- Sidecar forwards trace headers on the egress upgrade.
- TCP and UDS internal transport config both build the expected listener/dial
  settings when UDS is enabled in unit scope.
- Session record hook fires on normal close, upstream close, parse error,
  provider lookup error, and deadline close.
- Frame tapping parses only `response.create` and completion/error usage
  frames; unrelated delta frames use the cheap forward path.
- Usage extraction maps `response.completed.response.usage` into the shared
  HTTP meter `TokenUsage` field vocabulary.
- Generated turns emit one per-turn metering record with `usage_status=reported`.
- `generate:false` turns emit warmup records and do not inflate generated-turn
  token totals unless the provider reports usage.
- Missing final usage, malformed usage, provider error, and early close produce
  bounded records with the correct `usage_status` and error class.
- Session summary aggregates only completed per-turn usage records and redacts
  prompts, tool outputs, full frames, credentials, and internal headers.
- `orange-ws-egress-match` accepts valid internal headers and writes the same
  decision shape as the HTTP classify path.
- `orange-ws-egress-match` rejects missing or config-inconsistent internal
  headers.
- `orange-ws-egress-match` strips every `x-orange-ws-*` header before egress.

End-to-end tests:

- Client WebSocket upgrade to `/v1/responses` reaches the sidecar.
- Sidecar reads first `response.create`, resolves the provider, and dials the
  Envoy egress listener.
- Envoy egress runs `orange-ws-egress-match`, then routes through
  `orange_default`.
- Existing Orange provider auth is applied by the Envoy egress path, not by
  the sidecar.
- Existing Orange host selection/TLS path is used for provider egress.
- Provider receives no `x-orange-ws-*` headers.
- Multiple sequential `response.create` frames on one connection are forwarded
  in order.
- Attempted parallel/multiplexed use is rejected or serialized according to
  the OpenAI one-in-flight rule.
- `store=false` plus `previous_response_id` continuation is forwarded without
  expanding full prior context in the sidecar.
- `generate:false` warmup is forwarded and its returned response ID can be
  chained by a later generated turn.
- Connection-limit or deadline close produces a bounded sidecar session record.
- Envoy access logs show the egress request; sidecar session records show
  frame-derived usage/outcome.
- Sidecar per-turn records and Envoy egress access logs share trace/request
  identifiers so billing records can be joined to Envoy-owned egress logs.
- A multi-turn WebSocket session emits multiple per-turn metering records plus
  one aggregate session summary.
- A session that closes before provider completion emits a non-billable
  `usage_status=missing` turn record.
- The integration equivalent of `tiered-ws-proxy-eg` proves the double-loopback
  egress-via-Envoy path.

## Out Of Scope

- Direct provider dial as a production Orange mode.
- Non-OpenAI-compatible WebSocket providers.
- Realtime API (`/v1/realtime`), audio, voice activity detection, or SIP.
- MCP streaming sidecar design. That remains a WS-G sibling, not this Orange
  Responses sidecar.
- Full request/response body translation inside the sidecar. Translation stays
  in the Orange adapt layer when it is possible on the Envoy-visible path.
