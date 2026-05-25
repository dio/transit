# Tiered WS-Proxy Envoy Gateway Integration

Two-stage WebSocket pipeline under Envoy Gateway: L1 shard-routes the WS
upgrade to the right L2 pod; L2 runs the embedded ws-proxy server and egresses
to a mock upstream via a second Envoy-managed listener.

Both tiers are Envoy Gateway managed — two GatewayClasses, two EnvoyProxy
objects, two generated Envoy Deployments in the same k3d cluster.

The point of the demo is the full ownership chain: **Envoy holds credentials
and TLS at every hop; Go holds session logic at L2**. In the non-WS path this
is free because Cluster Extension clusters give Envoy egress ownership by
default. For WS, after `101 Switching Protocols` Envoy becomes a transparent
TCP tunnel and the Cluster Extension cannot touch frames. The embedded server +
L2 egress listener restores the same property for WebSocket traffic.

## Scenario

Three structural gates:

- **Gate 1**: WS upgrade from client reaches L1, L1 selects L2, upgrade
  propagates to L2 embedded server — `101` at both hops, no `426`.
- **Gate 2**: Frames echo end-to-end intact (client → L1 → L2 embedded server
  → L2 egress listener → mock upstream → back). 10 frames of varying size.
- **Gate 3**: L2 session record written after close — proves `SessionTap` fired
  on real frames, extracting model and token counts from the mock's
  `response.completed`.

## Architecture

### Runtime topology

```mermaid
flowchart TD
    TH(["Test Harness<br/>go test ./e2e/..."])

    subgraph k3d["k3d cluster · transit-tiered-ws-proxy-eg"]

        subgraph egs["envoy-gateway-system"]
            EG["Envoy Gateway v1.8.0"]

            subgraph l1pod["L1 Envoy Pod  (cluster-shard-router image)"]
                direction TB
                L1LIS["Listener tcp-80<br/>upgrade_configs: websocket<br/>cluster-shard-router LB policy"]
                L1LIS --> L1PICK["ChooseHost → l2.default.svc:80<br/>(at upgrade time, before tunnel)"]
            end

            subgraph l2pod["L2 Envoy Pod  (ws-proxy image)"]
                direction TB
                L2IN["Listener tcp-80  (inbound)<br/>① DynamicModuleFilter ws-proxy<br/>② upgrade_configs: websocket<br/>→ STATIC 127.0.0.1:10001"]
                EMB["Embedded WS Server :10001<br/>(RegisterWithGroup · libws-proxy.so)<br/>SessionTap · WSPROXY_EGRESS_URL=ws://127.0.0.1:10002"]
                L2EG["Listener tcp-10002  (egress)<br/>upgrade_configs: websocket<br/>ws-auth upstream filter<br/>→ mock-upstream cluster"]
                L2IN --> EMB --> L2EG
            end
        end

        subgraph def["default namespace"]
            L1EPP["EnvoyPatchPolicy l1<br/>upgrade_configs + LB policy patch"]
            L2EPP["EnvoyPatchPolicy l2<br/>DynamicModuleFilter + STATIC + egress upgrade_configs + ws-auth"]
            GWL1["Gateway l1  · listener :80"]
            GWL2["Gateway l2  · listeners :80 + :10002"]
            HRL1["HTTPRoute l1<br/>/v1/responses → Service/l2"]
            HRL2IN["HTTPRoute l2-inbound<br/>/v1/responses → pause placeholder"]
            HRL2EG["HTTPRoute l2-egress<br/>/v1/responses → Service/mock-upstream"]
            SVC_L2["Service l2<br/>→ L2 Envoy pod :80"]
            SVC_MOCK["Service mock-upstream<br/>→ mock WS echo pod :8080"]
            MOCK["Deployment mock-upstream<br/>WS echo server<br/>emits response.completed {usage}"]
        end

        EG -- "xDS + EPP applied" --> l1pod
        EG -- "xDS + EPP applied" --> l2pod
        L1EPP -- "2 patches" --> EG
        L2EPP -- "6 patches" --> EG
        HRL1 --> EG
        HRL2IN --> EG
        HRL2EG --> EG
        SVC_L2 --> l2pod
        SVC_MOCK --> MOCK
    end

    TH -- "port-forward svc/l1 :80" --> l1pod
    TH -. "Gate 1  WS dial → 101" .-> L1LIS
    TH -. "Gate 2  10 frames echo" .-> L1LIS
    TH -- "port-forward L2 admin :19000" --> l2pod
    TH -. "Gate 3  session record" .-> EMB
```

