# orange-ratelimit: Rate Limit Filter Design

## Background

Orange's config system already has full rate limit rule support — raw types, compiled domain
types, admin RPCs, and resolution helpers. What is missing is the enforcement layer: the
pipeline stage that reads those rules and either rejects, throttles, or logs requests that
exceed them.

This document designs the `orange-ratelimit` downstream HTTP filter and the supporting
infrastructure for both production and development deployments.

---

## Goals

1. Enforce rate limits configured via orange's config delivery mechanism — no separate
   per-service YAML files, no file watches, no SIGHUP.
2. Support all limit dimensions modelled in `RateLimitRule`: RPM/RPH/RPD,
   input/output/cache token limits (per minute/hour/day), and USD cost limits.
3. Support all three `OnExceed` behaviours: `reject`, `throttle`, `log_only`.
4. Use [envoyproxy/ratelimit](https://github.com/envoyproxy/ratelimit) as the counter
   and rate limit service, configured via its `GRPC_XDS_SOTW` mode so the CP pushes
   config dynamically over xDS ADS — no YAML files involved.
5. Keep the filter on the Envoy worker thread with no blocking I/O in the hot path.

---

## Deployment Model

```
Production:

  orange-server (CP)                envoyproxy/ratelimit       Redis
  ──────────────────                ────────────────────         ─────
  internal/rlsxds (xDS ADS server)  CONFIG_TYPE=GRPC_XDS_SOTW   ◀── counters
    pushes RateLimitConfig protos ──▶ hot-reloads on each push
  orange-ratelimit filter ────────────────────────────────────▶ ShouldRateLimit gRPC

Development / single-node:

  orange-server (CP)
  ├── embeddedpg              (Postgres subprocess, existing)
  ├── embeddedratelimit       (envoyproxy/ratelimit in-process, NEW)
  │     runner.NewRunner(Settings{ConfigType: "GRPC_XDS_SOTW", ...}).Run()
  └── miniredis               (in-process, inside embeddedratelimit)
```

Orange-server is the **single source of truth** for rate limit config in both modes.
envoyproxy/ratelimit never reads a YAML file; it connects to orange-server's xDS endpoint
and receives `RateLimitConfig` proto resources via ADS SotW whenever the config snapshot
changes.

---

## Architecture Overview

```
Client
  │
  ▼
[orange-match]          ← resolves key, model, routes; sets KeyBlobKey / KeyIDKey
  │
  ▼
[orange-ratelimit]      ← THIS FILTER
  │  pre-request: check RPM/RPH/RPD via ShouldRateLimit gRPC
  │  post-stream: record token/USD usage via ShouldRateLimit gRPC (async)
  │  on limit hit: send 429 local response (OnExceed=reject/throttle)
  │              or tag and continue (OnExceed=log_only)
  │
  ▼
[orange-adapt / orange-tracer / orange-meter ...]
  ▼
Upstream provider

orange-server (CP)
  └── internal/rlsxds
        xDS ADS server (go-control-plane SnapshotCache)
        AppState watcher → translate rules → push RateLimitConfig snapshot
```

---

## Filter Placement

`orange-ratelimit` is a **downstream** HTTP filter inserted between `orange-match` and
`orange-adapt`. This is the only correct position:

- `orange-match` must run first — it resolves the key and model.
- `orange-adapt` must run after — rejected requests never reach the upstream.
- `orange-meter` runs on the response path and writes token counts to `orange_meter`
  dynamic metadata, which `orange-ratelimit` reads in its finalized hook for post-stream
  counter updates.

---

## Two-Phase Enforcement

### Phase 1 — Pre-request (body phase, after model is resolved)

Dimensions checkable before the upstream call:

| Field | Window |
|---|---|
| `RPM` / `RPH` / `RPD` | per minute / hour / day |

The filter issues one `ShouldRateLimit` RPC with all active RPx descriptors for the
current (keyID, modelID). If any descriptor returns `OVER_LIMIT`, the filter sends a local
429 and short-circuits the stream. The RPC increments counters atomically.

### Phase 2 — Post-stream (OnStreamFinalized hook)

Dimensions requiring the upstream response:

| Field | Source metadata key |
|---|---|
| `InputTokensPerMinute/Hour/Day` | `orange_meter.input_tokens` |
| `OutputTokensPerMinute/Hour/Day` | `orange_meter.output_tokens` |
| `CacheReadTokensPerHour/Day` | `orange_meter.cache_read_input_tokens` |
| `CacheWriteTokensPerHour/Day` | `orange_meter.cache_creation_input_tokens` |
| `USDPerMinute/Hour/Day` | computed from tokens × model pricing |

After stream completion, the filter issues a fire-and-forget `ShouldRateLimit` RPC with
`hits_addend` set to the token count (or USD expressed as micro-dollars). The resulting
`OVER_LIMIT` state is stored in Redis, so the next pre-request check on the same
key+window sees the counter as exhausted.

---

## xDS Config Server (`internal/rlsxds`)

Orange-server embeds a small xDS ADS server that speaks the SotW protocol expected by
`envoyproxy/ratelimit`'s `XdsGrpcSotwProvider`. It uses `go-control-plane`'s
`cache.SnapshotCache` and `server.Server`.

### Resource type

```
type.googleapis.com/ratelimit.config.ratelimit.v3.RateLimitConfig
```

Each `RateLimitConfig` resource represents one domain. Orange uses a single domain
(`"orange"`) and pushes one resource per snapshot.

### Snapshot update flow

```
AppState.Snapshot() changes
  → rlsxds.Watcher.OnSnapshot(snapshot)
  → translate(snapshot) → []RateLimitConfig protos
  → cache.SetSnapshot(nodeID, newSnapshot)
  → envoyproxy/ratelimit receives SotW response, hot-reloads config
```

No restart, no SIGHUP, no file writes.

### Rule translation: AppState → RateLimitConfig proto

The translator builds a nested `RateLimitDescriptor` tree from all rules in the snapshot.

```
RateLimitConfig {
  name:   "orange-ratelimit"
  domain: "orange"
  descriptors: [
    // workspace-scope rule (1-segment): key_id=ws-abc, dim=rpm
    RateLimitDescriptor {
      key: "key_id", value: "ws-abc"
      descriptors: [
        { key: "dim", value: "rpm",
          rate_limit: { unit: MINUTE, requests_per_unit: 1000 } }
        { key: "dim", value: "input_tpd",
          rate_limit: { unit: DAY, requests_per_unit: 5000000 } }
      ]
    }
    // user-scope rule (2-segment): key_id=ws-abc/user-xyz
    RateLimitDescriptor {
      key: "key_id", value: "ws-abc/user-xyz"
      descriptors: [
        { key: "dim", value: "rpm",
          rate_limit: { unit: MINUTE, requests_per_unit: 200 } }
      ]
    }
    // key-scope rule: key_id=sk-def456
    RateLimitDescriptor {
      key: "key_id", value: "sk-def456"
      descriptors: [
        { key: "dim", value: "rpm",
          rate_limit: { unit: MINUTE, requests_per_unit: 60 } }
        { key: "dim", value: "usd_per_day",
          rate_limit: { unit: DAY, requests_per_unit: 5000000 } }  // micro-USD
      ]
    }
  ]
}
```

The `key_id` descriptor value encodes the scope:
- 1-segment (`ws-abc`): workspace scope — `GlobalConfig.RateLimits[workspaceID]`
- 2-segment (`ws-abc/user-xyz`): user scope — `GlobalConfig.RateLimits[workspaceID+"/"+userID]`
- raw key ID (`sk-def456`): key scope — `KeyRecord.RateLimitRules`

The translator iterates all scopes in `GlobalConfig.RateLimits` and all `KeyRecord`s in
the snapshot to produce a flat list of `key_id`-keyed top-level descriptors.

### Dimension → unit mapping

| `dim` value | unit | Field |
|---|---|---|
| `rpm` | `MINUTE` | `RPM` |
| `rph` | `HOUR` | `RPH` |
| `rpd` | `DAY` | `RPD` |
| `input_tpm` | `MINUTE` | `InputTokensPerMinute` |
| `input_tph` | `HOUR` | `InputTokensPerHour` |
| `input_tpd` | `DAY` | `InputTokensPerDay` |
| `output_tpm` | `MINUTE` | `OutputTokensPerMinute` |
| `output_tph` | `HOUR` | `OutputTokensPerHour` |
| `output_tpd` | `DAY` | `OutputTokensPerDay` |
| `cache_read_tph` | `HOUR` | `CacheReadTokensPerHour` |
| `cache_read_tpd` | `DAY` | `CacheReadTokensPerDay` |
| `cache_write_tph` | `HOUR` | `CacheWriteTokensPerHour` |
| `cache_write_tpd` | `DAY` | `CacheWriteTokensPerDay` |
| `usd_per_min` | `MINUTE` | `USDPerMinute` (× 1,000,000 → micro-USD integer) |
| `usd_per_hour` | `HOUR` | `USDPerHour` |
| `usd_per_day` | `DAY` | `USDPerDay` |

USD dimensions use micro-dollars (`requests_per_unit = USD × 1_000_000`) so fractional
costs remain representable as `uint32` without floating-point.

---

## Descriptors Sent by the Filter

The filter sends one descriptor tuple per (scope level, active dimension) for the current
request, so workspace, user, and key-scope counters are all checked and incremented in a
single `ShouldRateLimit` RPC.

```
domain: "orange"
descriptors:
  # key-scope RPM
  - [ key_id=<keyID>,                    dim=rpm ]  hits_addend=1
  # user-scope RPM
  - [ key_id=<wsID>/<userID>,            dim=rpm ]  hits_addend=1
  # workspace-scope RPM
  - [ key_id=<wsID>,                     dim=rpm ]  hits_addend=1
  # ... one tuple per (scope, active dimension)
```

Post-stream token descriptors use `hits_addend = <token_count>` (or `<micro_USD>`).

---

## orange-ratelimit Filter Implementation

### Registration

```go
// internal/pipeline/ratelimit/ratelimit.go
func init() {
    up.Register(ExtensionName,
        requestHeaders,
        up.WithMutableBody(requestBody),
        up.WithOnStreamFinalized(streamFinalized),
        up.WithConfig(configure),
    )
}
```

### Per-stream state

```go
type state struct {
    keyID       string
    workspaceID string
    userID      string // empty if key is not under a user scope
    modelID     string
    rules       []config.RateLimitRule // merged global + key-scope rules for this model
    skip        bool                   // no rules or no key
    logOnly     bool                   // limit hit but OnExceed=log_only
}
```

### Request headers phase

Read `KeyIDKey` from the stream object bag. If absent (legacy/no-keys mode), set
`skip = true`. Parse workspace and user prefix from the key ID intern handles for scope
descriptor construction.

### Request body phase (MutableBody)

1. Read `KeyBlobKey.Get(w)` and model ID from `match.Decision`.
2. Resolve rules: `GlobalConfig.ResolveRateLimitRules(keyID, modelID)` + append
   `KeyRecord.RateLimitRules` (model-filtered).
3. If no rules, set `skip = true` and return.
4. Build pre-request (RPx) descriptors for all three scope levels.
5. Call `ShouldRateLimit` with a 50 ms deadline. On error: log and fail open (configurable).
6. On `OVER_LIMIT`: find the matching rule's `OnExceed`:
   - `reject` / `throttle`: `send.Error(w, 429, send.RateLimitError, msg)` with
     `Retry-After` and `X-RateLimit-*` headers from the RPC response.
   - `log_only`: continue, set `state.logOnly = true`.

### OnStreamFinalized hook

1. If `skip`, return.
2. Read token counts from `orange_meter` dynamic metadata.
3. Compute USD: tokens × model pricing → micro-dollars (using `shopspring/decimal`).
4. Build post-stream descriptors with token/micro-USD `hits_addend`.
5. Issue `ShouldRateLimit` in a goroutine (fire-and-forget, no stream deadline).
6. Log any `OVER_LIMIT` responses — enforcement hits the next request.

---

## embeddedratelimit (Development Mode)

Since `envoyproxy/ratelimit` is a Go project, it is embedded **in-process** rather than
as a subprocess. Its `runner.Runner` struct exposes `NewRunner(Settings)`, `Run()` (blocks),
and `Stop()` — enough to drive it from orange-server's lifecycle directly. No binary
download, no checksum pinning, no subprocess management.

`miniredis` is already a dependency of `envoyproxy/ratelimit`'s own `go.mod`, so it comes
for free transitively.

```go
// internal/embeddedratelimit/embeddedratelimit.go
package embeddedratelimit

import (
    "fmt"

    "github.com/alicebob/miniredis/v2"
    rlrunner  "github.com/envoyproxy/ratelimit/src/service_cmd/runner"
    rlsettings "github.com/envoyproxy/ratelimit/src/settings"
)

type Instance struct {
    GRPCAddr string // "127.0.0.1:<GRPCPort>" — filter connects here
    runner   *rlrunner.Runner
    mr       *miniredis.Miniredis
}

// Start starts an in-process miniredis and an in-process envoyproxy/ratelimit
// service configured to pull config via xDS SotW from xdsServerURL.
func Start(xdsServerURL string, grpcPort int) (*Instance, error) {
    mr, err := miniredis.Run()
    if err != nil {
        return nil, err
    }
    s := rlsettings.Settings{
        ConfigType:                     "GRPC_XDS_SOTW",
        ConfigGrpcXdsServerUrl:         xdsServerURL,
        ConfigGrpcXdsNodeId:            "orange-dev",
        ForceStartWithoutInitialConfig: true,
        RedisSocketType:                "tcp",
        RedisUrl:                       mr.Addr(),
        GrpcPort:                       grpcPort,
        DisableStats:                   true,
        LogLevel:                       "WARN",
    }
    r := rlrunner.NewRunner(s)
    go r.Run()
    return &Instance{
        GRPCAddr: fmt.Sprintf("127.0.0.1:%d", grpcPort),
        runner:   &r,
        mr:       mr,
    }, nil
}

// Stop shuts down the ratelimit runner and miniredis. Must be called from
// orange-server's shutdown path before the process exits, so that the runner's
// own SIGINT/SIGTERM handlers never fire (they would race with orange-server's).
func (i *Instance) Stop() {
    i.runner.Stop()
    i.mr.Close()
}
```

### Signal handler note

`runner.Run()` registers its own `SIGINT`/`SIGTERM`/`SIGHUP` handlers. When embedded,
calling `i.Stop()` from orange-server's existing shutdown path (the same deferred cleanup
block that calls `embeddedpg.Stop()`) ensures the runner exits cleanly before the process
terminates. The runner's signal handler never gets a chance to fire.

