---
name: transit-core-api-patterns
description: Patterns for working in the down/ and up/ layers of transit. Use when modifying the ABI bridge, registration API, filter lifecycle, mutation queuing, async callouts (HTTPCallout / Go+Do), access loggers, cluster extensions, or LB policies.
---

# Transit Core API Patterns

Use this skill when touching `down/`, `up/`, or anything that crosses the
Go↔Envoy ABI boundary.

## Layer boundary rule

```
Envoy .so
  down/abi_impl   ← CGO, unsafe, //export symbols — stays here
  down/           ← shared public types, registries, no CGO
  up/             ← user-facing Go API, re-exports down types
  your package    ← handlers, factories — imports only up/
```

`down/abi_impl` is the only place allowed to use CGO or `unsafe`. Keep
ordinary handler code out of that layer. Never add CGO to `down/down.go` or
any `down/*.go` file outside `abi_impl/`.

## `down/` file layout

Each subsystem has its own file; `down.go` holds only `RegisterHttpFilter`:

| File | Contents |
|---|---|
| `down.go` | `RegisterHttpFilter` (HTTP-filter bridge only) |
| `access_logger.go` | `AccessLogger*` types, registry |
| `cluster.go` | `Cluster*`, `ClusterLB*`, `HostPtr`, `HostSpec`, `ClusterLBCompletion` |
| `lb.go` | `LBPolicy*`, `LBHandle`, `LBContext` |
| `response_flags.go` | `ResponseFlagsString`, `responseFlagNames` |

## Registration pattern

Register filters from `init()` only. Use the narrowest form that covers the
need:

| Need | Call |
|---|---|
| Request headers only | `up.Register(name, onReq)` |
| Request + response headers | `up.RegisterWithResponse(name, onReq, onResp)` |
| Streaming request body | `up.RegisterWithBody(name, onReq, onBody, onResp)` |
| Buffered body read/replace | `up.RegisterWithMutableBody(name, onReq, onBody, onResp)` |
| Per-config metrics | `up.RegisterWithConfig(name, cfg, onReq, onResp)` |
| Background goroutines | `up.RegisterWithGroup(name, group, onReq)` |
| Access logger | `up.RegisterAccessLogger(name, factory)` |
| Cluster Extension | `up.RegisterCluster(name, factory)` |
| LB Policy | `up.RegisterLBPolicy(name, factory)` |

`registry` in `up/up.go` is a duplicate-name sentinel only; the canonical
runtime registry lives in `down`. Duplicate name → panic at startup.

## Mutation queue + flush model

CGO calls must run on the Envoy worker thread. Writer mutation methods
(`SetRequestHeader`, `SetFilterState`, `SetUpstreamOverrideHost`, etc.)
**queue** the operation when inside a filter lifecycle (production path) and
apply it **directly** only when `directWrite=true` (test/example Writers).

`flush()` drains all queues and optionally calls `ContinueRequest`:
- `flush(true)` — async path: stream was paused, resume it
- `flush(false)` — sync path: handler returning Continue, just apply mutations

Never call CGO mutation methods directly from a goroutine or callout callback.
Queue via Writer and let `flush` apply them on the worker thread.

## Async callout state machine

`calloutState` is a one-way CAS ratchet with four states:

```
Active → Paused → Flushed   (normal async path: callback fires after Stop)
Active → Done   → Flushed   (early sync path: callback fires before Stop)
```

`allSettledBatch.finish` (for `HTTPCalloutAllSettled`) follows the same
ratchet. Always check `streamDone.Load()` both before calling the user
callback and after it returns, before calling `flush(true)`.

## Choosing an async mode

| Mode | API | Use when |
|---|---|---|
| Callback callout | `w.HTTPCallout` | Need to send a local response on error |
| Fan-out callout | `w.HTTPCalloutAllSettled` | Multiple parallel upstream calls, single result callback |
| Goroutine + Do | `w.Go` + `w.Do` | Forward the request; no local response needed |

`SendLocalResponse` is only reliable from `HTTPCallout`/`HTTPCalloutAllSettled`
callbacks, which run from a filter callback. It is NOT reliable from `w.Go`
goroutines (scheduled callbacks are ignored by Envoy for local replies).

`w.Do` supports concurrent calls from fan-out goroutines, but Writer mutation
methods (`SetRequestHeader`, etc.) are NOT goroutine-safe. Join all goroutines
before issuing any mutations.

## `Group` lifecycle (RegisterWithGroup)

`Group.Start` runs all registered goroutines. If **any** actor exits for any
reason (including normal return), `Group.Stop` is called on the whole group.
Actors that must survive transient errors must loop and retry internally.

`RegisterWithGroup` wires `group.Stop` to the filter factory's `OnDestroy`,
so the group is stopped when Envoy unloads the filter config.

## `Empty*` embed types

`EmptyAccessLogger`, `EmptyClusterLB`, and `EmptyLBPolicy` provide no-op
implementations of optional interface methods. Embed them to avoid boilerplate
when only a subset of the interface is needed.

## `ClusterLBCompletion` (async host selection)

`Complete` and `Cancel` are idempotent via a mutex+done flag ratchet. Only the
first terminal call wins. `SetFinishFn` must handle the case where `done` is
already true at injection time — it fires the function immediately in that case.

## Comment style

Comments explain WHY, not WHAT. Write block comments before state machine
const blocks describing the full transition graph. One-line comments for
invariants that would surprise a reader. No comments for self-evident code.
