# Envoy ABI Patch: `add_hosts_with_hostnames` for Dynamic-Module Clusters

Companion to `docs/auto-host-sni-verdict.md`. That doc proved that
`UpstreamTlsContext.auto_host_sni` cannot derive an SNI for dynamic-module
cluster hosts because the existing
`envoy_dynamic_module_callback_cluster_add_hosts` ABI has no hostname
parameter. This doc is the proposed Envoy patch and how to build it via
`envoy-mini-builder`.

## What the patch does

Additive ABI: adds a new callback
`envoy_dynamic_module_callback_cluster_add_hosts_with_hostnames` that accepts a
per-host `hostnames` array alongside the existing `addresses` array. Each
non-empty hostname is stored on the resulting `HostImpl` and is what
`Upstream::HostDescription::hostname()` returns — the value
`UpstreamTlsContext.auto_host_sni` and `auto_sni_san_validation` read at connect
time. Empty entries (or a nullptr `hostnames` array) preserve the current
synthesized hostname (`cluster_name + address`), so existing modules see no
behavior change.

The existing `envoy_dynamic_module_callback_cluster_add_hosts` keeps its
signature; internally it now passes an all-empty hostnames vector to the shared
`DynamicModuleCluster::addHosts` implementation.

## Files changed (against `envoyproxy/envoy@0d6e3c60aa55`)

```
source/extensions/dynamic_modules/abi/abi.h
source/extensions/dynamic_modules/abi_impl.cc          (weak stub)
source/extensions/clusters/dynamic_modules/abi_impl.cc (new entrypoint)
source/extensions/clusters/dynamic_modules/cluster.h   (addHosts signature)
source/extensions/clusters/dynamic_modules/cluster.cc  (use hostname when non-empty)
test/extensions/clusters/dynamic_modules/cluster_test.cc (3 new tests + helper update)
```

Total: 242 insertions, 16 deletions across 6 files.

## Tests added

In `test/extensions/clusters/dynamic_modules/cluster_test.cc`:

1. `AbiCallbacksAddHostsWithHostnames` — three hosts with hostnames
   `host-c.test`, `host-d.test`, and empty. Asserts
   `host->hostname()` returns each provided hostname, and that the empty entry
   falls back to the legacy synthesized form `cluster_name + address`.
2. `AbiCallbacksAddHostsWithHostnamesNullArrayIsLegacy` — pass `nullptr` for
   `hostnames`. Asserts both hosts get the legacy synthesized hostname,
   matching the behavior callers of the original entrypoint see today.
3. `AbiCallbacksLegacyAddHostsPreservesSynthesizedHostname` — regression test
   for the unchanged
   `envoy_dynamic_module_callback_cluster_add_hosts` entrypoint: asserts the
   resulting `host->hostname()` is still `cluster_name + address`.

The shared `addSimpleHosts` test helper is updated to pass an empty hostnames
vector through the new `addHosts` signature.

No Rust SDK or Go SDK callers reference the new function; only Transit
(`down/abi_impl/cluster.go`) will adopt it after the Envoy side lands. Existing
Rust SDK (`sdk/rust/src/cluster.rs`) and the legacy ABI tests continue to call
`envoy_dynamic_module_callback_cluster_add_hosts` and remain green.

## On `std::string` vs `absl::string_view` for the hostnames parameter

Considered. `HostImpl::create` takes `const std::string& hostname`
(`source/common/upstream/upstream_impl.h:518`) and `HostImpl` stores it as an
owned `std::string`, so a copy is required at the sink regardless of how
`addHosts` receives the value. The four sibling per-host arrays
(`addresses`, `regions`, `zones`, `sub_zones`) are all
`std::vector<std::string>`; the ABI layer deliberately copies module buffers
into owned strings because the buffers' lifetime is the ABI call. Switching
only `hostnames` to `string_view` would save one string copy in the ABI parsing
loop at the cost of inconsistency with the four sibling params. Migrating all
five together is a worthwhile follow-up but out of scope here; this patch
keeps `std::vector<std::string>` for local consistency.

## Building via envoy-mini-builder

The patch is published as a public gist:

- Gist: <https://gist.github.com/dio/e4d1c59710a2039d146deb98ee19977c>
- Raw URL (verified byte-identical to `docs/auto-host-sni-hostname-abi.patch`
  and applies cleanly against `0d6e3c60aa55`):
  <https://gist.githubusercontent.com/dio/e4d1c59710a2039d146deb98ee19977c/raw/ea4139ca6e87ead2df0ef878a65a16fbf645ce89/auto-host-sni-hostname-abi.patch>

For now we only build `darwin-arm64` — that's what the local e2e suite
(`make -C examples/cluster-async-router e2e-static-tls`) runs against.

```sh
envoy-mini-builder build \
  --sha       0d6e3c60aa55 \
  --patch     https://gist.githubusercontent.com/dio/e4d1c59710a2039d146deb98ee19977c/raw/ea4139ca6e87ead2df0ef878a65a16fbf645ce89/auto-host-sni-hostname-abi.patch \
  --suffix    -auto-host-sni \
  --tag       envoy-0d6e3c60-auto-host-sni \
  --platform  darwin-arm64
```

Add `--no-release --out ./dist` if you want the binary locally without
publishing to the GitHub release. Other platforms (`linux-arm64`,
`linux-amd64`) can be added later by re-running with `--platform <name>`
under the same `--tag`.

If you update the patch, push a new gist revision (`gh gist edit <id>
<file>`) and re-fetch the raw URL — gist raw URLs are pinned to a specific
revision SHA, so the URL above never silently changes.

## Validating the patch locally

```sh
cd ~/src/dio/envoy-repo
git checkout 0d6e3c60aa55
git apply --check docs/auto-host-sni-hostname-abi.patch   # confirmed clean
git apply docs/auto-host-sni-hostname-abi.patch
```

To run the new unit tests under Bazel (when an Envoy build environment is
available):

```sh
bazel test //test/extensions/clusters/dynamic_modules:cluster_test \
  --test_filter='DynamicModuleClusterTest.AbiCallbacks*Hostname*:DynamicModuleClusterTest.AbiCallbacksLegacyAddHostsPreservesSynthesizedHostname'
```

## Transit side (after patched Envoy is available)

After the patched Envoy is published as a release asset and pinned via
`down/abi_impl/VERSION`:

1. Update `down/abi_impl/abi.h` from the vendored Envoy SDK.
2. Update `down/abi_impl/cluster.go::AddHosts` to call
   `envoy_dynamic_module_callback_cluster_add_hosts_with_hostnames`, threading
   `HostSpec.Hostname` through a new buffer array.
3. Add `Hostname string` to `down/cluster.go::HostSpec`.
4. In `examples/cluster-async-router/cluster_async_router.go`, store the
   original FQDN in `HostSpec.Hostname` before resolving the address to IP.
5. Re-run `make -C examples/cluster-async-router e2e-static-tls` — the two
   currently failing tests should pass with a single static `transport_socket`
   on the cluster.
