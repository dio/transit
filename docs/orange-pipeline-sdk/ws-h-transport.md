# WS-H: Envoy Gateway Transport Integration

Status: planning — pending Linux Envoy build for tiered-ws-proxy-eg e2e.

Related:
- `docs/auto-host-sni-verdict.md` — WS-B proof; V1 verdict
- `docs/provider-runbook.md` — operational runbook for provider add/remove
- `docs/orange-pipeline-sdk/plan.md` — WS-H workstream definition

## V1 Transport Approach

**WS-B verdict: V1 — fully dynamic host + Envoy-owned TLS.**

For ordinary public WebPKI providers, a single static `UpstreamTlsContext` on
the cluster handles all upstream TLS. Envoy derives SNI and SAN validation
target from `HostDescription::hostname()` at connect time. Provider add/remove
is pipeline config only — no EPP update, no xDS reconciliation, no Envoy
restart.

### Proven transport block

Taken verbatim from the passing `e2e-static-tls` suite in
`examples/cluster-async-router`. Both `TestStaticTLS_Httpbin` and
`TestStaticTLS_Example` pass against real public WebPKI hosts.

```yaml
transport_socket:
  name: envoy.transport_sockets.tls
  typed_config:
    "@type": type.googleapis.com/envoy.extensions.transport_sockets.tls.v3.UpstreamTlsContext
    auto_host_sni: true
    auto_sni_san_validation: true
    common_tls_context:
      validation_context:
        trusted_ca: { filename: /etc/ssl/certs/ca-certificates.crt }
```

The only requirement on the Go side: `HostSpec.Hostname` must be set to the
provider FQDN before `AddHosts`. `down/abi_impl/cluster.go` already passes
hostname alongside address via `add_hosts_with_hostnames` (ABI patch from
WS-B). `examples/cluster-async-router` sets `spec.Hostname = he.SNI` at the
call site.

### When the single static socket does not apply

A cluster cannot mix TLS and plaintext hosts under a single static
`transport_socket` — every host in the cluster receives a TLS handshake attempt.
For clusters with both TLS and plaintext hosts, use `transport_socket_matches`
with a bucket key (`sni` metadata) to select the right context per host. The
`epp-static-tls-matches.tmpl.yaml` in `cluster-async-router-eg` is the
reference for that variant.

For providers using a private CA or mutual TLS, the two-channel rollout applies
— see `docs/provider-runbook.md`.

## Four EG Integration Audit

### `cluster-async-router-eg`

**Current state.** Has `epp-auto-host-sni.tmpl.yaml` which uses `auto_sni` /
`auto_san_validation` on `upstream_http_protocol_options` but still carries
`transport_socket_matches` entries for host selection. That is an intermediate
experiment from WS-B (Experiment 5 — `:authority`-derived SNI timing), not the
V1 production path.

**WS-H delta.**
Add `epp-v1.tmpl.yaml`: same cluster patch as `epp.tmpl.yaml` but replace the
`transport_socket_matches` block entirely with the single static
`transport_socket` above. Drop all per-host `transport_socket` entries. Update
`e2e/suite_test.go` to use the V1 EPP. The canonical e2e (four tests) must stay
green.

This is the primary proof that EG can express the V1 transport without listing
every provider — `cluster-async-router-eg` is the SNI tripwire integration.

### `tiered-router-eg`

**Current state.** L2 EPP cluster patch has no `transport_socket` at all.
All current e2e traffic flows over plaintext to in-cluster mocks. The control
plane reads provider routes from `tiered-router-control` and the cluster config
carries `hostname` in each route entry, but `HostSpec.Hostname` is not yet wired
through to `AddHosts`.

**WS-H delta.**
1. Add the single static `transport_socket` block to the L2 cluster patch in
   `epp-l2.tmpl.yaml`. The mock upstream does not terminate TLS, so the block is
   present but dormant in the existing e2e — it becomes active when a real
   provider (OpenAI, Bedrock) is the target.
2. Verify `HostSpec.Hostname` is set from the provider hostname in the cluster
   router (`examples/tiered-router` or its SDK successor). If not, wire it
   alongside `HostSpec.Address`.
3. Add an e2e case or note confirming the block does not break the plaintext mock
   path (expected: block is additive; mock host has no TLS, so connections
   succeed on the plaintext path — but only if the cluster is not forced into TLS
   for all hosts; confirm the mock uses a separate cluster or the same cluster
   without the `transport_socket` forcing TLS on that host).

### `mcp-profile-tiered-router-eg`

**Current state.** Same as `tiered-router-eg` — no TLS on L2 EPP. MCP server
routes are resolved at L1 classify time; L2 receives the `x-mcp-server` header
and routes to the MCP server cluster. No `HostSpec.Hostname` wired.

**WS-H delta.** Same two steps as `tiered-router-eg` — add `transport_socket`
to L2 EPP and wire `HostSpec.Hostname` from MCP server catalog entry. The
plaintext mock path must stay green.

### `tiered-ws-proxy-eg`

**Current state.** The egress cluster on L2 routes WebSocket frames to a
plaintext mock upstream. Egress-via-Envoy path is structurally wired (Patch 5 +
Patch 6 in `l2-epp.tmpl.yaml`) but `transport_socket` is absent.

