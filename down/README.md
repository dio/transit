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

## Patched Envoy builds

When using a patched Envoy binary (e.g. one built with additional ABI callbacks
not yet merged upstream), `down/abi_impl/VERSION` carries three extra fields:

```
ENVOY_TAG=envoy-0d6e3c60-auto-host-sni   # release tag on dio/envoy-builder
ENVOY_ASSET_SUFFIX=-auto-host-sni        # suffix on the binary asset name
ABI_SOURCE=release                       # abi.h comes from the release, not gomod
```

`make sync-abi` downloads `abi.h` from that release asset so the local vendored
header stays in sync with the binary. `make download-envoy` picks up the patched
binary automatically via `ENVOY_TAG` + `ENVOY_ASSET_SUFFIX`.

### VSCode / gopls false positive

gopls reports `undefined: C.<new_callback>` for any symbol declared only in the
local `down/abi_impl/abi.h` but absent from the Go module cache copy. This is a
known gopls limitation with cgo relative includes — the language server does not
always execute the C preprocessor against the local header.

The actual compiler resolves `#include "abi.h"` to the local file and the build
succeeds:

```sh
GOWORK=off CGO_ENABLED=1 go build ./down/...  # no errors
```

The red squiggle in the editor is safe to ignore.
