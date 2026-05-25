# transit

`transit` is a Go layer for Envoy dynamic modules. It lets you write normal Go
handlers, register them by name, and build a shared library that Envoy can load.

The goal is to keep day to day filter code small and familiar while still
leaving the Envoy ABI details available when the project needs them.

## What you write

A basic HTTP filter is just a function:

```go
package hello

import "github.com/dio/transit/up"

func Handler(w *up.Writer, r *up.Request) {
	w.Log(up.LogWarn, "hello: %s %s", r.Method, r.Path)
}
```

`r` gives you the parsed request, including method, path, host, and headers.
`w` gives you the actions you can take, such as logging, sending a local
response, changing metadata, and working with the active tracing span.

For example, this sends an immediate response and stops the filter chain:

```go
func Handler(w *up.Writer, r *up.Request) {
	if r.Header("x-api-key") == "" {
		w.SendLocalResponse(401, []byte(`{"error":"missing x-api-key"}`))
		return
	}
}
```

## Registration

Register filters from the shared library entrypoint:

```go
package main

import (
	_ "github.com/dio/transit/down/abi_impl"
	"github.com/dio/transit/examples/hello"
	"github.com/dio/transit/up"
)

func init() {
	up.Register("hello", hello.Handler)
}

func main() {}
```

The blank import of `down/abi_impl` is intentional. It links the Envoy dynamic
module exports into the shared library. Keep that import in `cmd/main.go`
entrypoints that are built as `.so` files. Do not put it in reusable library
packages.

Registration happens in `init`, and the registered name must match the
`filter_name` in your Envoy config.

## Handler options

Start with `up.Register`. Move to a larger registration form only when the
filter needs another stream phase or lifecycle hook.

| Use case | Register with |
| --- | --- |
| Request headers only | `up.Register(name, onReq)` |
| Request and response headers | `up.RegisterWithResponse(name, onReq, onResp)` |
| Streaming request body chunks | `up.RegisterWithBody(name, onReq, onBody, onResp)` |
| Buffered body reads or body replacement | `up.RegisterWithMutableBody(name, onReq, onBody, onResp)` |
| Per-config metrics setup | `up.RegisterWithConfig(name, configure, onReq, onResp)` |
| Background goroutines tied to the loaded filter config | `up.RegisterWithGroup(name, group, onReq)` |
| Access logs after the stream is complete | `up.RegisterAccessLogger(name, factory)` |
| Envoy Cluster Extension modules | `up.RegisterCluster(name, factory)` |
| Envoy LB Policy modules | `up.RegisterLBPolicy(name, factory)` |

The request handler, usually named `onReq` above, always has this shape:

```go
func(w *up.Writer, r *up.Request)
```

Response and body handlers are separate callbacks because Envoy calls them at
different points in the stream. `RegisterWithBody` sees request body chunks as
they arrive. `RegisterWithMutableBody` buffers the body first so the handler can
read or replace the complete content.

For request-time Envoy HTTP callouts, use `w.HTTPCallout` when the handler may
send a local response. Use `w.Go` plus `w.Do` only for async work that forwards
the request after queued mutations. See `docs/async-http-callouts.md`.

## What Writer can do

`up.Writer` wraps the common Envoy actions you need from a filter:

- log through Envoy
- send local responses
- read and replace buffered bodies
- set dynamic metadata
- write filter state
- set an upstream override host
- annotate the active tracing span

Transit also supports upstream HTTP filters. Configure the dynamic module filter
under `HttpProtocolOptions.http_filters` on a cluster when you want it to run on
the upstream side instead of the listener side.

## Build

Build the example module for your host platform:

```sh
make build EXAMPLE=hello
```

Cross compile for Linux:

```sh
make build-linux-amd64 EXAMPLE=hello
make build-linux-arm64 EXAMPLE=hello
```

Build output goes to `dist/`. The build uses Zig as the C toolchain. The Makefile
downloads Zig on first use unless you set `ZIG_BIN` yourself.

## Run

Run the example with Envoy:

```sh
make run EXAMPLE=hello
```

Then call Envoy:

```sh
curl localhost:10000/
```

The Makefile downloads Envoy to `.bin/envoy` on first use unless you set
`ENVOY_BIN`.

## Test

Run unit tests:

```sh
make test
```

Run the root e2e suite:

```sh
make e2e
```

