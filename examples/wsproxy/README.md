# wsproxy

This example proxies WebSocket connections to the OpenAI Responses API in
WebSocket mode (`wss://api.openai.com/v1/responses`). It demonstrates how to
use `up.RegisterWithGroup` to run an embedded `net/http` server as a background
goroutine, accept a WebSocket connection from Envoy on the loopback, dial the
upstream with injected credentials, and run a bidirectional frame pump with
selective JSON tapping for usage metering.

Reference: https://openai.com/index/speeding-up-agentic-workflows-with-websockets/

---

## What problem this solves

The OpenAI Responses API (`POST /v1/responses`) normally requires a full HTTP
round-trip per turn. For agentic workflows with many sequential tool calls this
compounds: each turn pays connection overhead and re-sends enough context for
the model to continue. OpenAI's WebSocket mode eliminates that:

- One persistent WS connection handles multiple sequential `response.create`
  requests without reconnecting.
- The server maintains a connection-local in-memory cache of the most recent
  response. Continuation only needs `previous_response_id` + new input items
  (not the full history). This is compatible with `store: false` and Zero Data
  Retention.
- Result: ~40% latency reduction for workflows with 20+ tool calls.

The proxy sits between the client and OpenAI, injecting the resolved credential
at upstream dial time (the client presents a virtual key), and extracting token
usage from `response.completed` events for billing.

This is NOT the Realtime API (`/v1/realtime`). There is no audio, no VAD, no
voice. This is the standard text/tool Responses API accessed over WebSocket
transport instead of HTTP.

---

## What it shows

- `up.RegisterWithGroup` -- embedded server tied to filter config lifetime
- `up.Register` -- pre-upgrade HTTP auth gate
- WebSocket upgrade routing via a STATIC loopback cluster in Envoy config
- Bidirectional frame pump with zero-copy forwarding
- Selective JSON tap: only `response.completed` frames are parsed;
  all other frames pass through unparsed (`bytes.Contains` fast-path)
- Session lifecycle enforcement via `context.WithDeadline`
- Envoy counters for session count, token usage, and errors

---

## Protocol

### Connection

```
wss://api.openai.com/v1/responses
Authorization: Bearer {OPENAI_API_KEY}
```

Standard WebSocket upgrade. Auth is in the HTTP header at upgrade time. All
subsequent frames on the connection inherit that auth. No model in the URL.

### Client-to-server frames

Only one event type:

```json
{
  "type": "response.create",
  "model": "gpt-4.1",
  "store": false,
  "previous_response_id": "resp_abc123",
  "input": [
    {
      "type": "message",
      "role": "user",
      "content": [{"type": "input_text", "text": "next step"}]
    }
  ],
  "tools": []
}
```

`previous_response_id` is omitted on the first request. On subsequent requests
within the same session it references the immediately preceding response. The
server's connection-local cache makes this fast; full history is NOT resent.

Optional warmup (pre-warm the connection cache without generating output):

```json
{"type": "response.create", "model": "gpt-4.1", "generate": false, ...}
```

### Server-to-client frames

Same streaming events as the HTTP Responses API:

- `response.created` -- response started
- `response.output_item.added` -- output item started
- `response.output_item.delta` -- streaming content chunk
- `response.output_item.done` -- output item complete
- `response.completed` -- response done; **contains usage** (tap target)
- `response.failed` -- response errored
- `error` -- connection-level error (does not close WS)

Usage is in `response.completed`:

```json
{
  "type": "response.completed",
  "response": {
    "id": "resp_abc123",
    "usage": {
      "input_tokens": 210,
      "output_tokens": 48,
      "total_tokens": 258
    }
  }
}
```

### Session semantics

- One in-flight `response.create` at a time per connection (sequential, not
  multiplexed). For parallel requests, use separate WS connections.
- Connection limit: 60 minutes. After that the server sends:
  ```json
  {"type":"error","error":{"code":"websocket_connection_limit_reached"}}
  ```
  Client must reconnect and continue with `previous_response_id`.
- If `store: false` and the previous response has been evicted from the
  connection cache (e.g., after reconnect), the server returns
  `previous_response_not_found`. Client must re-send full context.

---

## Architecture

