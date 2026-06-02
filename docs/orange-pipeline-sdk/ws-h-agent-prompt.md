# WS-H Agent Prompt

Prompt for an agent to implement WS-H — Envoy Gateway Transport Integration.

---

**Task:** Implement WS-H — Envoy Gateway Transport Integration

**Repo:** `/Users/dio/src/dio/transit2`. Go workspace; integrations live under `integrations/`, SDK under `up/`, docs under `docs/`.

---

### Background

WS-H is the final workstream in the Orange Pipeline SDK plan (`docs/orange-pipeline-sdk/plan.md`). WS-A through WS-G are complete. All prior workstreams have been committed to `main`.

The WS-B research spike (docs: `docs/auto-host-sni-verdict.md`) proved **V1: fully dynamic host + Envoy-owned TLS**. For public WebPKI providers, a single static `UpstreamTlsContext` on the cluster handles all upstream TLS — Envoy derives SNI and SAN validation from `HostDescription::hostname()` at connect time. Provider add/remove requires only pipeline config changes; no EPP update, no xDS reconciliation, no Envoy restart.

The V1 transport block (proven in `examples/cluster-async-router/e2e-static-tls/`):

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

The Go side is already wired: `HostSpec.Hostname` in `down/cluster.go`, passed via `add_hosts_with_hostnames` in `down/abi_impl/cluster.go`. Examples that already set `spec.Hostname`: `examples/cluster-async-router/cluster_async_router.go` (`spec.Hostname = he.SNI`).

Detailed prep, per-integration audit, EPP shape, PR sequence, and runbook are in:
- `docs/orange-pipeline-sdk/ws-h-transport.md`
- `docs/provider-runbook.md`

Read both before starting. The analysis there is complete; your job is to implement it.

---

### What exists in each integration today

**`integrations/cluster-async-router-eg/k8s/`**
- `epp.tmpl.yaml` — baseline, uses `transport_socket_matches` per host
- `epp-auto-host-sni.tmpl.yaml` — WS-B experiment, still has `transport_socket_matches` (not V1)
- `epp-static-tls-matches.tmpl.yaml`, `epp-sds.tmpl.yaml`, `epp-authority-rewrite.tmpl.yaml` — other WS-B experiments
- e2e run: `RUN_CLUSTER_ASYNC_ROUTER_EG_E2E=1 make -C integrations/cluster-async-router-eg e2e`

**`integrations/tiered-router-eg/k8s/`**
- `epp-l2.tmpl.yaml` — L2 cluster patch, **no `transport_socket` at all**
- e2e run: `make -C integrations/tiered-router-eg e2e`

**`integrations/mcp-profile-tiered-router-eg/k8s/`**
- `epp-l2.tmpl.yaml` — L2 cluster patch, **no `transport_socket` at all**
- e2e run: `make -C integrations/mcp-profile-tiered-router-eg e2e`

**`integrations/tiered-ws-proxy-eg/k8s/`**
- `l2-epp.tmpl.yaml` — six patches; egress cluster replace (Patch 4 area) has **no `transport_socket`**
- e2e blocked on Linux Envoy build (WS-G pending) — run when build lands

---

### Your work: five PRs in order

#### PR 1 — `cluster-async-router-eg`: add `epp-v1.tmpl.yaml`

Create `integrations/cluster-async-router-eg/k8s/epp-v1.tmpl.yaml`.

Start from the existing `epp.tmpl.yaml`. Keep the HCM listener patch (Patch 1) and the cluster replace patch (Patch 2) unchanged **except**: remove the entire `transport_socket_matches` block and replace it with the single static `transport_socket` block above. The `trusted_ca` path inside the cluster e2e is `/etc/envoy/tls/ca.pem` (local e2e CA) — but in the EG integration the system CA is correct since the e2e targets real public hosts (`httpbin.org`, `example.com`). Check what the existing `epp.tmpl.yaml` uses for `trusted_ca` and match it.

In `integrations/cluster-async-router-eg/e2e/suite_test.go`, add or update the test variant that applies `epp-v1.tmpl.yaml`. The four-test canonical suite (`TestAsyncRouter_TLS_Httpbin`, `TestAsyncRouter_TLS_Example`, `TestAsyncRouter_Plaintext`, `TestAsyncRouter_UnknownTarget`) must pass with the V1 EPP. Look at how other EPP variants are selected in the suite to understand the pattern.

