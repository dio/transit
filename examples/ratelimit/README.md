# Rate-limit admission module

An independently loadable `libratelimit.so` using Envoy's RLS v3
`ShouldRateLimit` API. Derived from `../grpc-callout`, with configurable
descriptors, immutable policies, explicit failure handling, and a configuration
source boundary. This lives in Transit as an example first; its runtime package
has no dependency on other examples, Plum, Orange, or Fraser.

See [Fraser adoption analysis](FRASER.md) for placement before aidiscovery,
native-versus-module tradeoffs, and the remaining token-budget integration.

```text
authenticated request
  → ratelimit (capture policy, construct descriptors)
  → Envoy HTTP/2 cluster → RLS ShouldRateLimit → Redis counters
  ← OK: continue / OVER_LIMIT: 429 / service error: configured open or closed
```

## Native or extension

Both paths speak the same RLS protocol and can use `github.com/dio/rls` or
`envoyproxy/ratelimit` as the backend:

| Path | Configuration | Use |
|---|---|---|
| Native Envoy | `envoy-native.yaml`; descriptors in route actions | Existing Envoy admission behavior without a custom binary |
| Dynamic module | `envoy.yaml` plus `config.json` | Custom descriptor/configuration delivery inside Envoy |

The configs use domain `orange` and descriptor entries `key_id`, then `dim=rpm`,
matching Fraser `adopt-orange`'s injected native RLS e2e configuration. This
example does not change Fraser's production filter chain.

These paths are alternative deployments, not exact behavioral substitutes:
the module rejects missing descriptor values with 400; native Envoy can omit
the descriptor when a required header is absent. Authenticate before either
filter and make the trusted identity available before its headers callback.
Do not accept a caller-selected `x-tars-id` as authenticated identity. The
module also supports `filter_state` for identity set by an earlier filter.

## Build and run

```sh
make -C examples/ratelimit test
make -C examples/ratelimit e2e
make -C examples/ratelimit run
```

The run config expects an HTTP upstream on `127.0.0.1:10001` and an h2c RLS on
`127.0.0.1:10002`. The listener is `127.0.0.1:10000`. For `dio/rls`, put
`rls-config.yaml` in its `RLS_CONFIG_DIR`, set `RLS_GRPC_ADDR=127.0.0.1:10002`,
and point `RLS_REDIS_ADDR` at Redis. This rule allows 60 requests per minute per
`key_id`. That service also accepts `RLS_CONFIG_URL` and has a `provider.Loader`
extension point for custom quota configuration.

```sh
curl -i http://127.0.0.1:10000/ -H 'x-tars-id: demo' -d hello
```

To use the native path, run Envoy with `-c examples/ratelimit/envoy-native.yaml`;
no module or `RATELIMIT_CONFIG` is needed.

The module command reads the file named by `RATELIMIT_CONFIG` once during
library initialization. `make run` supplies `config.json`. Missing or invalid
initial config prevents registration and therefore prevents Envoy from loading
the requested filter. There is no implicit file watching or remote fetch.
`filter_config` is empty in this example; client policy comes from that file.

## Client policy

See `config.json` for the schema. `cluster` and `domain` are required.
`timeout_ms` defaults to 50, with a maximum of 60000. `failure_mode` is `closed`
(default) or `open`. Each descriptor is an ordered array of entries; each entry
requires a key and exactly one nonempty `value`, lowercase `header`, or
`filter_state` source. Missing dynamic values reject the request with 400 rather
than silently skipping quota enforcement. Every admitted check uses
`hits_addend=1`.

`OVER_LIMIT` always returns 429. Callout initialization, transport, gRPC status,
decode, and unknown response-code failures return 503 in closed mode and
continue in open mode. Missing configuration always returns 503. Each request
uses one captured policy throughout its asynchronous callout.

This is request admission: no body parsing or full-body requirement, per-attempt provider selection,
chain advancement, retries of quota checks, token/cost accounting, or response
header propagation from RLS. An admission denial terminates the request. Chain
integration needs a separate decision contract; it must not turn a caller quota
denial into a provider fallback.

