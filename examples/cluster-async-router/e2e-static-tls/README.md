# e2e-static-tls

End-to-end test that validates a **single static `UpstreamTlsContext`** on a
dynamic-modules cluster, using `auto_host_sni` and `auto_sni_san_validation` to
derive the SNI and SAN matcher from each host's logical FQDN at connect time.

## Background and rationale

### The original problem

Envoy's `transport_socket_matches` (the older pattern for per-host TLS) requires
the cluster config to declare every possible TLS profile upfront and for the
dynamic module to set the right metadata key on each host. This works but
couples the cluster config to the set of TLS profiles needed by the Go module —
adding a new CA or cert means changing both Go code and YAML.

A simpler design uses a *single* static `UpstreamTlsContext` at the cluster
level with:

```yaml
transport_socket:
  name: envoy.transport_sockets.tls
  typed_config:
    auto_host_sni: true
    auto_sni_san_validation: true
    common_tls_context:
      validation_context:
        trusted_ca: { filename: /etc/ssl/cert.pem }
```

With `auto_host_sni`, Envoy reads `HostDescription::hostname()` from the
selected upstream host and uses it as both the TLS SNI and the SAN matcher. No
per-host YAML required — the cluster config is completely independent of which
FQDNs the Go module registers.

### The blocker: hostname was absent from the dynamic-modules ABI

The original `envoy_dynamic_module_callback_cluster_add_hosts` ABI accepted only
`ip:port` addresses, weights, locality strings, and metadata. There was no field
for a logical hostname. As a result, `HostImpl::hostname()` was always empty for
hosts registered by a dynamic module, and `auto_host_sni` synthesized a broken
SNI of the form `<cluster_name><ip:port>`, which failed both the server's SNI
routing and Envoy's own SAN validation (`CERTIFICATE_VERIFY_FAILED`).

### The fix: patched ABI

A new ABI callback was introduced:

```c
bool envoy_dynamic_module_callback_cluster_add_hosts_with_hostnames(
    envoy_dynamic_module_type_cluster_envoy_ptr cluster_envoy_ptr,
    uint32_t priority,
    const envoy_dynamic_module_type_module_buffer* addresses,
    const envoy_dynamic_module_type_module_buffer* hostnames,   // ← new
    ...);
```

The `hostnames` array is passed parallel to `addresses`. Envoy stores each
entry on the `HostImpl`, so `HostDescription::hostname()` returns the correct
FQDN at connect time. `auto_host_sni` then reads it and the TLS handshake
succeeds.

The patch is at:
```
https://gist.githubusercontent.com/dio/e4d1c59710a2039d146deb98ee19977c/raw/.../auto-host-sni-hostname-abi.patch
```

A patched Envoy binary is published as the `envoy-0d6e3c60-auto-host-sni`
release on [dio/envoy-builder](https://github.com/dio/envoy-builder/releases).

### Transit side

`down/cluster.go` gained a `Hostname string` field on `HostSpec`. When non-empty
it is forwarded to the new ABI callback. On the Go side, setting
`spec.Hostname = "httpbin.org"` is all that's needed — no per-host YAML, no
metadata plumbing.

The cluster config JSON uses the `sni` field in the host entry, which
`cluster_async_router.go` maps to both `spec.Hostname` (for the ABI) and
`spec.Metadata["sni"]` (kept for backwards compatibility with
`transport_socket_matches`-based configs):

```json
{"hosts":[
  {"name":"httpbin","address":"httpbin.org:443","sni":"httpbin.org"},
  {"name":"example","address":"example.com:443","sni":"example.com"}
]}
```

## What this test covers

- Two TLS upstreams (`httpbin.org:443`, `example.com:443`) behind a single
  cluster with one static `UpstreamTlsContext`.
- Verifies `auto_host_sni` reads the FQDN from the ABI-registered hostname and
  produces the correct SNI on the wire.
- A non-5xx response from each public endpoint confirms TLS connected and SAN
  validation passed. A 503 with `CERTIFICATE_VERIFY_FAILED` or
  `upstream_connection_failure` means the hostname was empty.

Plain HTTP targets are intentionally excluded: a single static `UpstreamTlsContext`
applies to the whole cluster, so hosts that speak plain HTTP cannot share it.
For mixed traffic see `e2e-static-tls-matches`.

## Prerequisites

| Requirement | How to satisfy |
|---|---|
| Patched Envoy binary | `make download-envoy` (downloads from `envoy-0d6e3c60-auto-host-sni` release) |
| Shared library | `make build EXAMPLE=cluster-async-router` from repo root |
| System CA bundle | `/etc/ssl/cert.pem` (macOS) or `/etc/ssl/certs/ca-certificates.crt` (Linux) |
| Internet access | Tests reach `httpbin.org` and `example.com` directly |

## Running

```sh
# From repo root
make -C examples/cluster-async-router e2e-static-tls

# Or directly
cd examples/cluster-async-router/e2e-static-tls
ENVOY_BIN=../../../.bin/envoy go test -v -timeout 60s
```

## Expected output

```
TestStaticTLS_Httpbin   status=405   PASS
TestStaticTLS_Example   status=403   PASS
```

Both return a non-5xx status, confirming the TLS handshake and SAN validation
succeeded. The exact status code depends on what the public endpoint returns for
`POST /`.

## Relationship to `e2e-static-tls-matches`

This experiment uses the simpler of the two TLS approaches (one socket for the
whole cluster). `e2e-static-tls-matches` proves the alternative: static
`transport_socket_matches` buckets with per-host metadata, which additionally
supports mixing plaintext and TLS hosts in one cluster. Both approaches require
the patched ABI for the hostname field.
