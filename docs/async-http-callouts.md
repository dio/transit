# Async HTTP Callouts

Transit HTTP filters can pause request processing, start Envoy HTTP callouts,
and resume the request after asynchronous work finishes.

Use callback-style `w.HTTPCallout` when the filter might send a local response.
The continuation runs from Envoy's callout callback, so `SendLocalResponse` is
honored. Use `w.Go` plus `w.Do` only for work that ultimately forwards the
request; local responses from the scheduler path are not reliable in Envoy.

## API Shape

```go
func Handler(w *up.Writer, r *up.Request) {
	_, err := w.HTTPCallout(up.HTTPCalloutRequest{
		Cluster:       "auth-service",
		Headers:       [][2]string{{":method", "POST"}, {":path", "/check"}, {"host", "auth-service.local"}},
		TimeoutMillis: 250,
	}, func(result up.HTTPCalloutResult, _ [][2]shared.UnsafeEnvoyBuffer, body []shared.UnsafeEnvoyBuffer) {
		if result != up.HTTPCalloutSuccess {
			w.SendLocalResponse(503, []byte(`{"error":"auth unavailable"}`))
			return
		}
		w.SetRequestHeader("x-auth-checked", "true")
	})
	if err != nil {
		w.SendLocalResponse(503, []byte(`{"error":"auth unavailable"}`))
	}
}
```

`headers` and `body` point into Envoy-owned memory. They are valid only for
the duration of the callback; copy any value that must outlive the call.

`w.HTTPCallout` returns `HTTPCalloutInitResult` immediately. If Envoy accepts
the callout, the request remains stopped until the continuation returns; Transit
then applies queued mutations and resumes the request, unless the continuation
sent a local response.

`w.Do` must be called inside `w.Go`. It schedules `HttpCallout` back onto the
Envoy stream thread, waits for the callout callback or context cancellation, and
then lets the goroutine queue request mutations before forwarding.

## Parallel Fan-Out

Multiple `w.Do` calls can be in flight at the same time. Use bounded
concurrency and join workers before mutating shared `Writer` state:

```go
w.Go(func(ctx context.Context) {
	var wg sync.WaitGroup
	results := make([]result, len(backends))
	sem := make(chan struct{}, 4)
	for i, backend := range backends {
		wg.Add(1)
		go func(i int, backend backend) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			resp, err := w.Do(ctx, backend.CalloutRequest())
			results[i] = result{resp: resp, err: err}
		}(i, backend)
	}
	wg.Wait()
	// Merge results, then mutate request headers before forwarding.
})
```

The first safe default fan-out limit is `4`. Raise it only after measuring with
real Envoy callouts and representative backend latency.

## Request Mirroring Is Different

Envoy `request_mirror_policies` are useful for shadow traffic, but they are not
a response fan-out primitive. Mirrored requests are fire-and-forget from the
client response path; the mirror response is ignored. Use mirror policies for
observability or migration experiments, not for MCP `tools/list` where the
client needs a merged response.

## Design invariants

### HTTPCallout path

`w.HTTPCallout` is mutex-free by design. `OnRequestHeaders` initiates the
callout; `OnHttpCalloutDone` fires the user callback and calls `w.flush(true)`.

`OnHttpCalloutDone` may fire from a different goroutine before `OnRequestHeaders`
returns — Envoy does not guarantee the callback is deferred. Transit handles
this with an atomic `calloutState` handoff (`Active→Paused/Done→Flushed`): if
the callback fires early, it transitions `Active→Done` and the headers callback
detects `Done` and flushes inline without calling `ContinueRequest`. If the
callback fires after the headers callback returns `Stop`, it transitions
`Paused→Flushed` and calls `ContinueRequest` to resume the request.

There is no mutex on this path. No extra struct is allocated.

### Go+Do path

`w.Go` launches a goroutine that is the sole writer to `Writer`'s mutation
slices. After the goroutine exits, `goScheduler.Schedule` hops back to the
Envoy worker thread to call `w.flush(true)`. `filter.goCompleted` is an
`atomic.Bool`; it protects the single race between the goroutine finishing and
`OnStreamComplete`. There is no mutex.

### Lock-while-CGO

Transit's own callout and Go+Do paths are structurally free of lock-while-CGO
deadlocks: the HTTPCallout path has no lock, and the Go+Do path uses
`atomic.Bool`, not a mutex.

**Custom code outside these primitives is still vulnerable.** Envoy can call
back into Go re-entrantly from within a CGO call. For example,
`envoy_dynamic_module_callback_http_send_response` may fire `OnStreamComplete`
before it returns. If a user-written wrapper holds a Go mutex during that CGO
call, it deadlocks against itself and the request hangs forever with no error.

If you write code that calls `SendLocalResponse`, `ContinueRequest`, or any
`handle.*` method: snapshot all state under the lock, release the lock, then
make the CGO calls with no lock held.

### `w.Do` + `w.SendLocalResponse` does not work

`w.Do` is valid only when the filter will **forward** the request. `SendLocalResponse`
called from inside `w.Go` is deferred through `Scheduler.Schedule` and Envoy
does not honor `SendLocalResponse` from scheduled callbacks -- only from
filter callbacks (`OnRequestHeaders`, `OnHttpCalloutDone`, etc.).

Use `w.HTTPCallout` (callback form) when the filter may reject the request with
a local response.

## Evaluation

The `up` package includes `BenchmarkWriterDoFanout` to compare wrapper overhead
for sequential and parallel fake callouts. Fake callouts complete inline, so
they do not show the latency benefit of parallel I/O. Use them only to track
allocation and wrapper overhead.

Before moving MCP fan-out fully into a dynamic module, add an Envoy e2e
benchmark that compares:

- sequential Envoy `HttpCallout`
- parallel Envoy `HttpCallout` callback aggregation at widths 2, 4, 8, and 16
- a service-level aggregator using normal Go HTTP clients

The decision gate is whether in-filter parallel callouts are efficient enough
at expected MCP profile fan-out sizes.