### Wiring in orange-server

```go
// server.go (alongside embeddedpg startup)
if cfg.EmbedRatelimit {
    rl, err := embeddedratelimit.Start(
        fmt.Sprintf("127.0.0.1:%d", rlsxdsPort),
        cfg.EmbeddedRatelimitGRPCPort,
    )
    defer rl.Stop()
    // rl.GRPCAddr is passed to the orange-ratelimit filter config
}
```

---

## OnExceed Behaviour

| Value | Pre-request | Post-stream |
|---|---|---|
| `reject` | 429, stream stopped | logs over-limit; next request blocked |
| `throttle` | 429, same as reject (LLM streams are not deferrable) | same |
| `log_only` | continue; add `X-RateLimit-Limit-Hit: <dim>` to response | logs only |

`throttle` and `reject` are identical in the filter. The distinction is preserved for a
possible future queuing extension.

---

## Response Headers (on 429)

```
HTTP/2 429 Too Many Requests
Content-Type: application/json
Retry-After: 37
X-RateLimit-Limit: 60
X-RateLimit-Remaining: 0
X-RateLimit-Reset: 1749340320
X-RateLimit-Policy: rpm

{"error":{"type":"rate_limit_error","message":"rpm limit of 60/min exceeded"}}
```

---

## Rule Resolution Order

