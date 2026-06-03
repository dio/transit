# Defer TransportSocketOptions until after async ChooseHost completes

## Context

orange routes requests to upstream LLM providers (anthropic, openai, …) by
reading the `model` field out of the request body. Provider selection is
body-driven: at headers phase the router has no idea which provider will be
chosen, so the cluster uses `lb_policy: CLUSTER_PROVIDED` with an async
`ChooseHost` that suspends until the body filter resolves a promise.

The cluster itself uses a single static `UpstreamTlsContext` with
`auto_host_sni: true` + `auto_sni_san_validation: true`. Each host has its own
hostname (e.g. `api.openai.com`, `api.anthropic.com`) plumbed through
`HostImpl::create(..., hostname, ...)` via the dynamic-modules ABI patch in
`docs/auto-host-sni-hostname-abi.patch`. The promise is that at TLS
handshake time Envoy reads `host->hostname()` off the *selected* host and uses
it as SNI.

The envoy repo is in /Users/dio/src/dio/envoy-repo. It is 0d6e3c60aa55e434f28e581df1d25fcb83404b68

We have it patched with: https://gist.githubusercontent.com/dio/e407ddaf6c1d0b21e87e6f091a4cecc1/raw/9efe60cc56af8edc5905b9301bd4f5440f191cfb/auto-host-sni-defer-tso.patch

If you need to build envoy, given a patch you can use envoy-mini-builder, for example, with the above patch:

```
envoy-mini-builder build   --sha 0d6e3c60   --platform darwin-arm64   --patch
  "https://gist.githubusercontent.com/dio/e407ddaf6c1d0b21e87e6f091a4cecc1/raw/9efe60cc56af8edc5905b9301bd4f5440f191cfb/auto-host-sni-defer-tso.patch" --suffix=-defer-tso
   --tag envoy-0d6e3c60-defer-tso   --bb-key 8FFgK4Ky7uwz9xuhEYll --no-clean --detach
```

## The bug

We have a minimal reproduction in
`examples/cluster-async-router/e2e-llm-tls/`. With this cluster config:

```
hosts: [{name: anthropic, ..., sni: api.anthropic.com},
        {name: openai,    ..., sni: api.openai.com}]
```

- Request targeting `anthropic` → TLS handshake succeeds, response 403.
- Request targeting `openai` → TLS handshake fails: `verify cert failed:
  SAN matcher, certificate SANs are [api.anthropic.com]`.

Swapping the host order in the config inverts which one succeeds. So **the
first host's hostname is being sent as SNI for every connection in the
cluster**, regardless of which host async ChooseHost actually selected.

This is *not* the dynamic-modules ABI patch being wrong — its unit test
verifies `host->hostname()` returns the per-host hostname. The bug is upstream
of that: by the time the SSL handshake runs, Envoy has already decided what
SNI to send and didn't ask the selected host.

Seems like caching issue indeed: ENVOY_COMPONENT_LOG_LEVEL=http:debug TEST_HOSTS=anthropic,openai go test ./e2e-llm-tls -v vs. ENVOY_COMPONENT_LOG_LEVEL=http:debug
  TEST_HOSTS=openai,anthropic go test ./e2e-llm-tls -v

## Root cause (the one to fix)

`source/common/router/router.cc:687-702`:

```cpp
if (!parsed_authority.is_ip_address_ && upstream_http_protocol_options->auto_sni() &&
    !filter_state->hasDataWithName(Network::UpstreamServerName::key())) {
  filter_state->setData(Network::UpstreamServerName::key(),
                        std::make_unique<Network::UpstreamServerName>(parsed_authority.host_));
}
// …
transport_socket_options_ = Network::TransportSocketOptionsUtility::fromFilterState(
    *callbacks_->streamInfo().filterState());
```

`transport_socket_options_` (TSO) is built **once, at `decodeHeaders` time**,
from filter state. The TSO carries `server_name_override`, which
`ClientContextImpl::newSsl` honors *in preference to* `auto_host_sni`
(`source/common/tls/client_context_impl.cc:132-138`).

For a body-driven async cluster the order of operations is:

1. `decodeHeaders` → router freezes TSO from FS (empty in this scenario).
2. Cluster's `ChooseHost` returns an async completion; router suspends.
3. Body filter parses body, resolves the completion with the selected host.
4. Pool calls `host_->createConnection(..., transport_socket_options_, ...)`.
5. SSL handshake reads `host->hostname()` if `options->serverNameOverride()`
   is absent.

Empirically, `auto_host_sni` is consistently leaking the *first* registered
host's hostname rather than the actually-selected host's. We have not
identified the precise C++ path that causes the leak (the patch test of
`host->hostname()` passes), but **regardless of that detail**, the safe, fully
general fix is to give the filter chain a way to inject the correct SNI
*after* async ChooseHost completes — and the router already knows how to
honor `Network::UpstreamServerName` in filter state. The blocker is purely
timing.

