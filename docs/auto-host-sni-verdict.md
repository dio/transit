# Verdict: Single Static Upstream TLS Transport with `auto_host_sni`

## Hypothesis

Replace per-upstream `transport_socket_matches` with a single static cluster
`transport_socket` using `auto_host_sni: true` and `auto_sni_san_validation:
true`. Envoy derives SNI and SAN validation target dynamically from the selected
host's hostname, eliminating all per-provider TLS configuration.

## Outcome

**Resolved — V1 confirmed (after ABI patch).** The approach works once the
dynamic module cluster ABI carries a per-host hostname. Both blockers identified
in the initial investigation have been fixed and proven experimentally.

See **Resolution** section below for the patch, test results, and WS-B verdict.

---

## Evidence

A focused e2e variant lives under
`examples/cluster-async-router/e2e-static-tls/` with:

- A single static `UpstreamTlsContext` (`auto_host_sni: true`,
  `auto_sni_san_validation: true`, no `transport_socket_matches`)
- TLS upstreams only — system CA bundle (`/etc/ssl/cert.pem`), real public hosts
  (`httpbin.org:443`, `example.com:443`) so the proof is independent of any local
  cert plumbing
- Body-driven host selection routes `{"target":"httpbin"}` to `httpbin.org` and
  `{"target":"example"}` to `example.com` through the dynamic-module cluster

Run: `make -C examples/cluster-async-router e2e-static-tls`

Both `TestStaticTLS_Httpbin` and `TestStaticTLS_Example` fail with HTTP 503.
Envoy debug logs (`--component-log-level connection:debug`) show the exact
reason:

```
verify cert failed: SAN matcher, certificate SANs are [httpbin.org, *.httpbin.org]
TLS_error:|268435581:SSL routines:OPENSSL_internal:CERTIFICATE_VERIFY_FAILED
```

The server returned its real cert (SAN `httpbin.org`), but Envoy's SAN matcher,
populated from `auto_sni_san_validation` reading the derived SNI, matched
against an empty / synthesized string instead of `httpbin.org` — so validation
rejected an otherwise-valid certificate.

(Earlier internal e2e variant against local cert-enforcing servers surfaced the
same root cause as a wrong SNI on the wire:
`got SNI "upstream127.0.0.1:52826", want "host-c.test"`.)

---

## Root Cause

### Blocker: `add_hosts` ABI has no hostname parameter

`envoy_dynamic_module_callback_cluster_add_hosts` (`abi.h:8663`) accepts only
`ip:port` address strings — there is no hostname parameter:

```c
bool envoy_dynamic_module_callback_cluster_add_hosts(
    envoy_dynamic_module_type_cluster_envoy_ptr cluster_envoy_ptr,
    uint32_t priority,
    const envoy_dynamic_module_type_module_buffer* addresses,   // ip:port only
    const uint32_t* weights,
    ...
    size_t count,
    envoy_dynamic_module_type_cluster_host_envoy_ptr* result_host_ptrs);
```

### What Envoy does with the missing hostname

In `source/extensions/clusters/dynamic_modules/cluster.cc:350–354`, Envoy
constructs `HostImpl` for each added host with:

```cpp
auto host_result = Upstream::HostImpl::create(
    cluster_info, cluster_info->name() + addresses[i], ...);
```

The second argument is the hostname stored on the host. With no hostname
parameter available, Envoy uses a bare concatenation of the cluster name and the
address string — **no separator**:

```
"upstream" + "127.0.0.1:52826" = "upstream127.0.0.1:52826"
```

`host->hostname()` returns this verbatim. `auto_host_sni` reads it and puts it
directly in the TLS ClientHello as the SNI extension value.

### Where hostname information is lost in the call chain

| Layer | Location | What happens |
|---|---|---|
| Cluster config JSON | `cluster_config.value` | `"address":"127.0.0.1:PORT"` — original hostname not present; `resolveHostAddr` has already resolved DNS to IP |
| Go — `resolveHostAddr` | `cluster_async_router.go:243` | Resolves `hostname:port → ip:port`, discarding the original hostname |
| Go — `AddHosts` | `down/abi_impl/cluster.go:81` | Passes only `s.Address` (ip:port) to the ABI; no hostname slot exists |
| ABI | `abi.h:8663` | No `hostnames` parameter |
| Envoy C++ | `cluster.cc:350` | `HostImpl` hostname = `cluster_name + address_string` |
| TLS transport | `auto_host_sni` | SNI = `host->hostname()` = `"upstream127.0.0.1:PORT"` |

---

## Smallest Required Fix

### 1. ABI — add a `hostnames` parameter

```c
bool envoy_dynamic_module_callback_cluster_add_hosts(
    envoy_dynamic_module_type_cluster_envoy_ptr cluster_envoy_ptr,
    uint32_t priority,
    const envoy_dynamic_module_type_module_buffer* addresses,
    const envoy_dynamic_module_type_module_buffer* hostnames,  // NEW: one per host, empty = no hostname
    const uint32_t* weights,
    ...
```

