# Verdict: Single Static Upstream TLS Transport with `auto_host_sni`

## Hypothesis

Replace per-upstream `transport_socket_matches` with a single static cluster
`transport_socket` using `auto_host_sni: true` and `auto_sni_san_validation:
true`. Envoy derives SNI and SAN validation target dynamically from the selected
host's hostname, eliminating all per-provider TLS configuration.

## Outcome

**Falsified.** The approach cannot work today with the dynamic module cluster
API. Two blockers were identified and proven experimentally.

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