## What to build

Rebuild `transport_socket_options_` after async ChooseHost completes, so
that an HTTP filter which resolves the routing decision in the body phase can
deposit `envoy.network.upstream_server_name` (and friends) into filter
state and have those values reach the upstream TLS handshake.

Concrete changes in `source/common/router/router.cc`:

1. Identify the completion path for async ChooseHost. It is the point where
   the suspended request resumes with a now-known upstream host. Grep for
   `Completing asynchronous host selection` (router emits this debug log when
   the completion fires); the call site is the resume hook for the `nullopt`
   return from `ChooseHost` (see `Router::Filter::onPoolReady` / the host
   selection completion callback in `router.cc`).

2. Immediately *before* the upstream connection is created (and definitely
   before `transport_socket_options_` is used by the pool's connection
   creation), do:

   ```cpp
   // Async ChooseHost may have resolved after a filter wrote routing-relevant
   // filter state (e.g., envoy.network.upstream_server_name) during body
   // processing. Rebuild TSO from the now-complete filter state so the
   // upstream TLS handshake sees the correct override.
   transport_socket_options_ = Network::TransportSocketOptionsUtility::fromFilterState(
       *callbacks_->streamInfo().filterState());
   ```

3. Verify the `auto_sni` / `auto_san_validation` block (router.cc:687-698)
   does not need to re-run. It populates FS from `:authority` only when not
   already present, which is idempotent — if the body handler also rewrote
   `:authority` and we want post-body authority to drive `auto_sni`, the same
   block needs to run again on the post-body authority. For the minimum fix,
   *do not* re-run that block (orange uses explicit FS, not auto_sni). Just
   re-read FS into TSO.

## Test plan

Two existing tests already exercise this end-to-end and fail today:

1. `examples/cluster-async-router/e2e-llm-tls/` — minimal repro using two
   real LLM TLS endpoints (`api.anthropic.com`, `api.openai.com`). The body
   handler needs one extra line that's already implemented in
   `examples/cluster-async-router/cluster_async_router.go:bodyHandler`:

   ```go
   if v, ok := sniByTarget.Load(target); ok {
       w.SetFilterStateTyped("envoy.network.upstream_server_name", v.(string))
   }
   ```

   After the patch lands, this test should pass — both `anthropic` and
   `openai` targets succeed (403 because we don't send creds; TLS handshake
   completes).

2. `examples/orange/e2e/` — the real orange flow. Update orange's match
   filter (`internal/pipeline/match/match.go:bodyHandler`) to set the typed
   FS right before `p.Resolve(d)`:

   ```go
   w.SetFilterStateTyped("envoy.network.upstream_server_name", provider.SNI())
   ```

   (where `provider.SNI()` returns the host portion the cluster registered).
   After the patch, hitting `claude-haiku-4-5` and `gpt-4o-mini` interleaved
   should both return real LLM responses (assuming valid API keys), not
   503 / CERTIFICATE_VERIFY_FAILED.

## What's already in place in transit (no work needed)

- `up.Writer.SetFilterStateTyped(key, value string)` (added in
  `up/writer.go`) — calls the dynamic-modules `set_filter_state_typed`
  ABI, which routes through the registered `ObjectFactory` so the FS
  entry is a real `Network::UpstreamServerName` object (not a
  `Router::StringAccessorImpl`). The router's `fromFilterState` uses
  `getDataReadOnly<UpstreamServerName>` which requires the typed object.
- Queued-mutation path in `up/filter.go:flush` switches on a `typed bool`
  on `filterStateMutation` (defined in `up/async.go`).
- `examples/cluster-async-router/cluster_async_router.go` already writes the
  typed FS in `bodyHandler` using the new API. No further example-side
  changes needed.

## What this fix does *not* attempt

- It does not investigate why `auto_host_sni` is shipping the first host's
  hostname instead of the selected host's. That is a separate, real bug
  worth filing upstream, but routing around it via filter-state override is
  more reliable for orange because it's an explicit per-request signal that
  doesn't depend on Envoy guessing.
- It does not change `transport_socket_matches` behavior — orange explicitly
  avoids that mechanism because it requires xDS for provider add/remove.

## Build / artifact

The patched binary lives at `/Users/dio/src/dio/transit2/.bin/envoy` and is
built from `/Users/dio/src/dio/envoy-repo` (HEAD `0d6e3c60`). The release
tag is `envoy-0d6e3c60-auto-host-sni` with asset suffix `-auto-host-sni`.
The orange `Makefile` and `VERSION` consume the binary by tag; bump the
tag/suffix after this fix lands and re-run `make sync-abi` (only needed
if `abi.h` changes — it does not for this fix).
