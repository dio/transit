---
name: transit-e2e-authoring
description: Create or extend transit end-to-end tests. Use when adding e2e filters, Envoy test config, sinks, assertions, or example e2e suites for transit dynamic modules.
---

# Transit E2E Authoring

Use this skill when adding new e2e coverage for transit or its examples.

## Choose the right suite

- Use root `e2e/` for core transit API behavior, ABI wrapper coverage, access
  logger behavior, body handling, metadata, telemetry, upstream filters, LB
  Policy behavior, and Cluster Extension behavior.
- Use `examples/<name>/e2e/` when validating a specific example's user-facing
  behavior.
- Every example should have e2e coverage and a local `make -C examples/<name>
  e2e` entry point. Do not add per-example e2e targets to the root `Makefile`.
- Add a new sink under `e2e/sinks/` only when existing HTTP JSON, OTLP, or ALS
  sinks cannot observe the behavior.

## Root e2e structure

```
e2e/
  cmd/main.go                  imports e2e/filters for shared-library build
  filters/*.go                 test dynamic modules registered in init
  testdata/envoy.tmpl.yaml     Envoy bootstrap template
  main_test.go                 builds libe2e.so, starts Envoy and sinks
  *_test.go                    feature assertions
  sinks/*                      in-process assertion sinks
```

To add a root e2e case:

1. Add or update a test filter in `e2e/filters`. Register it with the same public
   API a user would call (`up.Register`, `RegisterWithBody`,
   `RegisterAccessLogger`, `RegisterLBPolicy`, etc.).
2. Ensure `e2e/cmd/main.go` blank-imports both
   `github.com/dio/transit/down/abi_impl` and
   `github.com/dio/transit/e2e/filters`. The `abi_impl` blank import belongs in
   the shared-library entrypoint, not in `up` or another library package.
3. Add listener, filter, cluster, access logger, or sink config to
   `e2e/testdata/envoy.tmpl.yaml`.
4. Add a port field and `freePort()` allocation in `e2e/main_test.go` when the
   test needs a new listener, admin endpoint, or upstream.
5. Add a focused `*_test.go` file or extend the relevant suite.
6. Run `make e2e` or a direct package command with `ENVOY_BIN`.

Keep test filter names stable and obvious: `e2e-<feature>` for filters and
`<feature>-e2e` for listener/stat prefixes.

For upstream selection work, check both APIs deliberately. LB Policy e2e
coverage does not cover Cluster Extension behavior, and Cluster Extension e2e
coverage does not cover LB Policy behavior.

## Harness Shapes

Choose the right in-process upstream or listener for each test scenario:

- **Direct-response listener** — for filters that terminate locally or mutate
  headers without an upstream dependency. Current: echo, guard, body, cluster-scheduler.
- **Plain upstream echo** — for asserting forwarded request headers/body reached
  upstream. Returns received request headers and/or body so tests can assert
  what Envoy actually forwarded.
- **Callout upstream** — for HTTPCallout/Do. Path echo controls which response
  the callout callback sees; header echo controls what the callout can read and
  set on the forwarded request.
- **Buffered body upstream** — for framing correctness and body replacement.
  Must return the observed request body and its content-length so tests can
  assert the correct value was forwarded, not the stale original.
- **Telemetry sink** — for OTLP/ALS/access log assertions with bounded waits.
  Never sleep; use `Wait*` predicates.
- **Cluster/LB upstream set** — for LB Policy and Cluster Extension host
  selection. Tests need multiple hosts to verify distribution.
- **TLS/mTLS upstream** — for transport-socket behavior owned by Envoy config.
  Generated cert material is in a temp dir; never commit it.
- **Scheduler probe listener** — minimal `direct_response` route used only to
  prove that a scheduled callback runs on the worker thread.

## Selection Rules

- If the feature changes what Envoy **forwards**, use an upstream echo harness.
- If the feature **terminates locally**, assert the client response and avoid
  starting an upstream unless the test must prove the upstream was NOT reached.
- If the feature involves **async resume** (HTTPCallout, Go+Do, scheduler),
  prefer root `e2e/` over `examples/` so ABI regressions are caught centrally.
- If `HTTPCallout` is initiated from a request body callback, add root e2e
  coverage that proves the local-response path and, when relevant, body
  replacement on resume. Unit tests can validate the state machine, but only
  Envoy verifies that body `StopAndBuffer` actually prevents fallback routing
  before the callout callback runs. Do not assert late request header mutations
  from body callbacks unless headers were intentionally stopped earlier.
- If a new `HTTPCalloutAllSettled` user is added, keep one root e2e case that proves
  multiple Envoy callout callbacks can be merged into one local response. Then
  add example-level e2e for the concrete workflow that uses wait-all fan-out.
- If **body replacement** is involved, the harness must assert the upstream-
  observed body length/content, not just the client-visible status code.
- If **multiple Transit APIs compose**, write one minimal root regression per
  API plus one example-level workflow test — do not merge them.

## Reusable Helpers

Before adding a new `start*Upstream` function, check whether an existing
upstream already covers the scenario. For new upstreams, follow the pattern in
`startAsyncCalloutUpstream` and add the port to the template data struct.

Echo helper checklist — a single upstream can serve multiple test scenarios if
it exposes:
- `method` and `path` of the forwarded request
- selected request headers (e.g., `authorization`, custom `x-*` headers)
- request body verbatim and its observed `content-length`
- a boolean "did the request arrive" (use a channel or atomic counter so tests
  can assert "not forwarded" without sleeps)

