# down

Package `down` bridges the official Envoy dynamic module SDK and defines types
that the SDK does not provide (access logger, cluster extension, LB policy).
Package `down/abi_impl` implements the Envoy C ABI via CGO `//export` symbols.

Normal handler code never imports either package directly. `up` re-exports
everything user-facing. The only place that imports `down/abi_impl` is the
shared library entrypoint.

## Layer map

```
Envoy loads the .so
  down/abi_impl   — CGO exports, unsafe pointer handling, ABI callbacks
  down            — registry, shared types, no CGO
  up              — user-facing Go API, no CGO
  your package    — handlers and factories
```

## down/abi_impl

Contains all CGO and `unsafe` code. Responsibilities:

- Exports the C symbols Envoy calls (`envoy_dynamic_module_*`)
- Maps ABI callbacks to the Go handler interface
- Manages per-request and per-config object lifetimes
- Implements access logger, cluster extension, and LB policy ABI callbacks

**Keep ordinary handler code out of this layer.** Importing `down/abi_impl`
from a reusable library package links the Envoy ABI exports into every binary
that depends on it, which breaks packages that are not built as `.so` files.

The blank import belongs only in `cmd/main.go` entrypoints:

```go
import _ "github.com/dio/transit/down/abi_impl"
```

## down

Defines types shared between `up` and `down/abi_impl` without pulling in CGO:

- HTTP filter, access logger, cluster extension, and LB policy registries
- `TimingInfo`, `BytesInfo`, `AccessLogType` — access log stream state
- `HostPtr`, `HostSpec`, `HostHealth`, `HostStat` — cluster and LB types
- `ClusterLB`, `Cluster`, `ClusterFactory`, `ClusterConfigFactory`
- `LBPolicy`, `LBPolicyFactory`, `LBPolicyConfigFactory`

All of these are re-exported by `up` under the same names. User code sees them
as `up.HostSpec`, `up.ClusterLB`, etc.

## ABI version

`down/abi_impl/VERSION` records the Envoy dynamic module ABI version this build
targets. The Envoy binary rejects a module whose ABI version does not match.
