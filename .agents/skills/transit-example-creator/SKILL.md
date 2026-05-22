---
name: transit-example-creator
description: Create or update transit examples. Use when adding an example dynamic module, example command, Envoy config, tests, e2e suite, or Make target under examples/.
---

# Transit Example Creator

Use this skill when creating or changing examples under `examples/`.

## Existing shape

Examples are separate from the root module and are tested with `GOWORK=off` from
the `examples` module.

```
examples/
  go.mod
  hello/
    hello.go
    cmd/main.go
    hello_test.go
    e2e/
  lb-policy/
    lb_policy.go
    cmd/main.go
    lb_policy_test.go
    e2e/
  sse-tap/
  request-ui/
  spa/
```

Simple examples use:

```
examples/<name>/<name>.go
examples/<name>/cmd/main.go
examples/<name>/<name>_test.go
examples/<name>/e2e/e2e_test.go
examples/<name>/e2e/testdata/envoy.yaml.tmpl
```

Larger examples may add local packages, UI assets, or a local `Makefile`.

## Implementation workflow

1. Pick a short, filesystem-safe example name. Keep package names valid Go
   identifiers; hyphenated directories can use compact package names like
   `lbpolicy`.
2. Put reusable filter or policy logic in `examples/<name>/<name>.go`.
3. Put dynamic module registration in `examples/<name>/cmd/main.go` or in the
   package `init` when that is already the example's pattern.
4. Register through `up` exactly as users should copy it:
   `up.Register`, `RegisterWithResponse`, `RegisterWithBody`,
   `RegisterWithMutableBody`, `RegisterWithGroup`, `RegisterAccessLogger`,
   `RegisterCluster`, or `RegisterLBPolicy`.
5. Every example shared-library entrypoint must blank-import
   `github.com/dio/transit/down/abi_impl` in `examples/<name>/cmd/main.go`.
   Do not move that blank import into `up` or any library package: Linux rejects
   normal test binaries with unresolved Envoy callback symbols, while the `.so`
   is resolved by Envoy at runtime.
6. Add unit tests for pure Go behavior where possible.
7. Add e2e coverage when the feature depends on Envoy behavior, dynamic module
   loading, upstream filters, telemetry, access logging, body mutation, or LB
   selection.

Do not put CGO details in examples. The only `down/abi_impl` detail examples
should carry is the required blank import in `cmd/main.go`.

For examples that use `//go:embed`, make sure the embedded files exist in a
clean checkout. `golangci-lint` typechecks packages in CI and fails on missing
embed patterns before any build script can generate assets. If a small built
fixture is intentional, unignore and track that exact fixture.

## Build and run commands

Build an example shared library from the repo root:

```
make build EXAMPLE=<name> EXAMPLE_CMD=./examples/<name>/cmd
```

Run with Envoy:

```
make run EXAMPLE=<name> ENVOY_YAML=$PWD/examples/<name>/envoy.yaml
```

For direct commands from `examples/`, use `GOWORK=off`:

```
cd examples && GOWORK=off go test ./<name>/...
```

## E2E expectations

If adding example e2e, also add or update a root Make target when the example is
meant to be run routinely:

```
.PHONY: e2e-<name>
e2e-<name>: $(ENVOY_BIN)
	cd examples && ENVOY_BIN=$(ENVOY_BIN) GOWORK=off go test ./<name>/e2e/... -v -timeout=60s
```

Use `transit-e2e-authoring` for the test harness details.

## Validation

For an example-only change:

```
cd examples && GOWORK=off go test ./<name>/...
```

For examples module-wide checks:

```
cd examples && GOWORK=off go test ./...
```

For examples with e2e coverage, run the matching `make e2e-<name>` target after
`make download-envoy` has populated `.bin/envoy`.

Use `transit-unit-testing` for ordinary example unit-test issues and
`transit-e2e-authoring` for Envoy-backed coverage.
