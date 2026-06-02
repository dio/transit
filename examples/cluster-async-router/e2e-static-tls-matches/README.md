# e2e-static-tls-matches

End-to-end test that validates **metadata-driven static `transport_socket_matches`**
on a dynamic-modules cluster. A single cluster serves both TLS and plaintext
upstreams: Envoy selects the right socket factory per host using a `bucket`
field in the `envoy.transport_socket_match` endpoint metadata namespace. No
runtime mutation of transport sockets is performed.

## Background and rationale

### Why `transport_socket_matches` instead of a single static socket

A single static `UpstreamTlsContext` (as in `e2e-static-tls`) applies TLS to
every host in the cluster. This is ideal when all upstreams speak TLS, but
breaks as soon as any host speaks plain HTTP — Envoy wraps the plaintext
connection in a TLS handshake that the upstream does not expect.

The standard Envoy solution is `transport_socket_matches`: declare several named
socket profiles on the cluster at config time, then have each host carry
endpoint metadata that selects the right profile. Envoy evaluates the match at
connect time — no dynamic reconfiguration needed.

```yaml
transport_socket_matches:
  - name: tls-system-ca
    match: { bucket: tls-system-ca }
    transport_socket:
      name: envoy.transport_sockets.tls
      ...
  - name: plaintext
    match: { bucket: plaintext }
    transport_socket:
      name: envoy.transport_sockets.raw_buffer
      ...
```

### The metadata namespace

Envoy reads transport socket match metadata from the
`envoy.transport_socket_match` endpoint metadata namespace. The match field in
the cluster config is a free-form struct; each key-value is compared against the
endpoint's metadata under that namespace.

In this experiment the discriminator is a single key `bucket`. Each host
registered by the Go module carries:

```json
"metadata": {
  "envoy.transport_socket_match": { "bucket": "tls-system-ca" }
}
```

or

```json
"metadata": {
  "envoy.transport_socket_match": { "bucket": "plaintext" }
}
```

The value flows from the cluster config JSON (`"bucket"` field in each host
entry) through `HostSpec.Metadata` in Go down to the ABI call
`envoy_dynamic_module_callback_cluster_add_hosts_with_hostnames`, which
serializes metadata as flat key=value pairs. `cluster_async_router.go` promotes
the `bucket` field to the `envoy.transport_socket_match` namespace when calling
`AddHosts`.

### The hostname ABI dependency

TLS hosts (`tls-system-ca` bucket) still require the patched ABI hostname field
so that `auto_host_sni` and `auto_sni_san_validation` read the real FQDN
instead of the synthesized `<cluster_name><ip:port>` string. See
[`e2e-static-tls/README.md`](../e2e-static-tls/README.md) for full details on
that patch.

Plaintext hosts do not need a hostname.

### Comparison with `e2e-static-tls`

| | `e2e-static-tls` | `e2e-static-tls-matches` |
|---|---|---|
| Transport socket | Single static TLS | Two static buckets (TLS + plaintext) |
| Per-host config | `sni` field only | `sni` + `bucket` fields |
| Mixed plain/TLS | Not supported | Supported |
| Runtime mutation | None | None |
| ABI patch needed | Yes (hostname) | Yes (hostname, for TLS hosts) |

Both approaches are simpler than the earlier pattern of mutating
`transport_socket_matches` at runtime from the Go module.

## What this test covers

Three hosts, one cluster, one Envoy run:

| Host | Address | Bucket | Expected |
|---|---|---|---|
| `httpbin` | `httpbin.org:443` | `tls-system-ca` | < 500 (TLS handshake succeeded) |
| `example` | `example.com:443` | `tls-system-ca` | < 500 (TLS handshake succeeded) |
| `plain` | `127.0.0.1:<port>` | `plaintext` | 200 (plaintext, no TLS) |

The plaintext server is an in-process `httptest.Server` started by the test
itself. Its port is embedded into the cluster config JSON at test startup, so no
fixed port is required.

## Prerequisites

| Requirement | How to satisfy |
|---|---|
| Patched Envoy binary | `make download-envoy` (downloads `envoy-darwin-arm64-auto-host-sni` from `envoy-0d6e3c60-auto-host-sni` release) |
| Shared library | `make build EXAMPLE=cluster-async-router` from repo root |
| System CA bundle | `/etc/ssl/cert.pem` (macOS) or `/etc/ssl/certs/ca-certificates.crt` (Linux) |
| Internet access | Tests reach `httpbin.org` and `example.com` directly |

## Running

```sh
# From repo root
make -C examples/cluster-async-router e2e-static-tls-matches

# Or directly
cd examples/cluster-async-router/e2e-static-tls-matches
ENVOY_BIN=../../../.bin/envoy go test -v -timeout 60s
```

## Expected output

```
TestTLSMatches_Httpbin    status=405   PASS
TestTLSMatches_Example    status=403   PASS
TestTLSMatches_Plaintext  status=200   PASS
```

## Debugging with Envoy logs

To confirm which socket bucket Envoy selected for each host, run with upstream
debug logging:

```sh
ENVOY_EXTRA_ARGS="--component-log-level upstream:debug,connection:debug" \
  go test -v -timeout 60s
```

Look for lines like:

```
transport socket match: matched 'tls-system-ca' for host 'httpbin.org'
transport socket match: matched 'plaintext' for host '127.0.0.1:...'
```

For TLS hosts you should also see the SNI on the wire matching the FQDN:

```
[C1] handshaking, SNI: httpbin.org
```

A `CERTIFICATE_VERIFY_FAILED` means the hostname ABI field is empty and
`auto_host_sni` produced a bad SNI — verify the patched Envoy binary is in use.
