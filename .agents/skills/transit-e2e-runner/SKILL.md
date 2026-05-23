---
name: transit-e2e-runner
description: Run and debug transit end-to-end tests. Use when executing e2e suites, choosing Make targets, reusing built shared libraries, handling Envoy binary setup, or interpreting transit e2e failures.
---

# Transit E2E Runner

Use this skill when running or debugging transit e2e tests against a real Envoy
binary.

## Prerequisites

The e2e harness needs an Envoy binary matching the vendored dynamic module SDK.
The normal path is:

```
make download-envoy
```

`make e2e` depends on `.bin/envoy` automatically. Example e2e suites are run
through their own Makefiles, which delegate to the root `download-envoy` target
when needed. Direct `go test` commands must set `ENVOY_BIN`; otherwise tests
skip.

## Main commands

From the repo root:

```
make e2e
make -C examples/hello e2e
make -C examples/sse-tap e2e
make -C examples/request-ui e2e
make -C examples/lb-policy e2e
make -C examples/cluster e2e
make -C examples/cluster-dfp e2e
make -C examples/spa e2e
```

Direct equivalents:

```
cd e2e && ENVOY_BIN=../.bin/envoy go test ./... -v -timeout=30s
cd examples && ENVOY_BIN=../.bin/envoy GOWORK=off go test ./hello/e2e/... -v -timeout=60s
cd examples && ENVOY_BIN=../.bin/envoy GOWORK=off go test ./sse-tap/e2e/... -v -timeout=60s
cd examples && ENVOY_BIN=../.bin/envoy GOWORK=off go test ./request-ui/e2e/... -v -timeout=120s
cd examples && ENVOY_BIN=../.bin/envoy GOWORK=off go test ./lb-policy/e2e/... -v -timeout=60s
cd examples && ENVOY_BIN=../.bin/envoy GOWORK=off go test ./cluster/e2e/... -v -timeout=60s
cd examples && ENVOY_BIN=../.bin/envoy GOWORK=off go test ./cluster-dfp/e2e/... -v -timeout=60s
```

Use `TRANSIT_SKIP_BUILD=1` for faster iteration after a successful shared
library build:

```
TRANSIT_SKIP_BUILD=1 make e2e
```

Do not use `TRANSIT_SKIP_BUILD=1` after changing Go code that is compiled into
the dynamic module.

## What the root e2e harness does

`e2e/main_test.go`:

- allocates loopback ports,
- starts in-process sinks for custom access logs, OTLP, and ALS,
- builds `e2e/libe2e.so` from `e2e/cmd`,
- renders the embedded `e2e/testdata/envoy.tmpl.yaml`,
- starts Envoy with `GODEBUG=cgocheck=0` and
  `ENVOY_DYNAMIC_MODULES_SEARCH_PATH=<repo>/e2e`,
- waits for the Envoy admin `/ready` endpoint,
- runs feature tests, then kills Envoy and removes the temp config.

When Envoy fails to start, inspect stderr first. The harness streams build and
Envoy logs to test stderr.

## Debugging workflow

- If a suite prints `SKIP: envoy not found`, run `make download-envoy` or set
  `ENVOY_BIN`.
- If the `.so` build fails, rerun without `TRANSIT_SKIP_BUILD=1` and fix the
  compile error in the relevant module (`e2e` or `examples`).
- If Envoy is not ready in time, inspect the generated config area in the log,
  dynamic module loading errors, listener bind conflicts, and admin readiness.
- If a sink assertion times out, check the filter registration name in
  `e2e/filters`, the matching `filter_name` or `logger_name` in
  `e2e/testdata/envoy.tmpl.yaml`, and whether the test resets or waits on the
  right sink.
- For flaky port behavior, rerun the single package first; ports are allocated
  dynamically but still have a close-and-bind race.
- In restricted sandboxes, e2e can fail before Envoy starts with
  `listen tcp 127.0.0.1:0: bind: operation not permitted`. That is a sandbox
  limitation, not an e2e assertion failure; rerun the relevant `make -C
  examples/<name> e2e` target with permission to bind loopback ports.

## Before finishing

For changes that affect e2e behavior, run the narrow e2e target first. If the
change touches shared transit APIs or wrappers, also run:

```
go test ./...
```

Use the root `make e2e` when the feature is covered by the consolidated `e2e`
suite.
