# orange/responsesws

WebSocket sidecar for the OpenAI Realtime Responses API (`/v1/responses`) in the
Orange pipeline.

Envoy dynamic modules cannot hold long-lived, bidirectional WebSocket connections
directly. Instead, Orange runs a Go HTTP server as a sidecar: it accepts WebSocket
upgrades, reads the opening `response.create` frame to identify the model and
provider, then dials Envoy's egress listener with internal routing headers and
proxies the full duplex tunnel.

## Architecture

```
client ──WS upgrade──► Envoy (port 8080)
                          upgrade_configs → orange-responsesws-meter filter
                          route /v1/responses → orange-responsesws-loopback cluster
                                                   └─ sidecar HTTP server (127.0.0.1:0 or UDS)
                                                        ├─ reads response.create frame → resolves model/provider
                                                        ├─ dials orange-responsesws-egress (port 10003) ──WS──►
                                                        │    └─ orange-responsesws-egress-match filter
                                                        │         └─ orange-responsesws-default cluster
                                                        │              └─ real backend (HTTPS + WS)
                                                        └─ bidirectional frame pump (client ↔ backend)
```

The sidecar **is not a separate process**. It runs as a goroutine inside the Envoy
dynamic-module process. The `orange-responsesws-loopback` cluster extension owns its
full lifecycle: bind on `Init`, start serving on `ServerInitialized`, stop on
`Shutdown`.

## Sub-packages

| Package | Role |
|---|---|
| `responsesws` | Handler, frame parsing, bidirectional pump, meter bridge; exports `NewSidecar` |
| `responsesws/loopback` | Cluster extension that wires the sidecar into Envoy's lifecycle |

## Configuration

### Environment variables

| Variable | Default | Description |
|---|---|---|
| `ORANGE_RESPONSESWS_LISTEN_ADDR` | `127.0.0.1:0` | Sidecar bind address. Ephemeral TCP by default; see UDS below |
| `ORANGE_RESPONSESWS_EGRESS_URL` | `ws://127.0.0.1:10003` | URL the sidecar dials for egress WebSocket upgrades. Accepts `ws+unix://` for UDS egress |
| `ORANGE_RESPONSESWS_FIRST_FRAME_TIMEOUT` | `10m` | How long to wait for the client's first `response.create` frame. Codex may prewarm connections before the user submits a prompt |

`ORANGE_RESPONSESWS_LISTEN_ADDR` accepts:

- **Ephemeral TCP** — `127.0.0.1:0` (default): the OS assigns a free port; the
  cluster reads it from `sc.ListenAddr()` and publishes it as the cluster's single
  host.
- **Fixed TCP** — `127.0.0.1:10002`: predictable port, useful for debugging.
- **Unix domain socket** — `unix:///tmp/orange-responsesws.sock` or
  `unix:///run/orange/responsesws.sock`: see UDS section.

`ORANGE_RESPONSESWS_EGRESS_URL` accepts `ws://` (TCP) or `ws+unix://` (UDS egress).
`dialOptionsForEgress` in `responsesws.go` handles both: the UDS variant injects a
custom `http.Transport` that dials the socket path, passing a plain `ws://localhost`
URL to the WebSocket library.

### Cluster-config override

The `orange-responsesws-loopback` cluster accepts an optional JSON config block in
`envoy.tmpl.yaml`. All fields are optional.

```yaml
- name: orange-responsesws-loopback
  connect_timeout: 5s
  lb_policy: CLUSTER_PROVIDED
  cluster_type:
    name: envoy.clusters.dynamic_modules
    typed_config:
      "@type": type.googleapis.com/envoy.extensions.clusters.dynamic_modules.v3.ClusterConfig
      dynamic_module_config:
        name: orange
      cluster_name: orange-responsesws-loopback
      cluster_config:
        "@type": type.googleapis.com/google.protobuf.StringValue
        value: '{"listen_addr":"unix:///run/orange/responsesws.sock"}'
```

**Supported cluster-config fields:**

| Field | Type | Description |
|---|---|---|
| `listen_addr` | string | Same syntax as `ORANGE_RESPONSESWS_LISTEN_ADDR`; overrides the env var |

`egress_url` and `first_frame_timeout` are env-only — not wired through cluster
config by design, to keep the YAML surface minimal.

## Unix domain socket (UDS)

UDS removes the TCP stack from the loopback path. Because the sidecar and Envoy
run in the same process and container, there is no practical latency difference for
most workloads, but UDS avoids port allocation and is slightly cleaner.

### Standalone / Docker

```bash
# env var
ORANGE_RESPONSESWS_LISTEN_ADDR=unix:///tmp/orange-responsesws.sock ./envoy -c envoy.yaml

# or cluster_config in envoy.tmpl.yaml (takes precedence)
value: '{"listen_addr":"unix:///tmp/orange-responsesws.sock"}'
```

