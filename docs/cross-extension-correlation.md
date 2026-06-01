# Cross-extension correlation: guidance

> **Status:** implementation guidance. Primitive B landed in `d5803e4`;
> Primitive A landed in `661f47a`, and orange has been migrated to it.
> request-ui still demonstrates the older access-logger correlation shape.

Envoy's dynamic-module extension types (HTTP filter, ClusterLB extension,
LB Policy, AccessLogger) are independent: each runs in its own per-stream
or per-request context, and the only typed data path Envoy gives the SDK
between them is **filter state** — a per-stream string → string bag. Any
time two extensions on the same stream need to share a typed value
(`*Pending`, `*sink.Record`, anything that isn't a string), code must
serialize identity into filter state, key a process-wide map by that
identity, and arrange unconditional cleanup. That pattern is what this
doc calls a **correlation**.

Correlations are a footgun for two reasons:

- **Leak class.** Every process-wide map needs a drain owner. If cleanup
  lives on the producer side (`defer m.Delete(key)` in the body handler),
  any teardown that skips the producer leaks. See
  [`orange-token-correlation-risks.md`](orange-token-correlation-risks.md)
  for the worked example.
- **Stringly-typed contract.** The token, the key, the cleanup discipline,
  and the lifetime are all conventions in user code, not types. They drift.

The SDK can absorb most of this. This doc enumerates the cases and
sketches the primitives.

## Cases

### 1. Downstream filter → ClusterLB extension (the orange case)

- **When**: body-driven routing — LB must pick a host *after* the body
  arrives, so `ChooseHost` needs to suspend on a value the request body
  handler will produce.
- **Today**: orange creates a `*pending.Pending` in the request handler,
  stores it with `Writer.SetStreamObject`, and `ChooseHost` reads it with
  `ClusterLBContext.GetStreamObject`. Cleanup of the process-wide SDK bag
  is owned by Primitive A; `up.WithOnStreamComplete` only resolves the
  pending value with `ErrStreamTerminated`.
- **Race**: yes — consumer (`ChooseHost`) arrives before producer
  (`bodyHandler`). `Pending.OnResolve` handles the sync.
- **Leak status**: closed (Phases 1–3 in orange).

### 2. Downstream filter → AccessLogger (the request-ui case)

- **When**: the filter wants finalized stream fields (duration, wire
  bytes, response flags, code details) attached to a record it built
  earlier. AccessLogger is the only place Envoy delivers those finalized
  fields.
- **Today**: `examples/request-ui` deposits a partial `*sink.Record` into
  `PendingRecords` (a `sync.Map`) keyed by `x-request-id` in the response
  handler. The access logger pops it in `OnLog(AccessLogTypeDownstreamEnd)`,
  enriches with finalized fields, sends to the sink.
- **Race**: no — producer always runs first.
- **Leak status**: a soft leak class exists if `x-request-id` is empty
  or changes between the two views; the early-return paths in
  `alLogger.OnLog` orphan entries silently.

### 3. Upstream filter ↔ downstream filter (same stream)

- **When**: an upstream filter learns something the downstream filter
  cares about (real upstream identity, attempt count, origin trace ids,
  cache hit, response-side decisions) and wants to surface it without
  going through filter state as strings.
- **Today**: not directly demonstrated in-tree; both halves can use
  `Writer.SetStreamObject` / `Writer.GetStreamObject` while running in the
  same stream.
- **Race**: usually no (response direction: upstream chain finalizes
  before downstream response handler observes the response). If the
  direction is downstream → upstream response side, treat as the orange
  case with `Pending.OnResolve`.

### 4. Upstream filter → LB Policy (NOT a stream correlation)

This one doesn't fit the same primitive. Two reasons:

1. **`LBContext` has no filter state.** `down/lb.go` is explicit:
   *"filter-state and downstream-SNI are unavailable in LB Policy
   context; use the Cluster Extension (ClusterLBContext) if those are
   needed."* No nonce can be passed in without new ABI surface.
2. **Order**: LB Policy runs *during host selection*, before the upstream
   filter chain runs for that request. The upstream filter for request N
   cannot inform LB for the same request N. Any data path here is
   **request N+1**: cluster-scoped, host-keyed feedback, not a per-stream
   handoff.

   ```
   Request N:   downstream filters → LB pick → upstream filters → origin
                                      ^                ↓
                                      |________________|  cluster-scoped, request N+1
   ```

The right primitive for this case is a different shape — a host-keyed
cluster bag — and it's out of scope of the stream-object work. See
[Out of scope: cluster-scoped host feedback](#out-of-scope-cluster-scoped-host-feedback).

## SDK primitives

Two additive primitives cover the in-scope cases (1–3).

### Primitive A — typed per-stream object handoff

A producer/consumer pair on the SDK surface. up owns the indirection
table and the drain.

```go
// up/writer.go — producer side, available to any HTTP filter
// (downstream or upstream). Stores v under key for the lifetime of the
// stream.
func (w *Writer) SetStreamObject(key string, v any)

// Symmetric reader for filter ↔ filter handoff on the same stream.
func (w *Writer) GetStreamObject(key string) (any, bool)

// up/cluster.go — consumer for ClusterLB.
type ClusterLBContext interface {
    // ... existing ...
    GetStreamObject(key string) (any, bool)
}

```

Implementation (`up/stream_object.go`, `down/stream_object.go`):

- On first `SetStreamObject` for a stream, mint a short random nonce,
  write it to a reserved filter-state key (`up.stream_object_id`), and
  create a per-stream bag in a process-wide `map[nonce]*streamObjects`.
- Subsequent `SetStreamObject` calls in the same stream reuse the nonce
  already in filter state.
- `Writer.GetStreamObject` reads the bag through the filter's stream
  nonce; `ClusterLBContext.GetStreamObject` reads the nonce from filter
  state and looks up the bag.
- `OnStreamComplete` deletes the bag for ordinary filters. If the filter
  also uses `WithOnStreamFinalized`, drain is deferred to the internal
  finalized access logger so finalized cleanup runs before the bag is
  removed.

Covers cases **1** and **3**. It is not currently exposed on
`AccessLoggerHandle`; use Primitive B for finalized stream fields.

#### What it eliminates

- orange: `pending.registry sync.Map`, `pending.Register/Lookup/Delete`,
  `classify.StateToken`, the `pending.Delete` call in `onStreamComplete`.
  `Pending` collapses back to a single-shot CAS + OnResolve primitive.
- future filter-to-filter consumers: ad hoc filter-state tokens and
  per-consumer registries.

### Primitive B — stream-finalized callback

Closes case **2** by collapsing it from two extensions to one.

```go
// up/up.go
type FinalizedInfo struct {
    Timing                       TimingInfo
    Bytes                        BytesInfo
    ResponseCode                 uint32
    ResponseCodeDetails          string
    ResponseFlags                uint64
    UpstreamFailure              string
    UpstreamLocalAddress         string
    UpstreamAddress              string
    RequestProtocol              string
    UpstreamPoolReadyDurationNs  int64
    UpstreamRequestAttempts      uint32
    TraceID, SpanID              string
    TraceSampled                 bool
    LocalReplyBody               string
}

// Fires on the worker thread after Envoy finalizes the stream, before
// the per-stream context is released. Like OnStreamComplete: no Writer,
// no mutations. Strict superset of what OnStreamComplete sees.
type OnStreamFinalizedFunc func(ctx *any, info FinalizedInfo)

func WithOnStreamFinalized(fn OnStreamFinalizedFunc) FilterOption
```

Implementation: `Register` auto-registers an SDK-internal access logger
under the filter name. The filter deposits `(fn, ctx)` during request
headers, keyed by `x-request-id` or an SDK fallback header when the request
id is absent. The finalized logger consumes that entry at
`AccessLogTypeDownstreamEnd` and populates `FinalizedInfo` from the same
attributes/timings `AccessLoggerHandle` wraps.

Coexists with `OnStreamComplete` — the simpler signature remains for
cleanup-only consumers (orange).

#### What it eliminates

- request-ui: the entire access-logger half (`accesslogger.go`,
  `PendingRecords`, `RegisterLogger`, the two-sided wiring in
  `cmd/main.go`). One filter, one hook, one `sink.Send`.

## Mapping

| Direction | Primitive | Race? | Notes |
|---|---|---|---|
| downstream filter ↔ ClusterLB | A | yes (header time) | `Pending.OnResolve` over A |
| downstream filter ↔ AccessLogger | **B** | no | A is not exposed on AccessLoggerHandle |
| upstream filter ↔ downstream filter | A | usually no | response direction natural; downstream→upstream response is the orange shape |
| upstream filter → LB Policy | neither | — | see next section |

## Out of scope: cluster-scoped host feedback

Case **4** needs a different primitive — not stream-scoped, host-keyed,
cluster-bag. Sketch only, parked until a real use case lands:

```go
// up/cluster.go — owned by the cluster extension.
type ClusterHandle interface {
    // ... existing ...
    SetHostFeedback(host HostPtr, key string, v any)
    GetHostFeedback(host HostPtr, key string) (any, bool)
}

// up/writer.go — convenience for filters: Writer knows the picked host
// via attributes.
func (w *Writer) RecordHostFeedback(key string, v any)
```

Distinct from A in scope (host, not stream), lifetime (drained by host
membership events, not stream end), and concurrency (multi-writer per
host). Don't conflate with Primitive A.

Also requires Envoy-side work to expose filter state to LB Policy if
that direction is ever needed in-flight — flagged here for completeness.

## Landed order

1. **Primitive B** landed first as `WithOnStreamFinalized`.
2. **Primitive A** landed next and migrated orange off
   `pending.registry`.
3. **Cluster-scoped host feedback** remains parked until a real LB
   feedback case appears. Sketch above; don't speculate further until
   then.

## Anti-patterns

These have come up and are explicitly rejected for the same reasons as
in `orange-token-correlation-risks.md`:

- **TTL sweeper.** Band-aid hiding the contract. Adds a knob, a
  goroutine, and silently swallows the bug class instead of fixing it.
- **Per-consumer cleanup discipline.** Putting `defer registry.Delete`
  on the producer side without an unconditional drain owner. Looks fine
  in tests, leaks under teardowns the producer doesn't see (client
  disconnect, idle timeout, foreign local reply).
- **String-encoding typed values into filter state.** JSON / proto in
  filter state for cross-extension typed handoff. Works once, doesn't
  scale, no lifecycle.

## Related docs

- [`orange-token-correlation-risks.md`](orange-token-correlation-risks.md) —
  the worked example. Phases 1–3, plus the now-resolved global registry
  note.
- [`envoy-dynamic-module-upstream-selection.md`](envoy-dynamic-module-upstream-selection.md) —
  why the filter ↔ ClusterLB hop exists at all.
- [`cluster-async-router-eg.md`](cluster-async-router-eg.md) — async
  routing primitives the orange case builds on.
