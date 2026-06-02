# cluster-async-router: canonical e2e suite

This is the permanent integration suite for the `cluster-async-router` example.
It uses the **bucket-keyed static `transport_socket_matches`** strategy
(`e2e-static-tls-matches`): two static buckets (`tls-system-ca` and
`plaintext`) selected via host metadata, with `auto_host_sni` reading the
per-host hostname from the patched ABI.

## Tests

| Test | Target | Bucket | Assertion |
|---|---|---|---|
| `TestAsyncRouter_TLS_Httpbin` | `httpbin.org:443` | `tls-system-ca` | status < 500 (TLS handshake OK) |
| `TestAsyncRouter_TLS_Example` | `example.com:443` | `tls-system-ca` | status < 500 (TLS handshake OK) |
| `TestAsyncRouter_Plaintext` | local `httptest.Server` | `plaintext` | status == 200 |
| `TestAsyncRouter_UnknownTarget` | (unregistered) | — | status == 503 |

## Run

```sh
make -C examples/cluster-async-router e2e
```

## Related

- `e2e-static-tls/` — experiment using per-host TLS servers with local cert generation
- `e2e-static-tls-matches/` — experiment that proved the bucket-keyed approach; this suite graduates it to canonical