**Exit:** `RUN_CLUSTER_ASYNC_ROUTER_EG_E2E=1 make -C integrations/cluster-async-router-eg e2e` green.

#### PR 2 — `tiered-router-eg`: add transport block to L2 EPP

Edit `integrations/tiered-router-eg/k8s/epp-l2.tmpl.yaml`. In the cluster replace patch value, add the `transport_socket` block immediately before `typed_extension_protocol_options` (or wherever fits the cluster structure — read the file first). The existing e2e uses plaintext mock upstreams; the block is additive and must not break them.

Also check: does the tiered-router example set `HostSpec.Hostname` when calling `AddHosts`? Find the cluster router code (likely in `examples/tiered-router/` or an equivalent). If `HostSpec.Hostname` is empty, the SNI will be wrong in production. If it's already set (from the provider catalog's `sni`/`hostname` field), document that in a comment in the EPP. If it's missing, add the wire-up in the same PR.

**Exit:** `make -C integrations/tiered-router-eg e2e` green (plaintext mock path stays green).

#### PR 3 — `mcp-profile-tiered-router-eg`: same delta

Same as PR 2, applied to `integrations/mcp-profile-tiered-router-eg/k8s/epp-l2.tmpl.yaml`. Check `HostSpec.Hostname` wiring in the MCP cluster router.

**Exit:** `make -C integrations/mcp-profile-tiered-router-eg e2e` green.

#### PR 4 — `tiered-ws-proxy-eg`: add transport block to egress cluster + run WS-G e2e

In `integrations/tiered-ws-proxy-eg/k8s/l2-epp.tmpl.yaml`, find the patch that replaces the **egress** cluster (the one that routes from the embedded ws-proxy server to the real upstream — NOT the inbound STATIC loopback cluster pointing at `127.0.0.1:10001`). Add the `transport_socket` block there.

The inbound loopback cluster (Patch 4 in the current file, `type: STATIC`, address `127.0.0.1:10001`) must stay as-is — no TLS on the loopback.

This PR also closes the WS-G pending e2e. Run it once the Linux Envoy build is available: `make -C integrations/tiered-ws-proxy-eg e2e`. The test must prove the egress-via-Envoy double-loopback path.

**Exit:** `make -C integrations/tiered-ws-proxy-eg e2e` green; egress-via-Envoy path confirmed in test output.

#### PR 5 — cross-cutting verification pass

After PRs 1–4 are green:
- Add a comment or `TODO` removal in `docs/orange-pipeline-sdk/plan.md` marking WS-H as complete (the plan has an exit criterion table — update the WS-H row).
- Confirm `docs/provider-runbook.md` and `docs/orange-pipeline-sdk/ws-h-transport.md` accurately reflect what shipped. If the EPP shapes or cluster names differ from what's in the docs, update the docs to match.

---

### Constraints

- **Do not touch `epp.tmpl.yaml`** (baseline) in any integration — it stays as the `transport_socket_matches` reference for the `epp-authority-rewrite` and `epp-sds` experiments.
- **Do not add `transport_socket` to the inbound STATIC loopback cluster** in `tiered-ws-proxy-eg` — only the egress cluster gets it.
- **Do not break existing e2e green paths.** Every integration's current e2e must stay green after your PR.
- **Egress-via-Envoy must be preserved** in `tiered-ws-proxy-eg` — the double-loopback architecture (ws-proxy sidecar → Envoy egress → upstream) is a WS-G/WS-H correctness invariant.
- Follow the existing commit message style: `feat(<scope>): <description>` with workstream tag in body.

---

### Key file references

| File | Purpose |
|---|---|
| `docs/auto-host-sni-verdict.md` | Full V1 proof, ABI patch details, passing test results |
| `docs/orange-pipeline-sdk/ws-h-transport.md` | Per-integration audit + annotated EPP diff |
| `docs/provider-runbook.md` | Operational runbook (verify your EPPs satisfy it) |
| `examples/cluster-async-router/e2e-static-tls/testdata/envoy.tmpl.yaml` | Canonical V1 local-Envoy config to copy `transport_socket` structure from |
| `integrations/cluster-async-router-eg/k8s/epp.tmpl.yaml` | Baseline EPP structure to start from for PR 1 |
| `down/cluster.go` | `HostSpec.Hostname` field definition |
| `down/abi_impl/cluster.go` | `AddHosts` implementation, `add_hosts_with_hostnames` call |
