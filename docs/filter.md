# Filter Lifecycle and Writer Semantics

This document covers the internal mechanics that matter when writing Transit
filters: the two Writer modes, what each phase can and cannot do, and how
stream completion interacts with response handlers.

## Writer modes: queued vs direct

Every callback receives a `*up.Writer`. The Writer has two operating modes
controlled by the `directWrite` field:

**Queued mode** (`directWrite=false`) — mutations are appended to per-stream
queues on the filter struct. `flush()` drains them and calls the corresponding
Envoy CGO functions in one shot, then calls `ContinueRequest` to resume the
stream. This is the request-phase mode: batching mutations before resuming is
necessary because Envoy requires all header/metadata changes to be applied
before `ContinueRequest` is called.

**Direct mode** (`directWrite=true`) — mutations call the Envoy CGO function
immediately, inline. No queuing. No flush needed.

### Which mode each phase uses

| Phase | directWrite | Why |
| --- | --- | --- |
| `OnRequestHeaders` | false (queued) | Must batch before `ContinueRequest` |
| `OnRequestBody` | false (queued) | Same — stream is paused |
| `OnResponseHeaders` | true (direct) | No `ContinueRequest` on response path |
| `OnResponseBody` | true (direct) | Same |
| `OnStreamComplete` synthesis | true (direct) | flush() will never run |
| `Writer.Go` goroutine | false (queued) | Hops back to worker thread before flush |

### The response-path trap

Response-phase Writers use `directWrite=true`. If you forget this and construct
`&Writer{f: f}` on the response path (as was the bug in early Transit versions),
`IncrementCounter` and `RecordHistogram` silently queue mutations that are never
flushed. The call returns no error. Envoy never sees the increment. The mutation
is discarded at stream end.

This is why `IncrementCounter` and `RecordHistogram` work correctly in request
handlers but appear to do nothing in response handlers unless `directWrite` is
set.

## What each phase can do

### Request headers (`OnRequestHeaders`)

Full mutation surface: set/remove headers, send local response, set metadata,
set filter state, set upstream override, annotate trace span, increment
counters, record histograms. Changes are queued and applied before Envoy
continues the request.

### Request body (`OnRequestBody` / `OnRequestMutableBody`)

Same mutation surface. For mutable body, `w.GetBufferedBody()` and
`w.SetBufferedBody()` are also available.

### Response headers / body (`ResponseHandlerFunc`)

Mutations apply inline (direct mode). Available: `w.Log`, `w.IncrementCounter`,
`w.RecordHistogram`, `w.SetDynamicMetadata`, `w.SetFilterState`,
`w.AddSpanTag`, `w.SetResponseHeader`, `w.RemoveResponseHeader`.

`SendLocalResponse` is not meaningful here — Envoy has already committed to
sending the upstream response.

`SetRequestHeader` / `RemoveRequestHeader` are no-ops on the response path.

### OnStreamComplete synthesis

When a response handler is registered and Envoy terminates the stream without
delivering a final `endOfStream=true` body call (common with HTTP/1.1
connection-close for SSE and chunked transfers), Transit synthesizes the
missing call from `OnStreamComplete`. The synthesized `ResponseChunk` has
`EndStream=true`, `Data=nil`. This ensures response handlers that finalize
state on `EndStream` (emit counters, write metadata, release resources) run
exactly once even when Envoy closes the connection without explicit framing.

The `responseEndSeen` flag on the filter struct prevents double-delivery: once
any path delivers `EndStream=true`, the flag is set and neither the body
callback nor `OnStreamComplete` will synthesize again.

## Counter and histogram registration

Metrics must be defined at config time via `ConfigFunc`, not at request time:

```go
var inputTokensID up.MetricID

up.RegisterWithConfig("my-filter",
    func(h up.ConfigHandle) error {
        var err error
        inputTokensID, err = h.DefineCounter("my_filter_input_tokens")
        return err
    },
    onRequest,
    onResponse,
)
```

`DefineCounter` and `DefineGauge` / `DefineHistogram` are called once on the
main thread. The returned `MetricID` is safe to use from worker threads via
`w.IncrementCounter` in any phase.

## Request-phase callout interaction

`w.HTTPCallout` pauses the request stream. The continuation callback runs on
the worker thread after the callout completes. `Writer` inside the continuation
is in direct mode — it applies mutations immediately, then Transit calls
`ContinueRequest` to resume.

`SendLocalResponse` is safe from inside the callout callback. Do not call it
from `w.Go` goroutines — goroutines hop back to the worker thread via
`w.Do`, and by that point Transit has already decided to forward the request.
See `docs/async-http-callouts.md` for the full decision guide.

## Summary of pitfalls

| Pitfall | Symptom | Fix |
| --- | --- | --- |
| `IncrementCounter` in response handler with queued Writer | stat stays 0, no error | ensure Writer uses `directWrite=true` (Transit does this for you) |
| Defining metrics inside request handler | panic or wrong ID | define in `ConfigFunc`, store in package var |
| `SendLocalResponse` from `w.Go` goroutine | request forwarded AND local reply sent | use `w.HTTPCallout` instead |
| Relying on `EndStream=true` body call for HTTP/1.1 SSE | finalization never runs | Transit synthesizes the call from `OnStreamComplete` |
