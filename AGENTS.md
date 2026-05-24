# Transit — Agent Guide

This file is loaded automatically by Hermes and compatible AI coding agents
when they operate inside this repository. Read it before writing any code.

---

## Repo layout

```
transit/                     root Go module: github.com/dio/transit
  down/                      Envoy ABI wrappers (CGO, never import directly)
    abi_impl/                Envoy callback exports — blank-import in cmd/main.go ONLY
  up/                        Public API: Register*, Writer, Request, Cluster, LBPolicy
  e2e/                       Root e2e suite (separate Go module, GOWORK=off)
    cmd/main.go              Shared-library entrypoint for root e2e filters
    filters/                 Test dynamic modules registered in init()
    testdata/                Envoy config templates
    sinks/                   In-process assertion sinks (OTLP, ALS, access-logger)
  examples/                  Standalone Go module (GOWORK=off), one dir per example
    internal/e2etest/        Shared e2e harness helpers (examples module only)
    hello/                   Minimal HTTP filter
    sse-tap/                 SSE response body tap, token usage extraction
    cluster-dfp/             Cluster Extension with DNS host resolution
    cluster-router/          Cluster Extension with live config refresh
    cluster-shard-router/    Cluster Extension with shard-based routing
    lb-policy/               LB Policy host selection
    mcp-profile-router/      MCP fan-out aggregator
    request-ui/              Filter with embedded UI served via access logger
    spa/                     Static asset serving filter
    ws-proxy/                WebSocket proxy with embedded upstream dial (see README.md)
  integrations/              K8s / Envoy Gateway integration tests (separate module)
    cluster-router-eg/
    tiered-router-eg/
    mcp-profile-tiered-router-eg/
  .agents/skills/            In-repo agent skills (load these for relevant tasks)
```

---

## Skills to load

**Always check `.agents/skills/` first.** These skills encode project-specific
conventions that override general knowledge. Load the relevant skill before
writing code, not after.

| Task | Skill |
|------|-------|
| Create or change an example under `examples/` | `transit-example-creator` |
| Add e2e tests (root or example) | `transit-e2e-authoring` |
| Write unit tests, choose unit vs e2e | `transit-unit-testing` |
| Work with Envoy dynamic module ABI (`down/`, `up/`) | `transit-envoy-dynamic-modules` |
| Work with `down/abi_impl` CGO wrappers | `transit-envoy-abi-wrapper` |
| Run e2e suites, debug Envoy startup | `transit-e2e-runner` |
| Add or debug k3d / Envoy Gateway integrations | `transit-k3d-envoy-gateway-e2e` |
| Design L1/L2 tiered router scenarios | `transit-tiered-router-design` |

Load with (Hermes):
```
skill_view(name="transit-example-creator")
```

---

## Non-negotiable rules

### 1. Blank import placement
`down/abi_impl` contains the Envoy CGO callback exports. It MUST be
blank-imported only in shared-library entrypoints:

```go
// examples/<name>/cmd/main.go or e2e/cmd/main.go
import _ "github.com/dio/transit/down/abi_impl"
```

Never in `up/`, `down/` (non-abi_impl), or reusable library packages.
On Linux, ordinary test binaries fail to link if they pull in unresolved
Envoy callback symbols.

### 2. Register from init(), not main()
Envoy loads the `.so` and calls `on_*_config_new` callbacks. It never
calls `main()`. Filters not registered from `init()` are invisible to Envoy.

```go
func init() {
    up.Register("my-filter", handler)
}
func main() {}
```

### 3. Module isolation for examples
`examples/` is a separate Go module. Always run example tests with:
```sh
cd examples && GOWORK=off go test ./<name>/...
```
Never import from the root `e2e` module into `examples/`. Shared harness
helpers belong in `examples/internal/e2etest`.

### 4. `.so` files are build artifacts
Never commit `*.so` or `lib*.h` files. They are in `.gitignore`.
`Makefile clean` targets must remove both `$(OUTPUT)` and `$(OUTPUT:.so=.h)`.

### 5. No CGO in handler code
Filter logic in `examples/<name>/<name>.go` must be pure Go. CGO belongs in
`down/abi_impl` only. If something feels like it needs CGO in filter code,
it belongs in `up/` as a new abstraction.

### 6. Lock-while-CGO deadlock
`FakeFilterHandle` in `up/testutil` is pure Go -- it does not call C.
A filter that holds a Go mutex while calling any `Writer` or handle method
passes all unit tests and deadlocks silently against real Envoy. If a
filter hangs in e2e after passing unit tests, search for mutexes wrapping
handle calls. Fix: snapshot state under the lock, release, then call handle.