The socket file is created at startup and removed by `Stop()` on shutdown, so
restarts do not leave stale socket files.

### Kubernetes

With `readOnlyRootFilesystem: true`, `/tmp` is read-only. Mount an `emptyDir`
volume at the socket directory:

```yaml
volumes:
  - name: orange-sockets
    emptyDir: {}
containers:
  - name: envoy
    volumeMounts:
      - name: orange-sockets
        mountPath: /run/orange
```

Then point the sidecar at a path inside that mount:

```yaml
# cluster_config in envoy.tmpl.yaml
value: '{"listen_addr":"unix:///run/orange/responsesws.sock"}'
```

```yaml
# or as a container env var
env:
  - name: ORANGE_RESPONSESWS_LISTEN_ADDR
    value: unix:///run/orange/responsesws.sock
```

The cluster config approach keeps the address co-located with the cluster
definition and avoids a separate env var.

## Filters

### orange-responsesws-meter (stays in upgrade_configs — do not move)

This filter bridges sidecar-accumulated token usage into the inbound access log.
It lives in `upgrade_configs.filters` on the inbound HCM, not in `http_filters`.

**Why it is needed:** `orange-meter` (the upstream HTTP filter on `orange_default`)
works by inspecting response bodies via terminal HTTP body events. For
`/v1/responses` over WebSocket:

- The inbound side terminates with `101 Switching Protocols` — no response body,
  no terminal event, so `orange-meter` never fires on the inbound path.
- The sidecar dials `orange-responsesws-egress`, which routes to
  `orange-responsesws-default`. That cluster intentionally omits upstream HTTP
  filters — after a 101 the upstream body callbacks never fire on tunnel frames.

`orange-responsesws-meter` plugs this gap:

1. The sidecar parses WebSocket frames, accumulates token counts per request ID,
   and publishes a `responseswsMeterRecord` keyed by `x-request-id`
   (`meter_bridge.go`).
2. The meter filter, on `EndStream` of the upgrade response, waits up to 250 ms
   for the sidecar's record, then writes `orange:model`, `orange:upstream`,
   `orange:provider`, `orange:backend_model`, `orange:endpoint`, and calls
   `meter.EmitUsage` — so the file access log sees the same token fields a
   normal HTTP request would.

**General rule:** if a sidecar's transport terminates as a WebSocket upgrade, a
meter-bridge filter on the inbound upgrade path is required. If it terminates as
ordinary HTTP (like MCP), it is not.

### orange-responsesws-egress-match

Real HTTP filter on the `orange-responsesws-egress` listener. It validates and
strips `x-orange-responsesws-*` internal headers written by the sidecar, injects
backend auth, and sets upstream state so `orange-pick` selects the correct backend.

There is no `orange-responsesws` HTTP filter on the inbound listener. The sidecar
lifecycle was previously wired through a no-op filter; it is now owned entirely by
the `orange-responsesws-loopback` cluster extension.

## WebSocket warmup

Codex prewarms WebSocket connections before the user submits a prompt. The sidecar
handles warmup locally without dialling the backend:

- A `response.create` frame with `"generate": false` is a warmup frame.
- The sidecar replies with a synthetic `response.completed` carrying a
  `resp_orange_responsesws_warmup_<sessionID>` response ID and zero token counts.
- Subsequent frames with `"previous_response_id"` matching that prefix have the
  field stripped before forwarding, so the backend never sees the local warmup ID.

## Session lifecycle

Each WebSocket connection is one session:

1. **Accept** — Envoy upgrade → sidecar.
2. **First frame** — read with `ORANGE_RESPONSESWS_FIRST_FRAME_TIMEOUT` deadline.
3. **Provider lookup** — parse `response.create`, resolve model → provider from the
   active `orange.yaml` snapshot.
4. **Egress dial** — dial `ORANGE_RESPONSESWS_EGRESS_URL` with internal routing
   headers; backend performs the upstream upgrade.
5. **Pump** — bidirectional frame relay; `FrameTap` accumulates per-turn token
   usage.
6. **Teardown** — first pump error classifies the close reason; `FrameTap` flushes
   any in-flight turn; meter bridge publishes the summary.

Maximum session duration is 60 minutes (OpenAI-documented limit). A per-session
context deadline enforces this regardless of client behaviour.

## Failure behaviour

If the sidecar fails to bind (`Listen()` returns an error), the cluster calls
`PreInitComplete()` without registering any hosts. Routes to `/v1/responses` return
503 until the process is restarted. This matches `pick`'s DNS-failure behaviour.
