# orange: token-correlation risks

> **Status (Phase 1–3 landed):** the two leak risks described below are
> closed. Phase 1 added the `OnStreamComplete` hook to the `up` SDK; Phase 2
> moved orange cleanup to a single owner driven by that hook; Phase 3
> replaced the per-request `waitAndComplete` goroutine with an inline
> `Pending.OnResolve` callback. The remaining "what about the global
> registry?" question is answered explicitly under [Note: global registry,
> kept](#note-global-registry-kept) below.

The orange LLM proxy routes by request **body** (`model` field), so the
upstream cluster must be picked *after* the body arrives. The mechanism:

1. `classify.requestHandler` (downstream headers phase) mints a token, calls
   `pending.Register(token)` → `*Pending`, stashes the token in filter state.
2. `hostpick.ChooseHost` (cluster LB, runs during `router.decodeHeaders`)
   reads the token from filter state, registers a `ClusterLBCompletion`
   waiter, and spawns a goroutine `waitAndComplete` that blocks on
   `<-p.Done()`.
3. `classify.bodyHandler` parses `model`, calls `st.p.Resolve(...)`, then
   `defer pending.Delete(st.token)`.
4. `waitAndComplete` unblocks, hops back to the worker thread via
   `handle.Schedule`, and calls `completion.Complete(host)`.

This works when the happy path runs end-to-end. Below are the two risks
when it doesn't.

Relevant files:
- `examples/orange/classify/classify.go` — token mint, Register, Delete (in bodyHandler defer)
- `examples/orange/hostpick/hostpick.go` — `lb.ChooseHost`, `waitAndComplete`, `CancelHostSelection`
- `examples/orange/pending/pending.go` — global `registry sync.Map`, `Pending` type
- `up/filter.go` — filter lifecycle; `OnStreamComplete` exists internally but is **not exposed**

## Risk 1 — token + Pending leak in the global registry — **fixed**

> **Status:** closed by Phase 2. Cleanup is now owned by
> `classify.onStreamComplete`, registered via `up.WithOnStreamComplete`. It
> runs for every stream Envoy terminates, idempotently calling
> `pending.Delete(token)` and a terminal `Resolve`. The `defer
> pending.Delete` in `bodyHandler` is gone; the body handler only publishes
> a real result.
>
> e2e coverage: `examples/orange/e2e/leak_test.go` —
> `TestRegistry_baselineAfterSuccess`,
> `TestRegistry_baselineAfterUnknownModel`,
> `TestRegistry_baselineAfterClientAbort`,
> `TestRegistry_baselineUnderConcurrentLoad`. All four poll
> `/pending/size` on the orange debug endpoint and assert the registry
> drains back to baseline.


`pending.registry` is a process-wide `sync.Map`. The only cleanup site is
the `defer pending.Delete(st.token)` inside `classify.bodyHandler`. If the
body handler never runs, the entry stays forever.

When can `bodyHandler` fail to run?
- Downstream disconnect after headers but before the body is fully received.
- Idle/request timeout fires after headers.
- Another filter returns a local reply between headers and body.
- Stream reset for any reason mid-headers.

Per-entry cost is small (~100B: a string token + a `Pending` struct with a
channel and atomic). At low QPS / short runs (demo, tests) this is invisible.
At production QPS it's an unbounded `sync.Map` — slow growth, but real.

## Risk 2 — goroutine leak in `waitAndComplete` — **fixed**

> **Status:** closed by Phase 3. `waitAndComplete` is deleted. `ChooseHost`
> now installs a callback via `pending.Pending.OnResolve`; when classify
> resolves the pending (real result or the terminal `orange.stream_terminated`
> from `onStreamComplete`), the callback fires inline and hops to the
> cluster main thread via `handle.Schedule`. No per-request goroutine is
> parked, so the 8 KB × in-flight stack cost is gone. The `waiters` map was
> renamed to `cancelled` and inverted — it's empty in the happy path and
> only gains entries on `CancelHostSelection`.


This one is sharper. Once `ChooseHost` runs and spawns the waiter goroutine:

```go
go l.waitAndComplete(p, completion)   // blocks on <-p.Done()
```

If the Pending is never `Resolve`d (same conditions as Risk 1 *plus* the
case where `CancelHostSelection` fires after `ChooseHost` returned), the
goroutine blocks forever. `CancelHostSelection` today only does:

```go
func (l *lb) CancelHostSelection(completion *up.ClusterLBCompletion) {
    l.mu.Lock()
    delete(l.waiters, completion)
    l.mu.Unlock()
}
```

It removes the completion from the waiter map but does **not** resolve the
Pending — so the goroutine keeps blocking on `<-p.Done()`.

Each blocked goroutine is ~8KB of stack. At 1k req/s with even 1% of streams
hitting the "after ChooseHost, before bodyHandler" window:

    1000 * 0.01 * 3600 * 8KB ≈ 290 MB / hour

Production-killer. Demo-noise.

## Severity by teardown timing

| Teardown timing                              | What leaks                              | Likelihood     |
|----------------------------------------------|------------------------------------------|----------------|
| Before `requestHandler`                      | nothing                                  | —              |
| After `requestHandler`, before `ChooseHost`  | token + Pending (~100B)                  | tiny window    |
| After `ChooseHost`, before `bodyHandler`     | token + Pending + goroutine (~8KB)       | small but real |
| After `bodyHandler` ran                      | nothing (defer ran, Resolve unblocked)   | —              |

## Practical vs theoretical

For the current orange demo (curl in a terminal, e2e tests) neither risk
matters. Both surface only under sustained traffic with non-trivial
disconnect/timeout rates.

That doesn't mean we should ignore them — the global registry + implicit
cleanup contract is the kind of pattern that's cheap to fix now and
expensive to debug later.

## Fixes

### Minimum viable: fix `CancelHostSelection`

Store `{token, p}` in the waiter map instead of `struct{}{}`. On cancel,
resolve the Pending with an error and delete the token:

```go
type waiter struct {
    token string
    p     *pending.Pending
}

func (l *lb) CancelHostSelection(completion *up.ClusterLBCompletion) {
    l.mu.Lock()
    w, ok := l.waiters[completion]
    delete(l.waiters, completion)
    l.mu.Unlock()
    if !ok { return }
    w.p.Resolve(pending.Result{Err: "orange.cancelled"})  // CAS — no-op if already resolved
    pending.Delete(w.token)                                // idempotent
}
```

Safe because `Pending.Resolve` is a CAS (loses races silently) and
`pending.Delete` is idempotent. Closes the goroutine leak whenever Envoy
calls `CancelHostSelection`.

This still doesn't cover the "stream torn down without `CancelHostSelection`
being called" case — see API gap below.

### Better: goroutine owns the cleanup

Move the `pending.Delete(token)` out of `classify.bodyHandler` and into
`waitAndComplete`, which is the *only* code path guaranteed to run once the
Pending closes (regardless of how it closed). Combine with a context so the
goroutine can exit on cancel:

```go
func (l *lb) waitAndComplete(ctx context.Context, token string, p *pending.Pending, completion *up.ClusterLBCompletion) {
    defer pending.Delete(token)
    select {
    case <-p.Done():
        // ... existing Schedule + Complete path ...
    case <-ctx.Done():
        return
    }
}
```

Cancel the context from `CancelHostSelection`. Now both risks collapse to
the same invariant: "the goroutine is the owner; if it exits, the token is
gone." `bodyHandler` only resolves.

### Architectural: expose `OnStreamComplete`

The root cause is that filter handlers have no end-of-stream hook. `up`
already has `OnStreamComplete` internally (`up/filter.go`); exposing it via
`RegisterWithMutableBody` (or a sibling) would let `classify` do its own
cleanup unconditionally:

```go
func onStreamComplete(w *up.Writer, r *up.Request) {
    st, _ := (*r.Context).(*streamState)
    if st != nil { pending.Delete(st.token) }
}
```

That's a transit2 API change, not an orange change.

### Not: a TTL sweeper

Tempting (background goroutine that walks `registry` every N seconds,
deletes anything older than M) but it's a band-aid hiding the real issue —
the contract that *something* must clean up after `Register`. Adds a knob
("how long is too long?"), adds a goroutine, and silently swallows the bug
class instead of fixing it. Skip.

## Recommended order

1. Fix `CancelHostSelection` to resolve + delete (small diff, eliminates the
   common goroutine-leak path).
2. Refactor to goroutine-owns-cleanup with `ctx` (closes both risks
   together; small classify diff).
3. (Separate work) add `OnStreamComplete` to the `up` filter API — useful
   beyond orange.

## What landed

A different order than the one recommended above, but converging on a
cleaner end state:

- **Phase 1: `up.WithOnStreamComplete`.** New `FilterOption` on every
  `up.RegisterWith*` constructor. The callback is invoked from
  `filter.OnStreamComplete` regardless of how Envoy terminated the
  stream (normal end, reset, idle timeout, foreign local reply). Receives
  the per-stream context (`*any`), no Writer — the stream is dead and
  mutations would be no-ops. Unit-tested in `up/up_test.go`; end-to-end
  tested in `e2e/stream_complete_test.go` against real Envoy, also under
  `ENVOY_CONCURRENCY=2` to exercise multi-worker behavior.
- **Phase 2: classify owns cleanup.** A single `onStreamComplete` handler
  in `classify` does `pending.Delete(token)` and a CAS `Resolve` with
  `orange.stream_terminated`. The `bodyHandler`'s `defer pending.Delete`
  was removed — bodyHandler now only publishes a real result, and the
  cleanup contract has exactly one owner that fires unconditionally.
- **Phase 3: kill the goroutine via OnResolve.** `pending.Pending` got an
  `OnResolve(fn)` API: fires inline on the resolver's thread, or
  immediately if the Pending was already resolved. `hostpick.ChooseHost`
  registers a callback that hops to the cluster main thread and calls
  `Complete`. `waitAndComplete` is gone. `waiters` was renamed to
  `cancelled` and inverted (empty in happy path; entries only on cancel).

## Note: global registry, kept

The `pending.registry sync.Map` is still there. With the leak class closed
this is a deliberate trade-off, not unfinished work:

- **The leak class is closed.** Cleanup is unconditional via
  `OnStreamComplete`. Entries land on `requestHandler` and drain on
  `onStreamComplete`. The e2e tests (`TestRegistry_baseline*`) prove the
  drain happens for every teardown class — success, local-reply,
  client-abort, and concurrent mixed load.
- **The registry exists for a structural reason.** Envoy's only typed
  cross-extension data path between a downstream filter and a cluster LB
  is filter state (string → string). Passing a `*Pending` requires
  serializing identity, which forces a global map keyed by that identity.
  Removing the registry means giving the `up` SDK a typed per-stream
  handoff (e.g. `ClusterLBContext.GetStreamObject(key) any`), which is a
  meaningful SDK change.
- **One consumer doesn't justify an SDK change.** orange is the only
  caller today. Defer the typed-handoff SDK work until a second consumer
  appears; revisit then with the actual use case in hand.
- **Cost is bounded.** Worst case `~100 B × in-flight requests` (one
  string token + one `Pending`), drained at stream complete. `Size()` is
  exposed via the orange debug endpoint so operators can monitor it if
  they want a hard ceiling later. Adding a metric backed by `Size()` is a
  one-liner the day it matters.

Net: the registry is an indirection table that exists because Envoy's
cross-extension protocol is stringly typed. The right place to remove it
is the SDK, not orange — and only once the cost/benefit of an `up`-side
typed handoff is justified by more than one caller.

### What didn't land (and why)

- *TTL sweeper* — still rejected. Band-aid hiding the contract.
- *Architectural `OnStreamComplete` only, no Phase 2* — would have left
  the goroutine + global registry pattern in place. Phase 3's OnResolve
  is the actual structural cleanup.
- *Removing the global registry* — see the note above. Deferred, not
  declined.
