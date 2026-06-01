# cluster-async-router-eg — Design Note

Status: P1–P3 implemented. P4 (EPP) dropped — see "Why not EPP" below.
Companion task list lives in the conversation that produced this doc.

## What this integration is for

It is the Envoy Gateway version of `examples/cluster-async-router`, plus the
per-host TLS path the SDK's `HostSpec.Metadata` change unlocks. It exists to
prove three things end-to-end in a real EG environment:

1. The async `ChooseHost` + body-driven routing pattern works behind a Gateway
   that EG translates from Gateway API. (Same shape as `cluster-router-eg`,
   but the routing key comes from the body, not a header.)
2. `HostSpec.Metadata` survives the EG → xDS round trip and reaches the
   transport_socket_matches selector — i.e. the cluster extension can attach
   per-host TLS personality from Go.
3. The whole thing keeps Gateway API as the source of truth. The user wires up
   one logical backend; the cluster extension and an `EnvoyPatchPolicy` do the
   rest.

Why not just extend `cluster-router-eg`: `cluster-router-eg` routes on the
`x-model` header at headers phase, before the body has arrived. There is no
async completion, no token-based handoff, no metadata→SNI path. Bolting body
routing + per-host TLS onto it would obscure both features. A sibling
integration that mirrors `examples/cluster-async-router` is clearer.

## Scope and division of labor

| Concern | Owner | Mechanism |
|---|---|---|
| Body-driven host selection | SDK (Go) | `up.Cluster` async `ChooseHost` + `Pending` registry, as in `examples/cluster-async-router` |
| Per-host TLS personality | SDK + Envoy config | `HostSpec.Metadata{"sni": "..."}` written from Go; matching `transport_socket_matches` injected via `EnvoyPatchPolicy` |
| Cluster substitution | EG + `EnvoyPatchPolicy` | Replace the EG-generated backend cluster with `CLUSTER_PROVIDED` + dynamic-modules cluster (same trick as `cluster-router-eg`) |

P3 is the terminal phase. The static `EnvoyPatchPolicy` carries everything
the per-host TLS path needs:

```
EnvoyPatchPolicy (static)
  ├─ cluster_config.hosts[]            ← our module's host set, sni in metadata
  └─ transport_socket_matches[]        ← sni → UpstreamTlsContext
```

The Go cluster extension picks a host (async, body-driven); the host's
`HostSpec.Metadata{"sni": ...}` selects the matching `transport_socket_match`;
Envoy applies the corresponding `UpstreamTlsContext`. No additional control
plane is required.

## EnvoyPatchPolicy shape

Compared to `cluster-router-eg`, the patch adds one thing: a
`transport_socket_matches` block alongside the replaced cluster.

```yaml
- type: "type.googleapis.com/envoy.config.cluster.v3.Cluster"
  name: "{{.ClusterName}}"     # discovered from /config_dump at e2e time
  operation:
    op: replace
    path: ""
    value:
      name: "{{.ClusterName}}"
      connect_timeout: 5s
      lb_policy: CLUSTER_PROVIDED
      transport_socket_matches:
      - name: host-c
        match: { sni: host-c.test }
        transport_socket:
          name: envoy.transport_sockets.tls
          typed_config:
            "@type": type.googleapis.com/envoy.extensions.transport_sockets.tls.v3.UpstreamTlsContext
            sni: host-c.test
            common_tls_context:
              validation_context:
                trusted_ca: { filename: /etc/envoy/tls/ca.pem }
                match_typed_subject_alt_names:
                  - san_type: DNS
                    matcher: { exact: host-c.test }
      - name: host-d
        match: { sni: host-d.test }
        transport_socket: { ... }
      cluster_type:
        name: envoy.clusters.dynamic_modules
        typed_config:
          "@type": type.googleapis.com/envoy.extensions.clusters.dynamic_modules.v3.ClusterConfig
          dynamic_module_config: { name: cluster-async-router }
          cluster_name: body-router-cluster
          cluster_config:
            "@type": type.googleapis.com/google.protobuf.StringValue
            value: '{"hosts":[
              {"name":"a","address":"upstream-a.default.svc.cluster.local:8080"},
              {"name":"b","address":"upstream-b.default.svc.cluster.local:8080"},
              {"name":"c","address":"upstream-c.default.svc.cluster.local:8443","sni":"host-c.test"},
              {"name":"d","address":"upstream-d.default.svc.cluster.local:8443","sni":"host-d.test"}
            ]}'
```

The listener patch also installs the body-router HTTP filter — same shape as
`cluster-router-eg`'s `cluster-router-debug` filter, just pointed at
`cluster-async-router` and `body-router-writer`.