**WS-H delta.** Add `transport_socket` block to the L2 egress cluster replace
patch so that when `WSPROXY_EGRESS_URL` points at a real `wss://` upstream,
Envoy handles TLS origination with `auto_host_sni`. In the e2e the egress
cluster still reaches the plaintext mock — the block is additive and harmless.

This integration also carries the pending WS-G e2e (blocked on Linux Envoy
build). The WS-H delta on this integration unblocks after that build lands and
the WS-G e2e passes.

## EPP Shape: L2 Cluster Patch with V1 Transport

Annotated diff versus the current `tiered-router-eg/k8s/epp-l2.tmpl.yaml`
cluster replace patch. Lines marked `+` are WS-H additions; everything else is
unchanged.

```yaml
- type: "type.googleapis.com/envoy.config.cluster.v3.Cluster"
  name: "{{.ClusterName}}"
  operation:
    op: replace
    path: ""
    value:
      name: "{{.ClusterName}}"
      connect_timeout: 5s
      lb_policy: CLUSTER_PROVIDED
+     # V1 generic transport. SNI and SAN validation derived from
+     # HostSpec.Hostname at connect time. No transport_socket_matches.
+     # Provider add/remove is pipeline config only.
+     transport_socket:
+       name: envoy.transport_sockets.tls
+       typed_config:
+         "@type": type.googleapis.com/envoy.extensions.transport_sockets.tls.v3.UpstreamTlsContext
+         auto_host_sni: true
+         auto_sni_san_validation: true
+         common_tls_context:
+           validation_context:
+             trusted_ca: { filename: /etc/ssl/certs/ca-certificates.crt }
      typed_extension_protocol_options:
        envoy.extensions.upstreams.http.v3.HttpProtocolOptions:
          # ... unchanged
      cluster_type:
        # ... unchanged
```

For `tiered-ws-proxy-eg` the patch target is the egress cluster replace in
`l2-epp.tmpl.yaml` (Patch 4 area), not the inbound STATIC loopback cluster.

## PR Sequence

Each PR is independently mergeable; later PRs are smaller because the shape is
proven in the earlier one.

### PR 1 — `cluster-async-router-eg` V1 EPP

**Scope.** Add `epp-v1.tmpl.yaml` in `integrations/cluster-async-router-eg/k8s/`.
Update `e2e/suite_test.go` to apply it. Four-test canonical suite must stay
green. This is the SNI tripwire proof for EG.

**Exit.** `RUN_CLUSTER_ASYNC_ROUTER_EG_E2E=1 make -C integrations/cluster-async-router-eg e2e` green.

### PR 2 — `tiered-router-eg` transport

**Scope.** Add `transport_socket` block to `epp-l2.tmpl.yaml`. Wire
`HostSpec.Hostname` in the cluster router if not already set. Existing e2e
(plaintext mock) must stay green; new note in e2e comment confirms TLS block is
additive.

**Exit.** `make -C integrations/tiered-router-eg e2e` green.

### PR 3 — `mcp-profile-tiered-router-eg` transport

**Scope.** Same delta as PR 2, applied to `mcp-profile-tiered-router-eg`.

**Exit.** `make -C integrations/mcp-profile-tiered-router-eg e2e` green.

### PR 4 — `tiered-ws-proxy-eg` transport + WS-G e2e close

**Scope.** Add `transport_socket` to L2 egress cluster patch. Run the WS-G
pending e2e (egress-via-Envoy double-loopback). Depends on Linux Envoy build.

**Exit.** `make -C integrations/tiered-ws-proxy-eg e2e` green; WS-G exit
criterion met.

### PR 5 — provider runbook

**Scope.** `docs/provider-runbook.md` (see that file). No code change.

## Remaining WS-B Experiments (non-blocking)

These transfer from WS-B to WS-H per the verdict doc. None change the V1
verdict; they are additive research that may sharpen the runbook or simplify EG
config.

- **Experiment 5:** `auto_sni` from `:authority` header timing. Determine
  whether `:authority` rewrite from the body phase reaches the router before SNI
  is derived. Existing `epp-authority-rewrite.tmpl.yaml` is the rig.
- **Experiments 6–7:** Envoy Gateway native Gateway API vs EPP comparison. Can
  Backend + BackendTLSPolicy express the V1 transport without EPP for naturally
  bounded provider families?
- **Experiment 9:** Cohere-style provider add on EG. Pipeline config only, no
  Gateway resource change, TLS originates correctly.

## Verification Matrix (WS-H specific)

| Test | Integration | What it proves |
|---|---|---|
| SNI tripwire: `epp-v1` | `cluster-async-router-eg` | EG expresses V1 transport; per-host `transport_socket_matches` not required |
| Plaintext mock + TLS block | `tiered-router-eg` | Transport block is additive; existing routing unbroken |
| MCP plaintext mock + TLS block | `mcp-profile-tiered-router-eg` | Same, MCP path |
| WS egress-via-Envoy | `tiered-ws-proxy-eg` | Double-loopback preserved; TLS block on egress cluster |
| Provider add config-only | Any | Adding a host with new `HostSpec.Hostname` routes with valid TLS; no EPP change |