```
Client
  |
  | wss://proxy/v1/responses
  | Authorization: Bearer sk-<virtual-key>
  v
Envoy listener :10443
  |
  +-- HTTP filter: wsproxy-auth       [transit dynamic module]
  |     reads Authorization header
  |     resolves credential (stub: env OPENAI_API_KEY)
  |     sets x-wsproxy-cred: <resolved-key>
  |     on failure: 401 before upgrade
  |
  +-- router
        Upgrade: websocket + /v1/responses -> cluster: wsproxy-loopback
  |
  | Envoy dials 127.0.0.1:19002 (STATIC cluster)
  v
Embedded net/http server (started by Group goroutine)
  |
  v
WSProxy.ServeHTTP
  reject non-WS with 400
  websocket.Accept(w, r)               downstream WS from Envoy
  read x-wsproxy-cred from r.Header
  websocket.Dial("wss://api.openai.com/v1/responses", ...)
    + Authorization: Bearer <cred>
  SessionTap.Start()
  goroutine: downstream -> tap.FeedDownstream -> upstream
  goroutine: upstream   -> tap.FeedUpstream   -> downstream
  wait for first goroutine to return
  SessionTap.Done() -> increment counters
```

### Why STATIC cluster, not filter-state selection

The loopback cluster is always one address (127.0.0.1:19002). Provider and
upstream URL selection happen inside `WSProxy.ServeHTTP` in pure Go, reading
`x-wsproxy-cred`. Envoy's cluster LB is not involved.

Envoy PR #45040 (filter state in cluster LB, 1.39-dev) is not needed. This
example works on Envoy 1.38.0.

### Why the loopback hop exists

After the 101 Switching Protocols handshake, Envoy handles WS frame forwarding
as a transparent TCP tunnel. The dynamic module ABI has no per-frame callback.
The embedded server is the only way to intercept frames from a dynamic module
without modifying Envoy.

Loopback RTT on Linux/macOS is 0.1--0.5 ms. Against OpenAI network RTT of
50--150 ms this is negligible.

---

## Frame tapping strategy

`SessionTap` intercepts one server-to-client event type:

**`response.completed`** -- contains usage:
```json
{
  "type": "response.completed",
  "response": {
    "id": "resp_abc",
    "usage": {"input_tokens": 210, "output_tokens": 48, "total_tokens": 258}
  }
}
```

Accumulated across all `response.completed` events in the session (one per
`response.create`). Emitted as Envoy counters at session close.

All other frames are forwarded without parsing.

Fast-path: `bytes.Contains(frame, []byte(`"response.completed"`))` before any
`json.Unmarshal`. The majority of frames (`response.output_item.delta`, etc.)
never reach the JSON decoder.

Client-to-server frames (`response.create`) are forwarded without inspection.
The proxy does not validate or modify request bodies.

---

## Envoy config overview

```
listener :10443
  upgrade_configs: [{upgrade_type: websocket}]   <- required for WS
  http_filters:
    - wsproxy-auth     <- auth gate, sets x-wsproxy-cred header
    - router

virtual_host routes:
  - match: prefix /v1/responses + header Upgrade: websocket
    route: cluster wsproxy-loopback
           upgrade_configs: [{upgrade_type: websocket}]
  - match: prefix /v1/responses (non-WS)
    direct_response: 400

clusters:
  - name: wsproxy-loopback
    type: STATIC
    endpoints: 127.0.0.1:19002
```

Port 19002 must match `loopback_addr` in the filter config JSON. In a
production EG deployment the EPP generates this cluster automatically from
filter config; the operator never writes it manually (see design doc
section 11).

---

## Session lifecycle

Each WS connection is a session that carries multiple sequential turns.