### 2. Envoy C++ — use hostname when non-empty

```cpp
// cluster.cc:350 — use provided hostname, fall back to address when absent
std::string hostname = hostnames[i].length > 0
    ? std::string(hostnames[i].ptr, hostnames[i].length)
    : addresses[i];
auto host_result = Upstream::HostImpl::create(
    cluster_info, hostname, std::move(resolved_address), ...);
```

### 3. Transit Go — thread hostname through `HostSpec`

```go
// down/cluster.go
type HostSpec struct {
    Address  string
    Hostname string            // NEW: used by auto_host_sni; empty = no SNI derivation
    Weight   uint32
    Metadata map[string]string
}
```

Pass the `hostnames` array in `AddHosts` alongside `addresses`.

### 4. Example — keep original hostname separate from resolved IP

In `resolveHostAddr` (or at the call site), store the original hostname string
in `HostSpec.Hostname` before resolving to IP for `HostSpec.Address`.

---

## Secondary Observation

A single static `UpstreamTlsContext` applies TLS to **all** connections in the
cluster. Clusters mixing TLS and plaintext hosts cannot use this approach — each
plaintext upstream would receive a TLS handshake attempt and return 503. This is
an independent constraint from the hostname blocker above, relevant if the fix is
implemented and the cluster config includes both TLS and non-TLS hosts.

---

## Resolution

All four steps from "Smallest Required Fix" above were implemented:

### ABI patch

A new `envoy_dynamic_module_callback_cluster_add_hosts_with_hostnames` callback
was added to Envoy. A patched binary is published at:

```
https://github.com/dio/envoy-builder/releases/tag/envoy-0d6e3c60-auto-host-sni
```

The patch gist:
```
https://gist.githubusercontent.com/dio/e4d1c59710a2039d146deb98ee19977c/raw/.../auto-host-sni-hostname-abi.patch
```

The patched `abi.h` is available as a release asset (`abi.h`) on the same release.
Transit vendors it at `down/abi_impl/abi.h` with `ABI_SOURCE=release` in `down/abi_impl/VERSION`.

### Transit changes

- `down/cluster.go`: `HostSpec.Hostname string` added
- `down/abi_impl/cluster.go`: `AddHosts` calls `add_hosts_with_hostnames`, passing hostname per host
- `examples/cluster-async-router/cluster_async_router.go`: `spec.Hostname = he.SNI`

### Test results

`make -C examples/cluster-async-router e2e-static-tls` — both tests now **PASS**:

```
TestStaticTLS_Httpbin   status=405   PASS  (TLS handshake succeeded, SAN validated)
TestStaticTLS_Example   status=403   PASS  (TLS handshake succeeded, SAN validated)
```

`make -C examples/cluster-async-router e2e-static-tls-matches` — all three **PASS**:

```
TestTLSMatches_Httpbin    status=405   PASS  (tls-system-ca bucket)
TestTLSMatches_Example    status=403   PASS  (tls-system-ca bucket)
TestTLSMatches_Plaintext  status=200   PASS  (plaintext bucket)
```

`make -C examples/cluster-async-router e2e` — canonical 4-test suite all **PASS**:

```
TestAsyncRouter_TLS_Httpbin    status=405   PASS
TestAsyncRouter_TLS_Example    status=403   PASS
TestAsyncRouter_Plaintext      status=200   PASS
TestAsyncRouter_UnknownTarget  status=503   PASS
```

### WS-B Verdict: V1 — Fully Dynamic Host + Envoy-Owned TLS

For ordinary public WebPKI providers, provider add/remove is **application config
only**. No xDS/EPP update is required. The stable Envoy config is:

```yaml
transport_socket:           # single static socket — OR —
  auto_host_sni: true       # use transport_socket_matches with bucket key
  auto_sni_san_validation: true    # for mixed plain+TLS clusters
  trusted_ca: system bundle
```

The dynamic module sets `HostSpec.Hostname` at `AddHosts` time. Envoy reads it
via `HostDescription::hostname()` at connect time.

**Downstream shifts (from plan.md WS-B verdict forks):**

| Workstream | V1 shift |
|---|---|
| WS-D | `HostPtr` carries hostname; `AsyncHostSelector[T]` lookup is `func(T) (host, sni)` |
| WS-H | Implements stable generic transport; provider/server add/remove is config-only across all four EG integrations |

**Remaining WS-B experiments for WS-H (do not change V1 verdict):**

- Experiment 5: `auto_sni` from `:authority` header timing (router trap)
- Experiments 6–7: Envoy Gateway native Gateway API vs EPP comparison
- Experiment 9: Cohere-style provider add on EG (config-only proof)
