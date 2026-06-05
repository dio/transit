# orange/mcp

MCP streamable-HTTP/SSE sidecar for the Orange pipeline.

Orange cannot run an MCP server inside an Envoy dynamic module (no goroutines, no
long-lived state), so instead it runs a minimal Go HTTP server as a sidecar and
routes `/mcp` traffic to it via a loopback cluster.

## Architecture

```
client → Envoy (port 8080)
           └─ route /mcp → orange-mcp-loopback cluster
                              └─ sidecar HTTP server (127.0.0.1:0 or UDS)
                                   ├─ authenticates session via ORANGE_MCP_SESSION_KEYS
                                   └─ proxies tool calls → orange-mcp-egress listener (port 10005)
                                                              └─ orange-mcp-egress-match filter
                                                                   └─ orange_default cluster
                                                                        └─ real backend (HTTPS)
```

The sidecar **is not a separate process**. It runs as a goroutine inside the Envoy
dynamic-module process. The `orange-mcp-loopback` cluster extension owns its full
lifecycle: bind on `Init`, start serving on `ServerInitialized`, stop on `Shutdown`.

## Sub-packages

| Package | Role |
|---|---|
| `mcp` | Handler, session crypto, SSE, JSON-RPC logic; exports `NewSidecar` |
| `mcp/loopback` | Cluster extension that wires the sidecar into Envoy's lifecycle |

## Configuration

### Environment variables

| Variable | Default | Description |
|---|---|---|
| `ORANGE_MCP_LISTEN_ADDR` | `127.0.0.1:0` | Sidecar bind address. Ephemeral TCP by default; see UDS below |
| `ORANGE_MCP_EGRESS_URL` | `http://127.0.0.1:10005` | URL the sidecar dials to reach Envoy's MCP egress listener |
| `ORANGE_MCP_SESSION_KEYS` | dev key (warns) | Comma-separated session-signing keys; first is primary. Use `orange-generated` for an ephemeral key (invalid after restart) |

`ORANGE_MCP_LISTEN_ADDR` accepts:

- **Ephemeral TCP** — `127.0.0.1:0` (default): the OS assigns a free port; the
  cluster reads it from `sc.ListenAddr()` and publishes it as the cluster's single
  host, so the route always reaches the sidecar regardless of which port was chosen.
- **Fixed TCP** — `127.0.0.1:10004`: useful when you need a predictable port for
  external tooling.
- **Unix domain socket** — `unix:///tmp/orange-mcp.sock` or
  `unix:///run/orange/mcp.sock`: eliminates TCP overhead; see UDS section below.

### Cluster-config override

The `orange-mcp-loopback` cluster accepts an optional JSON config block in
`envoy.tmpl.yaml`. All fields are optional; omit to rely on env vars / defaults.

```yaml
- name: orange-mcp-loopback
  connect_timeout: 5s
  lb_policy: CLUSTER_PROVIDED
  cluster_type:
    name: envoy.clusters.dynamic_modules
    typed_config:
      "@type": type.googleapis.com/envoy.extensions.clusters.dynamic_modules.v3.ClusterConfig
      dynamic_module_config:
        name: orange
      cluster_name: orange-mcp-loopback
      cluster_config:
        "@type": type.googleapis.com/google.protobuf.StringValue
        value: '{"listen_addr":"unix:///run/orange/mcp.sock"}'
```

The cluster config takes precedence over the env var. Setting it in YAML is
convenient when you want the address co-located with the route that uses it.

**Supported cluster-config fields:**

| Field | Type | Description |
|---|---|---|
| `listen_addr` | string | Same syntax as `ORANGE_MCP_LISTEN_ADDR`; overrides the env var |

`egress_url` and other sidecar options are env-only for now — they are not
wired through the cluster config intentionally, to keep the YAML surface small.

## Unix domain socket (UDS)

UDS removes the TCP stack from the loopback path. Since both sides (Envoy cluster
and sidecar) run inside the same process and the same container, there is no
observability difference and latency is marginally lower.

### Standalone / Docker

Set the address on either side — env var or cluster config — to a writable path:

```bash
# env var
ORANGE_MCP_LISTEN_ADDR=unix:///tmp/orange-mcp.sock ./envoy -c envoy.yaml

# or cluster_config in envoy.tmpl.yaml (takes precedence)
value: '{"listen_addr":"unix:///tmp/orange-mcp.sock"}'
```

`/tmp` is writable in most environments. The socket file is created by the sidecar
at startup and removed by `Stop()` on shutdown, so restarts do not leave stale files.

### Kubernetes

With `readOnlyRootFilesystem: true`, `/tmp` is not writable. Mount a dedicated
`emptyDir` volume at the socket directory:

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

Then set the address (via env var or cluster config) to a path inside that mount:

```yaml
# in envoy.tmpl.yaml cluster_config
value: '{"listen_addr":"unix:///run/orange/mcp.sock"}'
```

```yaml
# or as a container env var
env:
  - name: ORANGE_MCP_LISTEN_ADDR
    value: unix:///run/orange/mcp.sock
```

The cluster config approach keeps the address visible in the Envoy config and avoids
a separate env var.

## Filters

`orange-mcp-egress-match` is a real HTTP filter that lives on the
`orange-mcp-egress` listener. It validates and strips `x-orange-mcp-*` internal
headers written by the sidecar, injects backend auth, and writes state upstream
so the `orange-pick` cluster selects the correct backend.

There is no `orange-mcp` HTTP filter on the inbound listener. The sidecar lifecycle
was previously wired through a no-op filter; it is now owned entirely by the
`orange-mcp-loopback` cluster extension.

## Session keys

MCP sessions are signed with HMAC-SHA256. The key ring supports rotation:

```
ORANGE_MCP_SESSION_KEYS=newkey,oldkey1,oldkey2
```

The first key is the signing key; subsequent keys are accepted for verification
only (rolling rotation without invalidating in-flight sessions). Keys are plain
strings or `orange-generated` (ephemeral, generates a random 32-byte key on
startup — sessions are invalid after a restart).

**Do not use the default dev key in production.** The default logs a warning at
startup.

## Failure behaviour

If the sidecar fails to bind (`Listen()` returns an error), the cluster calls
`PreInitComplete()` without registering any hosts. Routes to `/mcp` return 503
until the process is restarted. This matches `pick`'s DNS-failure behaviour and
is preferable to crashing the process.