1. Starts when `websocket.Accept` succeeds.
2. Enforces a max duration via `context.WithDeadline` (default 3600s, matching
   OpenAI's 60-minute limit). On deadline the proxy sends WS close 1000 to
   downstream and closes upstream.
3. Ends when upstream closes, downstream closes, or deadline fires.
4. On session end: emit accumulated token counts as Envoy counters, record
   close reason.

Active sessions tracked in a `sync.Map`. `Group.Stop` closes all active
sessions gracefully before the embedded server shuts down.

---

## Envoy counters

Defined at config time via `up.RegisterWithConfig`:

```
wsproxy_sessions_total          total WS sessions accepted
wsproxy_input_tokens_total      accumulated input_tokens from response.completed
wsproxy_output_tokens_total     accumulated output_tokens from response.completed
wsproxy_turns_total             number of response.completed events seen
wsproxy_upstream_dial_errors    upstream dial failures (credential resolved but dial failed)
wsproxy_upstream_errors         error events forwarded from upstream
wsproxy_sessions_timeout        sessions closed by proxy deadline
```

---

## Credential handling (this example)

The `wsproxy-auth` filter resolves the real credential at request time from
the `OPENAI_API_KEY` environment variable (set at filter config creation). Any
non-empty `Authorization: Bearer ...` header from the client is accepted (auth
gate validates presence, not value). The resolved key is passed as
`x-wsproxy-cred` on the loopback hop.

For a production integration, replace env-var resolution with a
`CredentialCache.Pin()` call returning an opaque handle, and unpin after the
upstream WS dial. The plaintext key should only be live during the upstream
WebSocket handshake.

---

## Files

```
examples/wsproxy/
  README.md               this file -- implementation spec
  wsproxy.go              WSProxy, SessionTap, frame pump, session lifecycle
  auth.go                 wsproxy-auth HTTP filter (pre-upgrade auth gate)
  observability.go        Envoy counter definitions
  wsproxy_test.go         unit tests for SessionTap and WSProxy
  cmd/
    main.go               .so entrypoint: blank-imports abi_impl, registers filters
  envoy.yaml              reference Envoy config for local runs
  Makefile
  e2e/
    e2e_test.go           end-to-end tests against a mock OpenAI upstream
    testdata/
      envoy.tmpl.yaml     embedded Envoy config template
```

---

## Implementation notes

### wsproxy.go

`WSProxy` created once at filter config time by `up.RegisterWithGroup`. The
`Group` goroutine starts a `net/http.Server` on `loopbackAddr`.

```go
type WSProxy struct {
    loopbackAddr string
    upstreamURL  string        // wss://api.openai.com/v1/responses
    maxDuration  time.Duration
    // MetricIDs defined at config time via up.RegisterWithConfig
    sessionsTotal up.MetricID
    inputTokens   up.MetricID
    outputTokens  up.MetricID
    turnsTotal    up.MetricID
    // ...
    active sync.Map // sessionID -> *Session
}

func (p *WSProxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
    // 1. Reject non-WS (no Upgrade header) with 400
    // 2. websocket.Accept(w, r, nil)
    // 3. cred := r.Header.Get("x-wsproxy-cred")
    // 4. websocket.Dial(ctx, p.upstreamURL, &websocket.DialOptions{
    //        HTTPHeader: http.Header{"Authorization": {"Bearer " + cred}},
    //    })
    // 5. tap := &SessionTap{}
    // 6. ctx, cancel = context.WithDeadline(r.Context(), time.Now().Add(p.maxDuration))
    // 7. errCh := make(chan error, 2)
    //    go pump(ctx, downstream, upstream, tap.FeedDownstream, errCh)
    //    go pump(ctx, upstream, downstream, tap.FeedUpstream, errCh)
    //    <-errCh; cancel(); <-errCh
    // 8. increment counters from tap.Counts()
}
```

`pump(ctx, src, dst, tapFn, errCh)` reads frames from `src`, calls
`tapFn(frame)` which returns the (unmodified) frame, writes to `dst`. Sends
first error to `errCh` and returns.

### auth.go

Runs on the Envoy worker thread. Must not block.

```go
func authHandler(w *up.Writer, r *up.Request) {
    if r.Header("authorization") == "" {
        w.SendLocalResponse(401, []byte(`{"error":"missing authorization"}`))
        return
    }
    cred := os.Getenv("OPENAI_API_KEY")
    if cred == "" {
        w.SendLocalResponse(503, []byte(`{"error":"upstream credential not configured"}`))
        return
    }
    w.SetRequestHeader("x-wsproxy-cred", cred)
}
```

### SessionTap

```go
type SessionTap struct {
    inputTokens  int64
    outputTokens int64
    turns        int64
    // FeedUpstream called from one goroutine; FeedDownstream from another.
    // No shared state between them -- no mutex needed.
}

func (t *SessionTap) FeedUpstream(frame []byte) []byte {
    if !bytes.Contains(frame, []byte(`"response.completed"`)) {
        return frame // fast path: skip parse
    }
    var ev struct {
        Type     string `json:"type"`
        Response struct {
            Usage struct {
                InputTokens  int64 `json:"input_tokens"`
                OutputTokens int64 `json:"output_tokens"`
            } `json:"usage"`
        } `json:"response"`
    }
    if err := json.Unmarshal(frame, &ev); err != nil || ev.Type != "response.completed" {
        return frame
    }
    t.inputTokens  += ev.Response.Usage.InputTokens
    t.outputTokens += ev.Response.Usage.OutputTokens
    t.turns++
    return frame
}

func (t *SessionTap) FeedDownstream(frame []byte) []byte { return frame }

func (t *SessionTap) Counts() (input, output, turns int64) {
    return t.inputTokens, t.outputTokens, t.turns
}
```

### cmd/main.go

```go
package main

import (
    _ "github.com/dio/transit/down/abi_impl"
    "github.com/dio/transit/examples/wsproxy"
)

func init() { wsproxy.Register() }
func main() {}
```

`wsproxy.Register()` calls `up.RegisterWithConfig` (counters) +
`up.RegisterWithGroup` (embedded server, uses the group) +
`up.Register` (auth filter).

---

## Unit tests (wsproxy_test.go)

- `TestSessionTap_ExtractsUsageFromResponseCompleted`: feed a valid
  `response.completed` frame, assert input/output/turns updated correctly.
- `TestSessionTap_FastPath_SkipsNonResponseCompleted`: feed 1000 frames
  without the `"response.completed"` substring, assert JSON decoder never
  called (stub `jsonUnmarshal` var to detect calls).
- `TestSessionTap_WrongType_SkipsUpdate`: frame contains
  `"response.completed"` as a string inside content but `type` field is
  `"response.output_item.delta"` -- counts must not be updated.
- `TestSessionTap_MalformedJSON_ForwardsFrame`: frame triggers the
  `bytes.Contains` check but is invalid JSON -- must not panic, must return
  frame unchanged.
- `TestSessionTap_AccumulatesAcrossMultipleTurns`: feed three
  `response.completed` frames with different token counts, assert cumulative
  totals.
- `TestWSProxy_RejectsNonWebSocket`: `ServeHTTP` with a plain HTTP GET returns
  400 without calling `websocket.Accept`.

---

## E2E tests (e2e/e2e_test.go)

In-process mock upstream: a `net/http` server that accepts WebSocket upgrades,
echoes all client frames back, and sends a `response.completed` event with
known token counts whenever it receives a `response.create` frame.

Tests:

- `TestWsProxy_ValidAuth_ConnectsToUpstream`: client connects with non-empty
  Bearer token, mock upstream receives the WS connection, exchange one frame.
- `TestWsProxy_MissingAuth_401BeforeUpgrade`: client connects with no
  Authorization header, Envoy returns HTTP 401 (no WS upgrade).
- `TestWsProxy_TokenUsageExtracted`: send one `response.create`, receive
  `response.completed` with known counts, close session, assert Envoy counter
  values via admin `/stats?filter=wsproxy`.
- `TestWsProxy_NonWsRequest_Returns400`: plain HTTP GET to `/v1/responses`
  returns 400.
- `TestWsProxy_MultipleTurns_AccumulatesTokens`: send three `response.create`
  frames, each returns `response.completed` with distinct token counts, close
  session, assert counters reflect the sum.
- `TestWsProxy_SessionTimeout`: configure 2-second max duration, assert proxy
  closes session with WS close 1000 after deadline.
- `TestWsProxy_FrameIntegrity`: send 20 frames of varying sizes (1 byte to
  12 KB) client->upstream->client, assert all arrive intact and in order.
- `TestWsProxy_ConcurrentSessions`: open 5 simultaneous sessions, each
  sends 3 `response.create` turns, all close cleanly with no data race
  (`-race` must pass).

---

## Run locally

```sh
# Build the shared library
make -C examples/wsproxy build

# Start Envoy (OPENAI_API_KEY must be set for real upstream)
OPENAI_API_KEY=sk-... make -C examples/wsproxy run

# Connect with any WS client
wscat -c 'ws://localhost:10443/v1/responses' -H 'Authorization: Bearer sk-test'
# then type: {"type":"response.create","model":"gpt-4.1","input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]}]}

# Unit tests (no Envoy needed)
make -C examples/wsproxy test

# End-to-end tests (mock upstream, no real API key needed)
make -C examples/wsproxy e2e
```

---

## Out of scope for this example

- Real credential cache / virtual key validation (fraser4 integration)
- `previous_response_id` tracking or validation -- the proxy forwards
  `response.create` frames verbatim; ID chaining is the client's responsibility
- Reconnect logic on `websocket_connection_limit_reached` -- client handles this
- `/responses/compact` integration for context window management
- Multi-provider routing (Azure, Bedrock equivalents)
- Envoy Gateway / EPP injection of the loopback cluster (see design doc
  section 11, `integrations/ws-realtime-eg/`)
- The Realtime API (`/v1/realtime`) -- separate protocol, separate example if needed
