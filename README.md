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

Use the smallest registration form that matches the lifecycle you need:

`up.Register(name, handler)`

Use this for request headers only.

`up.RegisterWithResponse(name, onReq, onResp)`

Use this when you need request and response headers.

`up.RegisterWithBody(name, onReq, onBody, onResp)`

Use this for streaming request body handling.

`up.RegisterWithMutableBody(name, onReq, onBody, onResp)`

Use this when you need buffered, replaceable body content.

`up.RegisterWithGroup(name, group, handler)`

Use this when the filter owns background goroutines with the same lifetime as
the loaded filter config.

`up.RegisterAccessLogger(name, factory)`

Use this for access logs that run after the stream is complete.

`up.RegisterCluster(name, factory)` and `up.RegisterLBPolicy(name, factory)`

Use these for Envoy Cluster Extension and LB Policy modules.

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

Run example e2e suites:

```sh
make e2e-hello
make e2e-lb-policy
make e2e-request-ui
make e2e-sse-tap
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

The `examples/` module shows several ways to use transit:

- `hello` for a minimal HTTP filter
- `lb-policy` for a custom LB Policy
- `sse-tap` for observing server sent events
- `request-ui` for request capture with a small UI
- `spa` for serving embedded static assets from a module

Start with `examples/hello` if you are new to the project.
