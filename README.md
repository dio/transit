# transit

An ergonomic Go handler layer for [Envoy dynamic modules](https://www.envoyproxy.io/docs/envoy/latest/intro/arch_overview/advanced/dynamic_modules), built on top of the official SDK.

Write a plain Go function, register it, ship a `.so`.

## Writing a handler

```go
package hello

import "github.com/dio/transit/up"

func Handler(w *up.Writer, r *up.Request) {
    w.Log(up.LogWarn, "hello: %s %s", r.Method, r.Path)
}
```

`r` carries the parsed request (method, path, host, and arbitrary headers via `r.Header`).
`w` provides actions: log, send a local response, and more.

Other registration variants cover the full request/response lifecycle:

| Function | Use case |
|---|---|
| `up.Register(name, handler)` | request headers only |
| `up.RegisterWithResponse(name, onReq, onResp)` | request + response headers |
| `up.RegisterWithBody(name, onReq, onBody)` | streaming request body |
| `up.RegisterWithMutableBody(name, onReq, onBody)` | buffered, replaceable request body |
| `up.RegisterAccessLogger(name, factory)` | access logger (fires after the stream ends) |

`Writer` also exposes span annotation (`w.GetActiveSpan().SetTag(k, v)`), dynamic
metadata (`w.SetMetadata(ns, key, value)`), and upstream filter support — wire a
filter via `HttpProtocolOptions.http_filters` on a cluster to run it on the
upstream side instead of the listener side.

## Registering

Wire the handler from your module's entry point:

```go
package main

import (
    "github.com/dio/transit/examples/hello"
    "github.com/dio/transit/up"
)

func init() { up.Register("hello", hello.Handler) }
func main() {}
```

`Register` must be called from `init`. The name must match `filter_name` in your Envoy config.

## Sending a response from the handler

Return an immediate reply — subsequent filters are not called:

```go
func Handler(w *up.Writer, r *up.Request) {
    if r.Header("x-api-key") == "" {
        w.SendLocalResponse(401, []byte(`{"error":"missing x-api-key"}`))
        return
    }
}
```

## Building

```sh
make build                  # host platform (.so in dist/)
make build-linux-amd64      # cross-compile for Linux amd64
make build-linux-arm64      # cross-compile for Linux arm64
```

Build uses [zig cc](https://ziglang.org) for the C toolchain. Zig is downloaded automatically on first use; no other C toolchain required.

Override versions on the command line:

```sh
make build ZIG_VERSION=0.16.0 EXAMPLE=hello
```

## Running

```sh
make run        # builds, downloads Envoy if needed, starts it
```

Then:

```sh
curl localhost:10000/
```

Envoy is downloaded automatically to `.bin/envoy`.

## Testing

Unit tests (no Envoy required):

```sh
make test
```

End-to-end tests (builds a `.so`, starts Envoy, makes real HTTP requests):

```sh
make e2e
```

Reuse an already-built `.so` for faster iteration:

```sh
TRANSIT_SKIP_BUILD=1 make e2e
```

## How it works

```
official SDK  ←  down/abi_impl  ←  up  ←  your handler
```

`up` is the ergonomic layer: `Register`, `HandlerFunc`, `Writer`, `Request`. It has no CGO.

`down/abi_impl` is the ABI shim: blank-imports the official SDK's HTTP filter `//export` symbols and provides the access-logger `//export` symbols that the upstream SDK does not yet include.

Your binary only needs to import `up`. The `down` package is pulled in transitively.

## Requirements

- Go 1.26+
- Internet access for first-time zig and Envoy downloads (or set `ZIG_BIN` / `ENVOY_BIN` to existing paths)