1. Workspace-scope rules (`GlobalConfig.RateLimits[workspaceID]`)
2. User-scope rules (`GlobalConfig.RateLimits[workspaceID+"/"+userID]`)
3. Key-scope rules (`KeyRecord.RateLimitRules`)

All matching rules are enforced. The most restrictive `OnExceed` wins (`reject` >
`throttle` > `log_only`) when multiple rules trigger simultaneously.

---

## New Package Structure

```
examples/orange/internal/
  rlsxds/
    server.go       ← go-control-plane SnapshotCache + ADS server wiring
    translate.go    ← ConfigSnapshot → []rls_conf_v3.RateLimitConfig proto resources
    watcher.go      ← AppState change hook → calls translate + SetSnapshot

  embeddedratelimit/
    embeddedratelimit.go  ← in-process runner.Runner + miniredis (single file)

  pipeline/ratelimit/
    ratelimit.go          ← filter registration, state type
    descriptors.go        ← buildPreRequestDescriptors, buildPostStreamDescriptors
    client.go             ← ShouldRateLimit gRPC client wrapper, timeout/retry
    usd.go                ← token counts × model pricing → micro-USD
```

---

## Dependencies

| Package | Purpose | Mode |
|---|---|---|
| `github.com/envoyproxy/go-control-plane` | xDS SnapshotCache, ADS server, `rls_conf_v3` proto | prod + dev |
| `github.com/envoyproxy/ratelimit` | In-process `runner.Runner` + `settings.Settings` | dev/test |
| `github.com/alicebob/miniredis/v2` | In-process Redis (transitive via ratelimit's go.mod) | dev/test |
| `envoy.service.ratelimit.v3` (in go-control-plane) | `ShouldRateLimit` RPC types | prod + dev |
| `github.com/shopspring/decimal` | USD micro-dollar arithmetic (already in go.mod) | prod + dev |

No Redis client library is needed in orange itself. envoyproxy/ratelimit owns its own
Redis connection; in dev mode that connection points at the in-process miniredis.

---

## Open Questions

1. **Node ID for xDS**: In production, each envoyproxy/ratelimit instance connects with a
   `CONFIG_GRPC_XDS_NODE_ID`. The CP must set a snapshot for that node ID (or use a wildcard
   cache). The simplest approach is a single well-known node ID (`"orange-ratelimit"`) for
   single-cluster deployments; multi-region deployments can use per-region IDs.

2. **Wildcard model + USD**: If a workspace rule targets `"*"` models, the USD budget is
   a cross-model aggregate. The filter computes actual USD from the resolved model's pricing
   and reports it as micro-dollars. The same YAML-less `requests_per_unit` in the descriptor
   covers the aggregate regardless of which model was used.

3. **Key rotation mid-window**: Rotated keys are rejected at the auth layer (match filter)
   before rate limit runs. Stale Redis counters under the old key ID expire naturally.

4. **Throttle as queue**: Out of scope for the initial implementation.