Add a reusable "upstream recorder" type with a bounded `Wait` that returns the
first observed request. Use it instead of `require.Eventually` + sleep for
tests that must assert forwarding behavior.

## Assertions

- Prefer black-box HTTP requests through Envoy over direct calls into filter
  code.
- Prefer `github.com/stretchr/testify/require` for test assertions and setup
  checks. Use `require.NoError`, `require.Equal`, `require.Contains`, and
  `require.Eventually` before hand-written `if ... t.Fatalf` blocks unless a
  custom helper needs a clearer failure message.
- Use existing helpers such as `mustDo`, `readBody`, `waitReady`, and sink
  `Wait*` methods when available.
- For asynchronous telemetry or access-log assertions, use bounded waits with
  predicates instead of sleeps.
- Reset shared sinks between tests if data from earlier requests can satisfy a
  later predicate.
- Assert both the externally visible behavior and the specific transit feature
  under test when possible.

## Example e2e structure

Example e2e suites usually live at:

```
examples/<name>/e2e/e2e_test.go
examples/<name>/e2e/testdata/envoy.tmpl.yaml
```

Browser-based examples can use a script-backed `examples/<name>/e2e/` harness
instead of Go tests, but the example Makefile must still expose `e2e`.

The example `TestMain` normally:

- locates `examples/` with `runtime.Caller`,
- checks `ENVOY_BIN` or `../.bin/envoy`,
- verifies that `lib<name>.so` already exists; the example Makefile owns
  building the `.so`, including the `go build -trimpath -buildmode=c-shared`
  command,
- starts any in-process upstream/test server,
- embeds `testdata/envoy.tmpl.yaml` with `//go:embed` and renders a temp Envoy
  config,
- starts Envoy with `GODEBUG=cgocheck=0` and
  `ENVOY_DYNAMIC_MODULES_SEARCH_PATH=<example dir>`,
- waits for admin `/ready`,
- runs tests and tears Envoy down.

Generated e2e shared libraries and c-shared headers are build artifacts. Clean
or ignore `e2e/libe2e.so`, `e2e/libe2e.h`, `examples/*/lib*.so`, and
`examples/*/lib*.h`; never commit them.

Reuse the nearest existing example e2e harness (`hello`, `lb-policy`,
`sse-tap`, or `request-ui`) rather than writing a new harness from memory.
When a helper is shared only by example e2e suites, put it in the examples
module, for example `examples/internal/e2etest`. Do not import the separate
root `e2e` module from `examples/` just to share small harness helpers.

If an e2e example depends on embedded static assets, keep a minimal tracked
fixture under the embedded path. CI lint/typecheck runs from a clean checkout
and will fail on `//go:embed` patterns that only exist after a local asset build.

For Cluster Extension refresh coverage, distinguish bootstrap host discovery
from live cluster mutation. If `ClusterHandle.Schedule` is involved, keep a
minimal root e2e regression for scheduler dispatch in addition to the example
e2e. This makes ABI scheduler regressions obvious instead of hiding them behind
DNS, config fetch, or host mutation behavior.

## Validation commands

Root e2e:

```
make e2e
```

Example e2e:

```
make -C examples/hello e2e
make -C examples/sse-tap e2e
make -C examples/request-ui e2e
make -C examples/lb-policy e2e
make -C examples/cluster e2e
make -C examples/cluster-dfp e2e
make -C examples/spa e2e
```

For new Go-backed example e2e targets, include `-count=1` in the Makefile
target so Envoy actually starts during verification instead of reusing cached
`go test` results.

Example e2e tests must not run `go build` themselves. Wire the local Makefile
as `e2e: build`, and have the Go e2e verify that the expected `.so` is present
before starting Envoy. This keeps build responsibility in one place and makes
direct `go test ./<name>/e2e/...` fail with a clear "run make build" message.

Fast rerun after a successful build:

```
TRANSIT_SKIP_BUILD=1 make e2e
```

Do not rely on `TRANSIT_SKIP_BUILD=1` for final verification after code changes.

## Debugging: filter accepts connection but never responds

If an e2e test hangs (client connects, Envoy accepts, no response ever arrives),
work through these steps in order.

For transit's built-in callout path:

1. Verify `f.calloutFn` is set inside `Writer.HTTPCallout` before the callout
   is initiated.

2. Verify `f.calloutFn` is non-nil when `OnHttpCalloutDone` fires. If it is
   nil the callback has no flush path and the stream hangs. (`calloutFn`
   replaced the former `calloutWriter` atomic pointer in Phase 2.)

3. Check the e2e filter registration name matches the `filter_name` in
   `e2e/testdata/envoy.tmpl.yaml`. A name mismatch means the filter is never
   instantiated and the callout callback never fires.

4. Add `w.Log` calls before and after `w.flush(true)` in the callout callback
   and rebuild. If the log before fires but the one after does not, inspect
   the mutations queued on `Writer` and whether `ContinueRequest` or
   `SendLocalResponse` was called.

For custom user code that wraps handle methods:

5. Search for `defer s.mu.Unlock()` or `s.mu.Lock()` scopes that contain any
   `handle.*` call. That is a lock-while-CGO deadlock. Transit's own callout
   path is mutex-free by design, but custom wrappers can still introduce this.

6. Fix custom code: snapshot all state under the lock, release, then call
   handle methods with no lock held. See `docs/async-http-callouts.md`.

This class of bug passes all unit tests. `FakeFilterHandle` is pure Go and
never re-enters Go from C, so the deadlock only surfaces against a real Envoy
binary. Do not conclude the filter is correct just because unit tests pass --
e2e coverage of every async code path is mandatory.
