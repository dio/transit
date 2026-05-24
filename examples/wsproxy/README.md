# wsproxy

This example proxies WebSocket connections to the OpenAI Realtime API
(`wss://api.openai.com/v1/realtime`). It demonstrates how to use
`up.RegisterWithGroup` to run an embedded `net/http` server as a background
goroutine, accept a WebSocket connection from Envoy on the loopback, dial the
upstream with injected credentials, and run a bidirectional frame pump with
selective JSON tapping for usage metering.

This is the canonical transit pattern for any full-duplex protocol that requires
per-session observability. Envoy's dynamic module ABI exposes only HTTP-level
callbacks; after the 101 Switching Protocols handshake, no filter callback fires
for individual WebSocket frames. The loopback server is the only way to intercept
frames from a dynamic module without modifying Envoy itself.

---

## What it shows

- `up.RegisterWithGroup` — embedded server tied to filter config lifetime
- `up.Register` — pre-upgrade HTTP auth gate
- WebSocket upgrade routing via a STATIC loopback cluster in Envoy config
- Bidirectional frame pump with zero-copy forwarding
- Selective JSON tap: only `response.done` and `rate_limits.updated` are parsed;
  all other frames (including large audio deltas) pass through unparsed
- Session lifecycle enforcement via `context.WithDeadline`
- Envoy counters for session count, token usage, and errors

---

## Architecture

```
Client
  |
  | wss://localhost:10443/v1/realtime?model=gpt-realtime-2
  | Authorization: Bearer sk-<virtual-key>
  v
Envoy listener :10443
  |
  +-- HTTP filter: wsproxy-auth       [transit dynamic module]
  |     reads Authorization header
  |     resolves credential (stub: env OPENAI_API_KEY)
  |     sets x-wsproxy-cred: <resolved-key>
  |     sets x-wsproxy-model: <model from ?model= query param>
  |     on failure: 401 before upgrade
  |
  +-- router
        Upgrade: websocket + /v1/realtime -> cluster: wsproxy-loopback
  |
  | Envoy dials 127.0.0.1:19002 (STATIC cluster)
  v
Embedded net/http server (started by Group goroutine)
  |
  v
WSProxy.ServeHTTP
  websocket.Accept(w, r)               downstream WS from Envoy
  read x-wsproxy-cred, x-wsproxy-model
  websocket.Dial(upstream, ...)        wss://api.openai.com/v1/realtime?model=...
    + Authorization: Bearer <cred>
  SessionTap.Start()
  goroutine: downstream -> tap.FeedDownstream -> upstream
  goroutine: upstream   -> tap.FeedUpstream   -> downstream
  wait for first goroutine to return
  SessionTap.Done() -> emit usage record
```

### Why STATIC cluster, not filter-state selection

The loopback cluster is always one address (127.0.0.1:19002). There is only one
embedded proxy server regardless of provider or model. Provider and model
selection happen inside the embedded proxy in pure Go (reading x-wsproxy-model
and x-wsproxy-cred). Envoy's cluster load balancer is not involved — the cluster
is always STATIC with one endpoint.

Envoy PR #45040 (filter state in cluster LB, 1.39-dev) is not needed here and
is not used. This example works on Envoy 1.38.0.

### Why the loopback hop exists

After the 101 Switching Protocols handshake, Envoy handles WebSocket frame
forwarding as a transparent TCP tunnel. The dynamic module ABI has no per-frame
callback. The embedded server on the loopback is the only way to intercept and
tap frames without modifying Envoy.

Loopback RTT on Linux/macOS is 0.1–0.5 ms. For audio-heavy realtime sessions
where network RTT to OpenAI is 50–150 ms, this overhead is negligible.

---

## Frame tapping strategy

The `SessionTap` intercepts two server-to-client event types:

**`response.done`** — contains the usage object:
```json
{
  "type": "response.done",
  "response": {
    "usage": {
      "input_tokens": 120,
      "output_tokens": 48,
      "input_token_details":  { "audio_tokens": 80, "cached_tokens": 0 },
      "output_token_details": { "audio_tokens": 30, "text_tokens": 18 }
    }
  }
}
```
Accumulated across all `response.done` events in the session and emitted as
Envoy counters at session close.

**`rate_limits.updated`** — logged at debug level. Useful for future circuit
breaker integration.

All other frames (audio deltas, input_audio_buffer.append, session.update, etc.)
are forwarded without parsing. Fast path: `bytes.Contains(frame, []byte(`response.done`))` 
before any `json.Unmarshal`. Most frames in an audio-heavy session never reach
the JSON decoder.

---

## Envoy config overview

```
listener :10443
  upgrade_configs: [{upgrade_type: websocket}]   <- required for WS
  http_filters:
    - wsproxy-auth     <- auth gate, sets x-wsproxy-* headers
    - router

virtual_host routes:
  - match: prefix /v1/realtime + header Upgrade: websocket
    route: cluster wsproxy-loopback
           upgrade_configs: [{upgrade_type: websocket}]
  - match: prefix /v1/realtime (non-WS)
    direct_response: 400

clusters:
  - name: wsproxy-loopback
    type: STATIC
    endpoints: 127.0.0.1:19002
```

The `wsproxy-loopback` cluster port (19002) must match `loopback_addr` in the
filter config JSON. In the example it is hardcoded to 19002. In a production
deployment the EPP generates the cluster from the filter config so the operator
never writes it manually (see design doc section 11).

---

## Session lifecycle

Each WebSocket connection is a session. The session:

1. Starts when `websocket.Accept` succeeds.
2. Enforces a max duration via `context.WithDeadline` (default 3600s, matching
   OpenAI's 60-minute limit). On deadline the proxy sends WS close frame 1000
   to the downstream and closes the upstream.
3. Ends when either the upstream or downstream closes, or the deadline fires.
4. Emits a session record (duration, close reason, accumulated token counts)
   as Envoy counters.

Active sessions are tracked in a `sync.Map` keyed by session ID. `Group.Stop`
closes all active sessions gracefully before the embedded server shuts down.

---

## Envoy counters

Defined at config time via `up.RegisterWithConfig`:

```
wsproxy_sessions_total              total sessions accepted
wsproxy_sessions_active             currently active sessions (approximated as counter delta)
wsproxy_session_duration_ms         distribution (not available in 1.38 — tracked as counter sum)
wsproxy_input_tokens_total          accumulated from response.done
wsproxy_output_tokens_total         accumulated from response.done
wsproxy_audio_input_tokens_total    from input_token_details.audio_tokens
wsproxy_audio_output_tokens_total   from output_token_details.audio_tokens
wsproxy_upstream_dial_errors_total  failed upstream dials
wsproxy_upstream_error_events_total error events forwarded to downstream
wsproxy_sessions_timeout_total      sessions closed by proxy deadline
```

---

## Credential handling (this example)

This example uses the simplest possible credential model: the `OPENAI_API_KEY`
environment variable resolved at filter config creation time. The `wsproxy-auth`
filter validates that the incoming `Authorization` header is non-empty (any
`Bearer sk-...` value is accepted), then sets `x-wsproxy-cred` to the value of
`OPENAI_API_KEY`.

For a production integration, replace the credential resolution with a
`CredentialCache.Pin()` call that returns an opaque handle, and unpin after the
upstream dial. The plaintext key should only be live for the microseconds of the
upstream WebSocket handshake.

---

## Files

```
examples/wsproxy/
  README.md               this file — implementation spec
  wsproxy.go              WSProxy, SessionTap, frame pump, session lifecycle
  auth.go                 wsproxy-auth HTTP filter (pre-upgrade auth gate)
  observability.go        Envoy counter definitions
  wsproxy_test.go         unit tests: SessionTap extraction, fast-path behavior
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

`WSProxy` is created once at filter config time by `up.RegisterWithGroup`.
The `Group` goroutine starts a `net/http.Server` on `loopbackAddr`. Each
incoming HTTP request to that server is a WebSocket upgrade from Envoy.

```go
type WSProxy struct {
    loopbackAddr string
    upstreamBase string        // wss://api.openai.com/v1/realtime
    maxDuration  time.Duration
    // counters — MetricIDs defined at config time
    sessionsTotal   up.MetricID
    inputTokens     up.MetricID
    outputTokens    up.MetricID
    // ...
    active sync.Map // sessionID -> *Session
}

func (p *WSProxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
    // 1. Reject non-WS requests
    // 2. websocket.Accept(w, r, nil)
    // 3. Read x-wsproxy-cred, x-wsproxy-model from r.Header
    // 4. Build upstream URL: upstreamBase + "?model=" + model
    // 5. websocket.Dial(ctx, upstreamURL, &websocket.DialOptions{
    //        HTTPHeader: http.Header{"Authorization": {"Bearer " + cred}},
    //    })
    // 6. Create SessionTap, register session
    // 7. ctx, cancel = context.WithDeadline(r.Context(), time.Now().Add(p.maxDuration))
    // 8. errCh := make(chan error, 2)
    //    go pump(ctx, downstream, upstream, tap.FeedDownstream, errCh)
    //    go pump(ctx, upstream, downstream, tap.FeedUpstream, errCh)
    //    <-errCh; cancel()
    // 9. tap.Done(), deregister session, increment counters
}
```

`pump` reads frames from `src`, calls `tapFn(frame)` which returns the
(possibly unmodified) frame, writes to `dst`. Returns on first read or write
error, or context cancellation.

### auth.go

`wsproxy-auth` runs on the Envoy worker thread. It must not block.

```go
func authHandler(w *up.Writer, r *up.Request) {
    auth := r.Header("authorization")
    if auth == "" {
        w.SendLocalResponse(401, []byte(`{"error":"missing authorization"}`))
        return
    }
    cred := os.Getenv("OPENAI_API_KEY")
    if cred == "" {
        w.SendLocalResponse(503, []byte(`{"error":"no upstream credential configured"}`))
        return
    }
    model := r.QueryParam("model")
    if model == "" {
        model = "gpt-realtime-2"
    }
    w.SetRequestHeader("x-wsproxy-cred", cred)
    w.SetRequestHeader("x-wsproxy-model", model)
}
```

Note: `r.QueryParam` may not exist yet in `up`. If it doesn't, parse from
`r.Path` or add a small helper. Check `up/request.go` first.

### SessionTap

```go
type SessionTap struct {
    inputTokens  int64
    outputTokens int64
    audioInput   int64
    audioOutput  int64
    // no mutex: FeedUpstream and FeedDownstream each called from one goroutine
}

// FeedUpstream is called for every server->client frame.
// Returns the frame unchanged. Side effect: updates token counts.
func (t *SessionTap) FeedUpstream(frame []byte) []byte {
    if !bytes.Contains(frame, []byte(`"response.done"`)) {
        return frame  // fast path: no parse
    }
    // json.Unmarshal into minimal struct, update counts
    return frame
}

// FeedDownstream is called for every client->server frame. Currently a no-op.
func (t *SessionTap) FeedDownstream(frame []byte) []byte {
    return frame
}
```

`FeedUpstream` and `FeedDownstream` are each called from exactly one goroutine
(the respective pump goroutine). No mutex needed.

### cmd/main.go

```go
package main

import (
    _ "github.com/dio/transit/down/abi_impl"
    "github.com/dio/transit/examples/wsproxy"
)

func init() {
    wsproxy.Register()
}

func main() {}
```

`wsproxy.Register()` calls `up.RegisterWithConfig` (for counters) +
`up.RegisterWithGroup` (for the embedded server + frame pump) +
`up.Register` (for the auth filter).

---

## Unit tests (wsproxy_test.go)

- `TestSessionTap_ExtractsTokensFromResponseDone`: feed a valid `response.done`
  frame, assert input/output/audio counts are updated.
- `TestSessionTap_FastPath_SkipsNonResponseDone`: feed 1000 frames without the
  `response.done` substring, assert JSON decoder was never called (track with a
  counter or a replaced `jsonUnmarshal` var).
- `TestSessionTap_PartialResponseDone`: frame contains the string
  `"response.done"` but is malformed JSON — tap must not panic and must forward
  the frame unchanged.
- `TestSessionTap_AudioTokensAccumulated`: feed three `response.done` frames
  with different audio token counts, assert cumulative total is correct.
- `TestWSProxy_RejectsNonWebSocket`: `ServeHTTP` with a plain HTTP GET must
  return 400 without calling `websocket.Accept`.

---

## E2E tests (e2e/e2e_test.go)

The e2e suite starts a mock OpenAI upstream (in-process WebSocket server),
starts Envoy with the wsproxy module, and runs the following assertions:

- **TestWsProxy_ValidAuth_SessionEstablished**: client connects with any
  non-empty Bearer token, session is established, client receives the mock
  `session.created` event.
- **TestWsProxy_InvalidAuth_401BeforeUpgrade**: client connects with no
  Authorization header, Envoy returns 401 HTTP response (not a WS upgrade).
- **TestWsProxy_TokenUsageExtracted**: mock upstream sends one `response.done`
  event with known token counts, session closes, assert Envoy counter values
  via admin `/stats?filter=wsproxy`.
- **TestWsProxy_NonWsRequest_Returns400**: plain HTTP GET to `/v1/realtime`
  returns 400 (embedded server rejects non-WS before `websocket.Accept`).
- **TestWsProxy_SessionTimeout**: configure a 2-second max duration, assert the
  proxy closes the session with WS close code 1000 after the deadline.
- **TestWsProxy_FrameIntegrity**: send 20 frames of varying sizes (1 byte to
  12 KB) from client to mock upstream and back, assert all frames arrive intact
  and in order.
- **TestWsProxy_MultipleSessionsConcurrent**: open 5 simultaneous sessions,
  each sends/receives 10 frames, all close cleanly with no race.

Mock upstream conventions:
- In-process `net/http` server that accepts WebSocket upgrades.
- On connect: sends `{"type":"session.created","session":{"id":"test-sess-1"}}`.
- Echoes any client frame back to the client.
- On receiving a `response.create` frame: sends a `response.done` with
  `{"input_tokens":100,"output_tokens":50,"input_token_details":{"audio_tokens":60},"output_token_details":{"audio_tokens":30,"text_tokens":20}}`.

---

## Run locally

```sh
# Build the shared library
make -C examples/wsproxy build

# Start Envoy (requires OPENAI_API_KEY or mock upstream)
make -C examples/wsproxy run

# Connect a WebSocket client
wscat -c 'ws://localhost:10443/v1/realtime?model=gpt-realtime-2' \
  -H 'Authorization: Bearer sk-test'

# Unit tests
make -C examples/wsproxy test

# End-to-end tests (mock upstream, no real OpenAI key needed)
make -C examples/wsproxy e2e
```

---

## Out of scope for this example

- Real credential cache / virtual key validation (see fraser4 integration)
- Multi-provider routing (Azure, Bedrock) — the loopback provider pattern
  is the same; just extend the upstream URL selection in `WSProxy.ServeHTTP`
- Envoy Gateway / EPP injection of the loopback cluster — see design doc
  section 11 and the planned `integrations/ws-realtime-eg/` validation suite
- WebRTC (different protocol stack entirely)
- Function call interception — the proxy forwards `function_call` events
  unmodified; client is responsible for executing functions