Envoy buffers arriving body data while the asynchronous quota decision is
pending. The Transit header-only callout path must hold both body and trailers
until that decision; this example includes that core fix and regression tests.

## Custom configuration source later

Descriptor assembly can be code-owned rather than declarative. An embedding
application supplies `Config.BuildDescriptors` instead of `Config.Descriptors`,
then calls `Compile`. For example, retain the existing RLS `key_id` meaning
(TARS router identity) while reading trusted metadata rather than a header:

```go
config.BuildDescriptors = func(w *up.Writer, _ *up.Request) ([]*rlscommonv3.RateLimitDescriptor, error) {
    id, ok := w.GetMetadataString(up.MetadataSourceDynamic, "tars", "router_id")
    if !ok || id.String() == "" {
        return nil, fmt.Errorf("trusted router identity unavailable")
    }
    return []*rlscommonv3.RateLimitDescriptor{{
        Entries: []*rlscommonv3.RateLimitDescriptor_Entry{
            {Key: "key_id", Value: id.String()},
            {Key: "dim", Value: "rpm"},
        },
    }}, nil
}
policy, err := ratelimit.Compile(config) // config.Descriptors must be empty
```

`rlscommonv3` is the Envoy `extensions/common/ratelimit/v3` protobuf package.
The callback must be concurrent-safe, nonblocking and return owned descriptors.
The module handles transport and decisions. An error or empty/invalid descriptor
result returns 400, including in fail-open mode; it never silently skips the
check. A custom source decoder can parse policy inputs, attach the builder, and
call `Compile`. Functions are not serialized into JSON.

There are two separate configuration boundaries:

1. **RLS quota rules:** `dio/rls`'s `provider.Loader` supplies limits. Both native
   and extension clients work with that source unchanged.
2. **Module client policy:** Transit `up.ConfigSource` supplies bytes that
   `ratelimit.Decode` validates before publication. The request path only reads
   `PipelineConfig.Snapshot()`; it never performs configuration I/O.

The embedding application can replace the static source without editing
enforcement:

```go
pc := up.NewPollingConfig(customSource, ratelimit.Decode, up.PollOptions{})
// Use a bounded context for initial loading. Abort startup on error.
if err := pc.RefreshOnce(initialLoadContext); err != nil {
    return err
}
// Register during module initialization.
up.Register("ratelimit", ratelimit.NewHandler(pc))
// Start from the application's lifecycle owner and call stop before destruction.
stop := pc.Start(applicationContext)
```

The application owns source startup/shutdown and serializes refresh writers.
Invalid fetches or decoded updates retain the last good policy. Policies copy
input slices and expose no mutable fields; builder closures must keep their
own captured inputs immutable or synchronized. No shared package-global policy or
poller is installed by this library. Remote module sources must use
Envoy-managed egress; this example adds no direct network client.

For a separate repository, move the package and `cmd/`, update the command's
import path, and add a `go.mod` pinning Transit, RLS protobufs and protobuf Go.
Adapt the example build/test helpers there; the enforcement API and config
schema need not change. Keep Redis and the RLS server outside the Envoy module.

## Verification scope

Unit/race coverage checks validation, descriptor mapping, response/error
decisions, immutable snapshots, last-good retention and concurrent reads during
refresh. Envoy e2e uses a protocol-compatible gRPC test service to check actual
forwarding, denial, deadlines, non-OK gRPC status, unknown RLS codes and missing
clusters in both failure modes. These tests do not establish Redis correctness
or deployment readiness of a separate RLS service.

A local smoke on 2026-10-08 also exercised both checked-in Envoy configurations
against `dio/rls` and an isolated Redis process: with one request per hour,
each returned 200 then 429 and forwarded exactly one request. Transit root
race tests and root Envoy e2e passed. The locally tested Envoy was
`0d6e3c60aa55` (1.39.0-dev); release-version and Composer coexistence acceptance
remain separate work.
