---
name: transit-body-driven-cluster-routing
description: Route to a dynamic-modules cluster based on data parsed from the request body (e.g. the OpenAI `model` field) when ChooseHost runs before the body filter does. Covers the phase-ordering trap, the auto_sni timing trap, and the async ClusterLBCompletion pattern.
---

# Transit body-driven cluster routing

Use this skill when an HTTP filter must read the request body to decide which
upstream a `CLUSTER_PROVIDED` cluster (dynamic-modules `ClusterLB.ChooseHost`)
selects. The canonical case is the `orange` LLM proxy classifying on the
`model` field of `/v1/chat/completions`.

Hard-won from building `examples/orange/`.

## Why this is not just "set filter state and read it in ChooseHost"

The naive design — `RegisterWithMutableBody` writes filter state in
`bodyHandler`, `ChooseHost` reads it via `ctx.GetFilterState` — does not work.
The SDK's `OnRequestHeaders` returns `HeadersStatusContinue` even in
mutable-body mode (`up/filter.go`, the path through `OnRequestHeaders` →
`flush(false)` → `HeadersStatusContinue`). That means:

1. classify.decodeHeaders → Continue
2. router.decodeHeaders → opens upstream conn pool → **ChooseHost runs here**
3. body chunks arrive → classify.decodeData → filter state written → **too late**