Run example e2e suites from their own directories:

```sh
make -C examples/hello e2e
make -C examples/lb-policy e2e
make -C examples/cluster e2e
make -C examples/cluster-dfp e2e
make -C examples/cluster-router e2e
make -C examples/cluster-shard-router e2e
make -C examples/mcp-profile-router e2e
make -C examples/request-ui e2e
make -C examples/sse-tap e2e
make -C examples/spa e2e
```

Run Envoy Gateway integration suites from their own directories:

```sh
make -C integrations/cluster-router-eg e2e
make -C integrations/tiered-router-eg e2e
```

For faster e2e iteration after a successful shared library build:

```sh
TRANSIT_SKIP_BUILD=1 make e2e
```

Do not use `TRANSIT_SKIP_BUILD=1` for final verification after changing code
that is compiled into the module.

## Project shape

Transit keeps the public API separate from the ABI implementation:

```text
Envoy loads the .so
down/abi_impl maps the C ABI
down stores shared public types and registries
up exposes the user-facing Go API
your package contains handlers and factories
```

Most user code imports only `github.com/dio/transit/up`. The shared library
entrypoint also blank-imports `github.com/dio/transit/down/abi_impl` so Envoy can
find the exported module symbols.

The `down/abi_impl` package is where CGO, `unsafe`, and Envoy ABI pointer
handling live. Keep ordinary handler code out of that layer.

## Requirements

- Go 1.26.2 or newer
- Internet access for first time Zig and Envoy downloads, unless `ZIG_BIN` and
  `ENVOY_BIN` already point to local binaries
- Envoy dynamic modules enabled in the Envoy binary used for local runs and e2e
  tests

## Examples

The `examples/` module shows Transit at different layers of Envoy's dynamic
module surface. Start with `examples/hello` if you are new to the project.

### Cluster Routing

The cluster examples show request-aware upstream selection without creating one
Envoy route or static cluster per destination:

| Example | What it shows |
| --- | --- |
| `cluster` | Minimal Cluster Extension: add hosts during cluster init and choose a healthy host. |
| `cluster-dfp` | Model-to-target routing with Go DNS resolution and `ClusterLB.ChooseHost`. |
| `cluster-router` | LLM/MCP-style model routing, live config refresh, upstream auth injection, redacted config dump, and HTTPS provider egress with Envoy-owned TLS. |
| `cluster-shard-router` | L1 shard selection before model routing: derive a stable tag from request identity and choose the L2 shard. |

Use `cluster-router` when you want the current end-to-end shape for model or
provider routing. It keeps one public Envoy route, lets Go own the model
snapshot and host selection, and uses an upstream HTTP filter only for final
request shaping such as provider headers.

Use `cluster-shard-router` for the tiered L1 problem: choose where user-adjacent
state lives before an L2 `cluster-router` applies provider/model routing.

### HTTP Filters And UI

| Example | What it shows |
| --- | --- |
| `hello` | Minimal downstream HTTP filter. |
| `body-transform` | `RegisterWithMutableBody`: rename a JSON field before forwarding. |
| `header-router` | `SetUpstreamOverrideHost`: route to one of two backends based on a request header. |
| `request-ui` | Request capture with a small UI. |
| `sse-tap` | Observing server-sent events. |
| `spa` | Serving embedded static assets from a module. |

### Envoy Extension Points

| Example | What it shows |
| --- | --- |
| `lb-policy` | Custom LB Policy when Envoy owns the host set and Go only chooses among existing hosts. |
| `mcp-profile-router` | Local MCP profile aggregation: one profile URL, multiple backend MCP sessions, tool fan-out, namespaced tool calls, and redacted backend credentials. |

### Envoy Gateway Integrations

The `integrations/` module runs Kubernetes demos with Envoy Gateway and k3d:

| Integration | What it proves |
| --- | --- |
| `cluster-router-eg` | Envoy Gateway with a custom Envoy image, EnvoyPatchPolicy, Transit Cluster Extension routing, upstream provider header injection, live model updates, and a redacted config CLI. |
| `tiered-router-eg` | Planned two-stage Gateway shape: L1 shard placement first, L2 model/provider routing inside the selected shard. |
| `tiered-ws-proxy-eg` | Two-stage WS pipeline: L1 shard-routes the upgrade, L2 runs the embedded ws-proxy server and egresses via a second EG-managed listener. |
