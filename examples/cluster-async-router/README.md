# cluster-async-router

Body-driven cluster routing using the async `ClusterLBCompletion` pattern
documented at `docs/envoy-dynamic-module-upstream-selection.md` (lines 154–235).

The HTTP filter inspects a JSON request body of the form `{"target":"<name>"}`
and routes to the named upstream registered with the dynamic-modules cluster.

## Why async

`ChooseHost` runs before the body callback. A naive design that writes filter
state from the body handler is too late — the cluster has already returned
nil. This example uses the async completion path:

1. At request-headers phase, mint a per-request token, register a `Pending`,
   and write the token to filter state (`body-router.token`).
2. `ChooseHost` reads the token, looks up the `Pending`, and returns
   `(nil, completion)`. A goroutine parks on `Pending.Done`.
3. The body handler parses `target`, calls `Pending.Resolve(...)`. The waiter
   wakes, hops back to the cluster main thread via `handle.Schedule`, and
   calls `completion.Complete(host, "")`.

See `.agents/skills/transit-body-driven-cluster-routing/SKILL.md` for the
phase-ordering background.

## Run

```bash
make download-envoy
make e2e            # build + run integration tests
make run            # run locally on :10000 (expects upstreams on 8081/8082)
```

Example request:

```bash
curl -s -XPOST -H 'content-type: application/json' \
  --data '{"target":"a"}' http://127.0.0.1:10000/
```