`HeadersStatusStopAllAndBuffer` is explicitly unsupported (see comment at
`up/filter.go` around the `stripFramingOnResume` block: "the SDK has no async
resume path for that status and it freezes the filter chain permanently").

Symptom: `upstream_cx_none_healthy` increments even though
`membership_healthy > 0`. The cluster has hosts, but ChooseHost returned nil
because filter state was empty.

## Two working patterns

### Header-driven (fast, demoable, what `examples/cluster-router` does)

Client sends the routing signal as a request header
(`x-model: gpt-4o-mini`). classify reads it in `requestHandler` (headers
phase), writes filter state synchronously. ChooseHost reads
`ctx.GetHeader("x-model")` directly or `ctx.GetFilterState("…upstream")`.
No body parsing in the routing path.

Trade-off: not transparent to OpenAI/Anthropic SDK clients — they don't send
custom headers.

### Body-driven via `ClusterLBCompletion` (real)

Use the async completion pattern documented at
`docs/envoy-dynamic-module-upstream-selection.md` lines 154–235.

1. classify keeps `RegisterWithMutableBody`. `requestHandler` mints a token
   (process-unique), registers a `*pending` in a `sync.Map`, writes the token
   to filter state (`orange.cls-token`), and stashes it in `streamCtx` for
   the body handler.
2. classify `bodyHandler` parses the model, looks up the upstream, writes the
   real filter state and dynamic metadata, then `pending.Resolve(upstream)`.
   It also deletes the registry entry.
3. `ChooseHost` reads the token from filter state. If present, calls
   `ctx.NewCompletion()`, registers a waiter on the `pending`, and returns
   `(nil, completion)`. The waiter runs from the classify-body goroutine;
   it calls `handle.Schedule(func() { completion.Complete(host, "") })`.
4. `CancelHostSelection` removes the waiter so the pending goroutine exits if
   the client aborts.

The token can be `x-request-id` (Envoy populates it by default) to avoid
minting your own — both filters can read it via `r.Header` / `ctx.GetHeader`.

## The `:authority` / `auto_sni` trap

For HTTPS upstreams with `auto_sni: true` + `auto_san_validation: true`, do
**not** rewrite `:authority` from an upstream HTTP filter — `auto_sni` is
sampled from the request **before** the upstream filter chain runs (TLS
handshake happens first). Symptom: SNI is `localhost:8080` (or whatever the
client sent), cert validation fails, `upstream_cx_connect_fail++`,
`remote connection failure`.

Rewrite `:authority` in the **downstream** filter (classify), in the same
place you write the routing filter state. credinject keeps the
strip-headers + inject-credential responsibilities.

### Auto-SNI sampling time (and why body-driven multi-provider can't use it)

Empirically, with the body-driven `ClusterLBCompletion` pattern:

- `:authority` set from the **headers** handler reaches the TLS handshake.
- `:authority` set from the **body** handler does **not**, even though the
  upstream HTTP filter (e.g. credinject) sees the post-mutation value.

The most consistent explanation: Envoy's router captures the SNI value from
`:authority` while preparing the upstream stream during `router.decodeHeaders`,
before `ChooseHost` runs. When `ChooseHost` returns a `ClusterLBCompletion`,
the host attaches later but the SNI is already locked. The body handler runs
in between, mutates request headers, and those mutations propagate to the
upstream HTTP filter chain — but not back into the SNI that was already
captured. `override_auto_sni_header` has the same timing, so swapping which
header SNI reads from does not help.

Consequence: body-driven routing across providers with different hostnames
**cannot rely on `auto_sni`**. Workarounds, in increasing production-fitness:

1. **Demo / single-provider**: disable `auto_sni`/`auto_san_validation` and
   set explicit `sni:` in the cluster's `UpstreamTlsContext`. Locks the
   cluster to one provider; trivial. Was the M3c demo path in
   `examples/orange/envoy.tmpl.yaml` (hardcoded to `api.openai.com`) before
   workaround #3 was implemented there.
2. **Per-provider cluster**: one strict-DNS cluster per upstream, each with
   its own `UpstreamTlsContext.sni`. classify mutates route or cluster
   header → routing picks the right cluster → static SNI per cluster.
   Production-correct but causes config explosion in gateway scenarios
   (one cluster per upstream per LLM provider).
3. **Single dynamic cluster + per-host transport socket** via
   `transport_socket_matches` keyed on host metadata. **Implemented** in
   both `examples/cluster-async-router` (mechanism demo with self-signed TLS
   tripwire e2e) and `examples/orange` (multi-provider LLM proxy:
   OpenAI + Anthropic on a single dynamic cluster, each with its own
   `UpstreamTlsContext`).

   `down.HostSpec` now carries `Metadata map[string]string`; `AddHosts` in
   `down/abi_impl/cluster.go` plumbs each entry as a
   `(envoy.transport_socket_match, key, value)` triple. Set `"sni"` to the
   upstream hostname when registering a host:

   ```go
   h.AddHosts([]up.HostSpec{{
       Address:  "93.184.216.34:443",
       Metadata: map[string]string{"sni": "example.com"},
   }})
   ```

   Then add a `transport_socket_matches` entry to the cluster YAML that
   matches on `sni: example.com` and carries the corresponding
   `UpstreamTlsContext`. Envoy selects the matching entry per connection so
   each host handshakes with the correct SNI. The demo config in
   `examples/cluster-async-router/envoy.yaml` shows two HTTPS upstreams
   (`httpbin.org` and `example.com`) in a single dynamic cluster, each with
   its own TLS context. Verify with:

   ```
   curl http://127.0.0.1:9901/clusters | grep -E 'cx_total|cx_connect_fail'
   ```

   Both hosts should show `cx_total > 0` and no `cx_connect_fail`. This is
   the pattern `examples/orange` should adopt to drop its hardcoded
   `sni: api.openai.com`.

## TLS trust store on macOS

`auto_san_validation: true` requires a trust store. Envoy on macOS does not
load the system roots by default. For local dev/demo:

```yaml
transport_socket:
  name: envoy.transport_sockets.tls
  typed_config:
    "@type": type.googleapis.com/envoy.extensions.transport_sockets.tls.v3.UpstreamTlsContext
    common_tls_context:
      validation_context:
        trusted_ca:
          filename: /etc/ssl/cert.pem
```

`/etc/ssl/cert.pem` is the macOS system bundle path. For Linux, prefer
`/etc/ssl/certs/ca-certificates.bundle` (Debian/Ubuntu) or
`/etc/pki/tls/certs/ca-bundle.crt` (RHEL/Fedora). Make this configurable
when shipping an example.

## Diagnostics that actually helped

- `curl http://127.0.0.1:9901/clusters` shows per-host cx counters. A
  `cx_connect_fail` on the **wrong** host's IP is the giveaway that routing
  picked something but TLS/transport broke before headers.
- `curl http://127.0.0.1:9901/stats | grep -E 'orange|none_healthy|connect_fail'`
  distinguishes "ChooseHost returned nil" (`upstream_cx_none_healthy`) from
  "ChooseHost returned a host but the connection broke" (`upstream_cx_connect_fail`).
- `w.Log(up.LogInfo, "…")` from both classify and credinject, printing the
  selected upstream and the current `:authority`, makes the phase-ordering
  bug obvious in seconds. Envoy `--log-level info` shows them on stderr.

## Phase-ordering cheat sheet

```
                 downstream phase                   upstream phase
client -> HCM -> classify.decodeHeaders       -> router -> ChooseHost
              -> classify.decodeData (body)      -> upstream HTTP filters
                                                    (credinject)
                                                 -> TLS handshake
                                                    (auto_sni reads :authority)
                                                 -> headers written upstream
```

Rules that fall out:

- Anything that must influence ChooseHost has to be in place before
  `router.decodeHeaders` finishes — i.e. either in the request headers, in
  filter state written from a downstream headers callback, or behind a
  `ClusterLBCompletion`.
- Anything that must influence the upstream TLS handshake (`:authority` for
  `auto_sni`, override host) has to be in place before the upstream conn
  pool opens the connection — i.e. downstream phase, not upstream filters.
- Upstream HTTP filters are good for credential injection, header stripping,
  and final request shaping, but they are **not** a routing extension point.