## Where the CA and certs come from

Two options. P3 picks option B unless we run into trouble.

**A. cert-manager** issues a self-signed cluster issuer, then a `Certificate`
per upstream. Real-world shape; adds an extra dependency for the demo.

**B. e2e harness generates CA + leaf certs in-process** (mirrors the
`examples/cluster-async-router/e2e` work we just did), writes them into k8s
`Secret`s. The TLS upstream Deployments mount the leaf cert; the Envoy
Deployment mounts the CA via a volume that the EnvoyProxy template adds. No
cert-manager dependency, no clock issues, hermetic.

The cost of B is the EnvoyProxy template grows a `volumeMounts` block. That's
local to the integration; acceptable.

## Image layout

Identical to `cluster-router-eg`:

```
transit-cluster-async-router-eg:<tag>            # custom Envoy + libcluster-async-router.so
transit-cluster-async-router-eg-demo:<tag>       # static Go binary, modes: control / upstream / upstream-tls / CLI
```

The demo binary gains an `upstream-tls` mode that takes a leaf cert+key from
mounted files (or env vars) and serves HTTPS with the same `GetCertificate`
SNI tripwire used in `examples/cluster-async-router/e2e/e2e_test.go`. That is
the SHOWCASE: if the metadata→SNI plumbing regresses in any layer (SDK ABI,
EG patch, transport_socket_matches selector), the TLS handshake fails and the
e2e assertion catches it.

## e2e contract

Bootstrap assertions (no control plane required for body-async routing):

- `POST {"target":"a"}` → reaches upstream-a (plaintext).
- `POST {"target":"b"}` → reaches upstream-b (plaintext).
- `POST {"target":"c"}` → reaches upstream-c over TLS, ClientHello SNI=`host-c.test`.
- `POST {"target":"d"}` → reaches upstream-d over TLS, ClientHello SNI=`host-d.test`.
- `POST {"target":"nope"}` → non-200 (unknown_upstream from the async completion).
- `POST {}` → non-200 (missing target).

Negative TLS assertion (the one that catches metadata regressions):

- upstream-c serves a cert ONLY when SNI=host-c.test; any other SNI fails
  handshake. Verified by the SNI tripwire in the TLS upstream itself, same
  pattern as `examples/cluster-async-router/e2e/e2e_test.go:startTLSUpstream`.

Gated by `RUN_CLUSTER_ASYNC_ROUTER_EG_E2E=1` — matches `cluster-router-eg`'s
`RUN_CLUSTER_ROUTER_EG_E2E=1`.

## Why not EPP

EPP (Gateway API Inference Extension EndpointPicker) does **not** configure
`transport_socket_matches`. It's a request-time ext_proc service that picks
a concrete endpoint *within* a cluster Envoy already knows about. Cluster
shape — including `transport_socket_matches` and the dynamic-modules
`cluster_config.hosts` — has to be present at cluster admission time. In
this integration that comes from the static `EnvoyPatchPolicy`.

EPP also overlaps with what our async `ChooseHost` already does (decide
which endpoint a request goes to), and adds nothing to the per-host TLS
story. We considered a P4 that swapped the static host set for an
`InferencePool`, but:

- The host set is static (a, b, c, d). There's no runtime endpoint variation
  for EPP to pick across.
- Per-host TLS still needs cluster-level `transport_socket_matches`; EPP
  wouldn't replace that.
- If we ever want runtime host churn, the right tool is an xDS control
  plane pushing `Cluster` updates, not EPP.

P4 was dropped. The integration is complete at P3.

## Refresh loop note

This integration probably does *not* need a control-plane refresh loop. The
host set is static (a, b, c, d) and routing is per-request via body. There is
no equivalent of `cluster-router-eg`'s "add `gpt-slow` at runtime" because
there is no model registry — the body says `"target":"c"`, the cluster
already has host c. If we later want to demo runtime host churn (add an
upstream-e while the gateway is live), we add a control-plane mode to the
demo binary and use the same `Cluster.Init`-time refresh loop pattern
`cluster-router-eg` documents. Not in scope for P2/P3.

## Phase summary

- **P1** — this doc + scaffold (`README.md`, `Makefile`, Dockerfiles).
- **P2** — k8s manifests, Go demo binary, plaintext body-driven routing e2e
  (`target=a/b`).
- **P3** — per-host TLS via `EnvoyPatchPolicy`: CA + leaf Secrets generated
  in-process by the e2e harness, `transport_socket_matches` keyed on sni
  metadata, `target=c/d` e2e assertions with the `GetCertificate` SNI
  tripwire on the upstream side.