### Test harness lifecycle

```mermaid
sequenceDiagram
    participant H  as Test Harness
    participant K  as k3d
    participant EG as Envoy Gateway
    participant NS as k8s / default ns
    participant L1 as L1 Envoy Pod
    participant L2 as L2 Envoy Pod
    participant MK as mock-upstream pod

    rect rgb(254,243,199)
        Note over H,K: Phase 1 · Cluster bootstrap
        H->>K: cluster create transit-tiered-ws-proxy-eg
        H->>EG: helm install v1.8.0
        H->>EG: apply envoy-gateway-config.yaml<br/>(enableEnvoyPatchPolicy + XDSNameSchemeV2)
        H->>K: k3d image import l1-image + l2-image
    end

    rect rgb(219,234,254)
        Note over H,NS: Phase 2 · Resource apply
        H->>NS: apply EnvoyProxy/l1 (cluster-shard-router image)
        H->>NS: apply EnvoyProxy/l2 (ws-proxy image + WSPROXY_EGRESS_URL)
        H->>NS: apply GatewayClass/l1 + Gateway/l1
        H->>NS: apply GatewayClass/l2 + Gateway/l2 (listeners :80 + :10002)
        H->>NS: apply HTTPRoute/l1 → Service/l2
        H->>NS: apply HTTPRoute/l2-inbound + pause placeholder
        H->>NS: apply HTTPRoute/l2-egress + Service/mock-upstream + Deployment/mock-upstream
        NS-->>L1: schedule L1 pod
        NS-->>L2: schedule L2 pod
        NS-->>MK: schedule mock-upstream pod
        H->>H: waitDeployment default/mock-upstream
        H->>H: waitDeployment default/l2-pause-backend
        H->>H: waitDeployment envoy-gateway-system/l1-envoy
        H->>H: waitDeployment envoy-gateway-system/l2-envoy
    end

    rect rgb(209,250,229)
        Note over H,L2: Phase 3 · EPP patches
        H->>NS: apply EnvoyPatchPolicy/l1
        EG->>L1: Patch 1 · upgrade_configs: websocket → tcp-80
        EG->>L1: Patch 2 · cluster-shard-router LB policy → generated L2 cluster
        H->>NS: apply EnvoyPatchPolicy/l2
        EG->>L2: Patch 1 · DynamicModuleFilter ws-proxy → tcp-80 http_filters/0
        EG->>L2: Patch 2 · upgrade_configs: websocket → tcp-80
        EG->>L2: Patch 3 · replace route in http-80 → STATIC cluster + timeout 0s + upgrade_configs
        EG->>L2: Patch 4 · replace inbound cluster → STATIC 127.0.0.1:10001
        EG->>L2: Patch 5 · upgrade_configs: websocket → tcp-10002
        EG->>L2: Patch 6 · ws-auth upstream filter → egress cluster (credential injection)
        EG-->>H: both EPPs Programmed=True
        H->>L2: poll /config_dump until DynamicModuleFilter + :10001 + :10002 present
    end

    rect rgb(237,233,254)
        Note over H,MK: Phase 4 · Gate assertions
        H->>L1: Gate 1 · WS dial /v1/responses  Host: tiered-ws-proxy.example.com
        L1-->>L2: WS upgrade tunnelled (ChooseHost selected L2)
        L2-->>MK: WS upgrade via egress listener :10002
        MK-->>H: 101 Switching Protocols (end-to-end)
        loop 10 frames
            H->>L1: {"type":"ping","seq":i,"pad":"x"×i×100}
            L1-->>L2: forward (TCP tunnel)
            L2-->>MK: forward (via egress)
            MK-->>L2: {"type":"response.completed","response":{"usage":{...}}}
            L2-->>H: echoed frame
        end
        H-->>H: ✓ Gate 1 PASS · ✓ Gate 2 PASS
        H->>L2: Gate 3 · read session log (WSPROXY_SESSION_LOG)
        L2-->>H: {model, input_tokens, output_tokens, duration_ms}
        H-->>H: ✓ Gate 3 PASS
    end
```

## Components

### L1 Envoy image (`Dockerfile.l1`)

- `/usr/local/bin/envoy`
- `/etc/envoy/dynamic-modules/libcluster-shard-router.so`

