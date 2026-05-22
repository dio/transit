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
  cluster/
    cluster.go
    cmd/main.go
    cluster_test.go
    e2e/
  cluster-dfp/
    cluster_dfp.go
    cmd/main.go
    cluster_dfp_test.go
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
examples/<name>/Makefile
examples/<name>/e2e/e2e_test.go
examples/<name>/e2e/testdata/envoy.tmpl.yaml
```

Larger examples may add local packages, UI assets, browser e2e assets, or
additional helper commands.

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
7. Add e2e coverage for every example. Keep it focused on the user-facing
   behavior the example exists to demonstrate.
8. Add `examples/<name>/Makefile` with at least `build`, `test`, `e2e`, `run`,
   `clean`, and `download-envoy`. Do not add per-example targets to the root
   `Makefile`; examples own their local lifecycle.

Do not put CGO details in examples. The only `down/abi_impl` detail examples
should carry is the required blank import in `cmd/main.go`.

For example e2e suites, prefer embedding the Envoy config template:

```
//go:embed testdata/envoy.tmpl.yaml
var envoyConfigTmpl string
```

Render the embedded template to a temp file before starting Envoy. This keeps
the test self-contained and makes missing template files fail at compile time.
If multiple example e2e suites need small shared harness helpers, keep them in
the examples module, such as `examples/internal/e2etest`, so the examples module
does not depend on the separate root `e2e` module.

For examples that use `//go:embed`, make sure the embedded files exist in a
clean checkout. `golangci-lint` typechecks packages in CI and fails on missing
embed patterns before any build script can generate assets. If a small built
fixture is intentional, unignore and track that exact fixture.

## Upstream selection examples

LB Policy and Cluster Extension are separate APIs. Do not treat coverage or
examples for one as coverage for the other.

An LB Policy example should show Envoy owning the host set while the module
implements `LBPolicy.ChooseHost`.

A Cluster Extension example should show the lifecycle users need to copy:

1. Parse config in `ClusterFactory.Create`.
2. Create a cluster from `ClusterConfigFactory.NewCluster`.
3. Add hosts and call `ClusterHandle.PreInitComplete` in `Cluster.Init`.
4. Return a `ClusterLB` from `Cluster.NewClusterLB`.
5. Choose a healthy host in `ClusterLB.ChooseHost`.

A DFP-style Cluster Extension example should stay separate from the basic
cluster example. Use request headers or filter state as the target signal,
resolve or look up the target asynchronously, mutate hosts through
`ClusterHandle.Schedule`, then complete host selection with
`ClusterLBCompletion`.

For cluster-router-style examples, live refresh is valid only when there is
separate root e2e coverage for `ClusterHandle.Schedule` dispatch. Keep example
e2e focused on the user-visible behavior, and use the root scheduler probe to
catch ABI callback regressions.

## Per-example Makefile

Every example should have its own Makefile so users can work locally without
remembering root target names:

```
make -C examples/<name> build
make -C examples/<name> test
make -C examples/<name> e2e
make -C examples/<name> run
```

Use the examples module for Go commands inside those targets:

```
cd $(ROOT)/examples && GOWORK=off go test ./<name>/...
```

Prefer the standard `EXAMPLE := <name>` variable and use `$(EXAMPLE)` in build,
test, e2e, and helper targets. This keeps examples consistent and avoids
hard-coded paths drifting when Makefiles are copied.

Build shared libraries into the example directory for local runs:

```
cd $(ROOT)/examples && CGO_ENABLED=1 GOWORK=off go build -trimpath -buildmode=c-shared \
  -o <name>/lib<name>.so ./<name>/cmd
```

Generated shared libraries and c-shared headers must stay out of git. The repo
ignores `*.so`, `dist/*.h`, `e2e/libe2e.h`, and `examples/*/lib*.h`; Makefile
`clean` targets should remove both `$(OUTPUT)` and `$(OUTPUT:.so=.h)`.

## E2E expectations

Every example must have an e2e entry point in its own Makefile:

```
.PHONY: e2e
e2e:
	cd $(ROOT)/examples && ENVOY_BIN=$(ENVOY_BIN) GOWORK=off go test ./<name>/e2e/... -v -timeout=60s -count=1
```

SPA/browser examples may use a script-backed e2e target, but `make -C
examples/<name> e2e` should remain the public command.

Use `transit-e2e-authoring` for the test harness details.

## Validation

For an example-only change:

```
make -C examples/<name> test
```

For examples module-wide checks:

```
cd examples && GOWORK=off go test ./...
```

For example e2e coverage, run `make -C examples/<name> e2e` after
`make download-envoy` has populated `.bin/envoy`.

Use `transit-unit-testing` for ordinary example unit-test issues and
`transit-e2e-authoring` for Envoy-backed coverage.
