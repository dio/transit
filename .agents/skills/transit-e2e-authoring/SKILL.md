---
name: transit-e2e-authoring
description: Create or extend transit end-to-end tests. Use when adding e2e filters, Envoy test config, sinks, assertions, or example e2e suites for transit dynamic modules.
---

# Transit E2E Authoring

Use this skill when adding new e2e coverage for transit or its examples.

## Choose the right suite

- Use root `e2e/` for core transit API behavior, ABI wrapper coverage, access
  logger behavior, body handling, metadata, telemetry, upstream filters, and LB
  Policy behavior.
- Use `examples/<name>/e2e/` when validating a specific example's user-facing
  behavior.
- Add a new sink under `e2e/sinks/` only when existing HTTP JSON, OTLP, or ALS
  sinks cannot observe the behavior.

## Root e2e structure

```
e2e/
  cmd/main.go                  imports e2e/filters for shared-library build
  filters/*.go                 test dynamic modules registered in init
  testdata/envoy.yaml.tmpl     Envoy bootstrap template
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
   `e2e/testdata/envoy.yaml.tmpl`.
4. Add a port field and `freePort()` allocation in `e2e/main_test.go` when the
   test needs a new listener, admin endpoint, or upstream.
5. Add a focused `*_test.go` file or extend the relevant suite.
6. Run `make e2e` or a direct package command with `ENVOY_BIN`.

Keep test filter names stable and obvious: `e2e-<feature>` for filters and
`<feature>-e2e` for listener/stat prefixes.

## Assertions

- Prefer black-box HTTP requests through Envoy over direct calls into filter
  code.
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
examples/<name>/e2e/testdata/envoy.yaml.tmpl
```

The example `TestMain` normally:

- locates `examples/` with `runtime.Caller`,
- checks `ENVOY_BIN` or `../.bin/envoy`,
- builds `lib<name>.so` with `go build -trimpath -buildmode=c-shared`; the
  example `cmd/main.go` must blank-import `github.com/dio/transit/down/abi_impl`
  so Envoy ABI exports are linked only into the `.so`,
- starts any in-process upstream/test server,
- renders a temp Envoy config,
- starts Envoy with `GODEBUG=cgocheck=0` and
  `ENVOY_DYNAMIC_MODULES_SEARCH_PATH=<example dir>`,
- waits for admin `/ready`,
- runs tests and tears Envoy down.

Reuse the nearest existing example e2e harness (`hello`, `lb-policy`,
`sse-tap`, or `request-ui`) rather than writing a new harness from memory.

If an e2e example depends on embedded static assets, keep a minimal tracked
fixture under the embedded path. CI lint/typecheck runs from a clean checkout
and will fail on `//go:embed` patterns that only exist after a local asset build.

## Validation commands

Root e2e:

```
make e2e
```

Example e2e:

```
make e2e-hello
make e2e-sse-tap
make e2e-request-ui
make e2e-lb-policy
```

Fast rerun after a successful build:

```
TRANSIT_SKIP_BUILD=1 make e2e
```

Do not rely on `TRANSIT_SKIP_BUILD=1` for final verification after code changes.