The cluster-shard-router module uses `RegisterLBPolicy` (CLUSTER_PROVIDED load
balancing). At WS upgrade time `ChooseHost` selects the L2 pod. After `101`,
L1 is a transparent TCP tunnel — no frames inspected at L1.

### L2 Envoy image (`Dockerfile.l2`)

- `/usr/local/bin/envoy`
- `/etc/envoy/dynamic-modules/libws-proxy.so`

The ws-proxy module uses `RegisterWithGroup`. The embedded server on
`:10001` intercepts WS frames, runs `SessionTap`, and dials
`WSPROXY_EGRESS_URL=ws://127.0.0.1:10002` for each session. The egress
listener on `:10002` is the second Gateway listener, EG-generated and
EPP-patched to add `upgrade_configs: websocket` and the `ws-auth` upstream
filter.

### Mock upstream (`Dockerfile.mock`)

A minimal Go WS echo server built `CGO_ENABLED=0` and deployed as a pod. It:

- accepts any WS upgrade on `/v1/responses`
- echoes every frame back unchanged — except `response.create`, which it
  answers with a synthetic `response.completed` containing fixed `usage` fields
  so Gate 3 can assert exact token counts

### Two-listener L2 Gateway

`Gateway/l2` has two listeners:

```yaml
listeners:
- name: inbound
  port: 80
  protocol: HTTP
- name: egress
  port: 10002
  protocol: HTTP
```

EG generates `tcp-80` (inbound) and `tcp-10002` (egress) from a single
EnvoyProxy/l2 deployment. EPP patches both. This avoids needing EPP to create a
net-new listener resource — EPP can only patch named xDS resources EG has
already generated; it cannot add new top-level resources.

## EPP design

### L1 EPP (2 patches)

| # | Resource | Op | What |
|---|---|---|---|
| 1 | `Listener/tcp-80` | add | `upgrade_configs: websocket` |
| 2 | `Cluster/<httproute-l1/.../rule/0>` | replace | cluster type → CLUSTER_PROVIDED with cluster-shard-router extension |

### L2 EPP (6 patches)

| # | Resource | Op | What |
|---|---|---|---|
| 1 | `Listener/tcp-80` | add | `DynamicModuleFilter ws-proxy` at `http_filters/0` |
| 2 | `Listener/tcp-80` | add | `upgrade_configs: websocket` |
| 3 | `RouteConfig/http-80` | replace | route → STATIC cluster, `timeout: 0s`, `upgrade_configs: websocket` |
| 4 | `Cluster/<httproute-l2-inbound/.../rule/0>` | replace | STATIC `127.0.0.1:10001` |
| 5 | `Listener/tcp-10002` | add | `upgrade_configs: websocket` |
| 6 | `Cluster/<httproute-l2-egress/.../rule/0>` | add | `upstream_http_filters` with `ws-auth` credential filter |

## Why the embedded server exists

After `101 Switching Protocols` Envoy handles WS frame forwarding as a
transparent TCP tunnel. The dynamic module ABI has no per-frame callback. The
embedded server is the only way to intercept frames from a dynamic module
without modifying Envoy.

## Why egress via Envoy at L2

In the non-WS path (cluster-router, cluster-shard-router) egress is via Envoy
for free: the Cluster Extension gives `ChooseHost` to Go but Envoy owns the TCP
connection, TLS origination, and upstream HTTP filters (auth injection). For WS
the tunnel breaks that — Go (the embedded server) owns the upstream dial. The
L2 egress listener restores Envoy ownership: the embedded server dials a plain
loopback connection; Envoy's egress listener handles TLS and credential injection
via `ws-auth` upstream filter. No credentials in Go code; no TLS in Go code.

## Running

```sh
# Full e2e (builds both images, creates cluster, runs gates, deletes cluster)
make -C integrations/tiered-ws-proxy-eg e2e

# Reuse prebuilt images
make -C integrations/tiered-ws-proxy-eg e2e SKIP_IMAGE_BUILD=1 \
  L1_IMAGE=transit-tiered-ws-proxy-l1:<tag> \
  L2_IMAGE=transit-tiered-ws-proxy-l2:<tag> \
  MOCK_IMAGE=transit-tiered-ws-proxy-mock:<tag>

# Install only (keeps cluster for manual inspection)
make -C integrations/tiered-ws-proxy-eg eg-install KEEP_CLUSTER=1

# Publish short-lived ttl.sh images for CI
make -C integrations/tiered-ws-proxy-eg publish
```
