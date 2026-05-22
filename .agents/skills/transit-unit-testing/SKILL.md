---
name: transit-unit-testing
description: Write, run, and debug transit unit tests. Use when adding Go tests, choosing between unit and e2e coverage, running make test, handling race tests, or working around Linux linker behavior for down/abi_impl tests.
---

# Transit Unit Testing

Use this skill for non-e2e tests in transit: root packages, `down`, `up`,
`up/testutil`, `down/abi_impl` helper tests, and example package tests.

## Test boundaries

- Unit tests should exercise pure Go behavior directly.
- Use `up/testutil` for HTTP filter handler tests instead of starting Envoy.
- Use e2e tests for behavior that requires Envoy, dynamic module loading,
  C callback execution, access-log callbacks, real routing, or actual `.so`
  lifecycle.
- Do not call Envoy C callbacks from ordinary unit tests. They are provided by
  the running Envoy process, not by Go test binaries.

## Main commands

Root unit/race suite:

```
make test
```

Equivalent:

```
go test -race ./...
```

Examples module:

```
cd examples && GOWORK=off go test ./...
cd examples && GOWORK=off go test ./<name>/...
```

If the sandbox blocks the default Go build cache, set a writable cache:

```
GOCACHE=/private/tmp/transit-gocache make test
```

## Linux abi_impl linker rule

`down/abi_impl` contains CGO wrappers that reference
`envoy_dynamic_module_callback_*` symbols. In production those unresolved
symbols are supplied by Envoy when it loads the module `.so`.

Ordinary Linux test binaries are linked strictly, so `go test ./down/abi_impl`
fails unless `down/abi_impl/internal.go` keeps:

```
#cgo linux LDFLAGS: -Wl,--unresolved-symbols=ignore-all
```

Darwin has the analogous:

```
#cgo darwin LDFLAGS: -Wl,-undefined,dynamic_lookup
```

Do not remove either line just because tests pass on macOS. Linux CI is the
source of truth for this failure mode.

## abi_impl unit tests

Keep `down/abi_impl` unit tests narrow:

- Good: `manager[T]` record/unwrap/remove behavior.
- Good: scheduler bookkeeping that does not call C.
- Good: `down.ClusterLBCompletion` completion/cancel idempotence.
- Risky: tests that instantiate handles and call methods backed by C callbacks.
- Wrong: tests that expect Envoy callback symbols to exist outside Envoy.

For wrapper behavior that must call C callbacks, add e2e coverage instead.

## Blank import rule

Do not fix linker failures by blank-importing `down/abi_impl` from `up` or other
library packages. That makes ordinary test binaries pull in CGO exports and
unresolved Envoy callbacks.

The blank import belongs only in `cmd/main.go` entrypoints that are built with:

```
go build -trimpath -buildmode=c-shared
```

Examples:

```
import _ "github.com/dio/transit/down/abi_impl"
```

in:

- `e2e/cmd/main.go`
- `examples/<name>/cmd/main.go`

## Race tests

`make test` runs `go test -race ./...`, so new unit tests must be race-clean.

- Avoid package-level mutable state unless protected or reset.
- For tests that touch registries, use unique names per test or isolate the
  package state explicitly.
- Use `t.Parallel()` only when the tested state is isolated.
- Prefer `sync/atomic`, mutexes, or channels for assertions involving goroutine
  coordination.

## Embed fixtures in unit tests

Go typechecking validates `//go:embed` patterns before tests run. If a package
embeds generated assets, a clean checkout must still contain the embedded path
or lint/typecheck fails in CI.

For examples with embedded static assets, keep a minimal tracked fixture under
the embedded path, or avoid compiling that package in unit/lint targets.

## Before finishing

For root package changes:

```
make test
```

For example changes:

```
cd examples && GOWORK=off go test ./<name>/...
```

If a change touches shared APIs or package layout, run both root and examples
tests.