### 7. Envoy version
The reference Envoy binary is 1.38.0, downloaded by `make download-envoy`
in each example. Do not require 1.39-dev features (filter state in cluster
LB, PR #45040) in any example. Flag clearly if a feature needs 1.39+.

---

## Key up/ API surface

```go
// HTTP filters
up.Register(name, handlerFunc)
up.RegisterWithResponse(name, onReq, onResp)
up.RegisterWithBody(name, onReq, onBody, onResp)
up.RegisterWithMutableBody(name, onReq, onBody, onResp)
up.RegisterWithConfig(name, configFunc, onReq, onResp)
up.RegisterWithGroup(name, group, onReq)

// Background goroutines tied to filter config lifetime
g := up.NewGroup()
g.Go(func(ctx context.Context) { ... })
up.RegisterWithGroup("name", g, handler)

// Cluster Extension (module owns host set)
up.RegisterCluster(name, clusterFactory)

// LB Policy (Envoy owns host set, module chooses)
up.RegisterLBPolicy(name, lbPolicyFactory)

// Access loggers (called after stream completes)
up.RegisterAccessLogger(name, factory)
```

Handler signatures:
```go
type HandlerFunc         func(w *Writer, r *Request)
type ResponseHandlerFunc func(w *Writer, chunk *ResponseChunk)
type RequestBodyHandlerFunc func(w *Writer, chunk *RequestChunk)
```

---

## e2e harness conventions

- TestMain: locate repo root via `runtime.Caller`, check `ENVOY_BIN` or
  `../.bin/envoy`, verify `.so` exists (Makefile builds it), start
  in-process servers, embed `testdata/envoy.tmpl.yaml` with `//go:embed`,
  render to temp file, start Envoy with `GODEBUG=cgocheck=0` and
  `ENVOY_DYNAMIC_MODULES_SEARCH_PATH=<dir>`, wait for admin `/ready`.
- Never sleep. Use bounded waits (`require.Eventually`, sink `Wait*` methods).
- Reset shared sinks between tests.
- Prefer `require.NoError`, `require.Equal`, `require.Contains` over hand-written
  `if ... t.Fatalf`.

---

## Makefile conventions

Every example has its own Makefile with at minimum:
```
build, test, e2e, run, clean, download-envoy
```

Standard build target:
```makefile
build:
	cd $(ROOT)/examples && CGO_ENABLED=1 GOWORK=off go build -trimpath \
	  -buildmode=c-shared -o $(EXAMPLE)/lib$(EXAMPLE).so ./$(EXAMPLE)/cmd
```

Do not add per-example targets to the root Makefile.

---

## Envoy config conventions

- `upgrade_configs: [{upgrade_type: websocket}]` is required on the HCM
  listener to allow WebSocket upgrades. Omitting it causes Envoy to reject
  the upgrade with 426.
- Filter names in YAML must exactly match the `name` string passed to
  `up.Register*`. Mismatch = filter not found at runtime.
- Dynamic module config: `name` in YAML maps to `lib<name>.so` on disk.
  Envoy prepends `lib` automatically.
- Use `ENVOY_DYNAMIC_MODULES_SEARCH_PATH` (env var) to tell Envoy where to
  find `.so` files. Set in e2e TestMain and in `make run`.

---

## What to read before implementing wsproxy

The wsproxy example has a design doc and a README spec:
- Design doc: `.hermes/plans/2025-05-25-openai-realtime-ws-proxy.md`
- Spec (source of truth for implementation): `examples/ws-proxy/README.md`

Key constraints from the design:
- The HTTP filter (`wsproxy-auth`) handles auth and sets internal headers.
  It does NOT touch routing.
- Routing to the embedded server is via a STATIC cluster in envoy.yaml
  pointing at the loopback port the Group goroutine binds to.
- The embedded `net/http` server is started by `up.RegisterWithGroup`.
- Frame pump: two goroutines, one per direction. No shared state between them.
- Only parse JSON for `response.done` and `rate_limits.updated`.
  All other frames forwarded without parse (bytes.Contains fast-path).
- WebSocket library: `github.com/coder/websocket` (context-aware, same as
  jisr/examples/ws-proxy).
- Session lifecycle: `context.WithDeadline` on pump goroutines.
- No PR #45040 dependency. Works on Envoy 1.38.0.
