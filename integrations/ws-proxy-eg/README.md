# WS-Proxy Envoy Gateway Integration

This integration runs the `integrations/ws-proxy-eg/echo` dynamic module behind
Envoy Gateway in a local k3d cluster. It is the Kubernetes demo for embedded
WebSocket proxying: Envoy routes WS traffic to a Go echo server that lives
inside the Envoy pod itself, with no external upstream.

The point of the demo is structural: a Transit dynamic module registers an
embedded HTTP/WS server via `RegisterWithGroup`. Gateway API provides the
listener and route shape; EnvoyPatchPolicy rewires the generated xDS so that
all `/v1/responses` traffic is served by the embedded server, not a Kubernetes
workload.

## Scenario

All three gates must pass:

- **Gate 1**: `upgrade_configs: websocket` is injected into the Envoy listener
  via EPP. Without it, Envoy returns 426 Upgrade Required on any WS dial.
- **Gate 2**: WS frames pass through the EPP-replaced STATIC loopback cluster
  intact and in order. The test sends 10 frames of varying sizes and checks the
  echo for each.
- **Gate 3**: A plain HTTP GET to `/v1/responses` returns 400. The embedded
  server calls `websocket.Accept` before doing anything else, so plain HTTP is
  rejected before any handler logic runs.

## Architecture

### Runtime topology

```mermaid
flowchart TD
    TH(["Test Harness\ngo test ./e2e/..."])

    subgraph k3d["k3d cluster · transit-ws-proxy-eg"]

        subgraph egs["envoy-gateway-system"]
            EG["Envoy Gateway v1.8.0"]

            subgraph pod["Generated Envoy Pod  (transit-ws-proxy-eg image)"]
                direction TB
                LIS["Listener tcp-80\n① DynamicModuleFilter ws-proxy\n② upgrade_configs: websocket"]
                RC["RouteConfig http-80\n/v1/responses\n→ httproute/default/ws-proxy/rule/0\ntimeout 0s · upgrade_configs: websocket"]
                CL["Cluster  httproute/default/ws-proxy/rule/0\ntype: STATIC · 127.0.0.1:10001"]
                ECHO["Embedded Echo Server\n127.0.0.1:10001\n(RegisterWithGroup · libws-proxy.so)"]
                LIS --> RC --> CL --> ECHO
            end
        end

        subgraph def["default namespace"]
            EPP["EnvoyPatchPolicy ws-proxy\nProgrammed=True · 4 JSON patches"]
            HR["HTTPRoute ws-proxy\n/v1/responses → ws-proxy-backend:80"]
            SVC["Service ws-proxy-backend"]
            PD["Deployment ws-proxy-backend\ntransit-ws-proxy-eg-pause image\n(select{} · structural placeholder)\ngives EG a ready endpoint so it\ngenerates the CDS cluster"]
        end

        EG -- "xDS + EPP applied" --> pod
        EPP -- "4 patches" --> EG
        HR --> EG
        PD -- "ready endpoint\n→ EG creates cluster" --> SVC --> EG
    end

    TH -- "port-forward :19000\nport-forward svc :80" --> pod
    TH -. "Gate 1  config_dump" .-> pod
    TH -. "Gate 2  WS dial" .-> LIS
    TH -. "Gate 3  HTTP GET" .-> LIS
```

### Test harness lifecycle

```mermaid
sequenceDiagram
    participant H  as Test Harness
    participant K  as k3d
    participant EG as Envoy Gateway
    participant NS as k8s / default ns
    participant EN as Envoy Pod

    rect rgb(254,243,199)
        Note over H,K: Phase 1 · Cluster bootstrap
        H->>K: cluster create transit-ws-proxy-eg
        H->>EG: helm install v1.8.0
        H->>EG: apply envoy-gateway-config.yaml<br/>(enableEnvoyPatchPolicy + XDSNameSchemeV2)
        H->>K: k3d image import transit-ws-proxy-eg:TAG
    end

    rect rgb(219,234,254)
        Note over H,NS: Phase 2 · Resource apply
        H->>NS: apply EnvoyProxy (custom image + ENVOY_DYNAMIC_MODULES_SEARCH_PATH)
        H->>NS: apply GatewayClass + Gateway
        H->>NS: apply HTTPRoute + Service + pause:3.9 Deployment
        NS-->>EN: schedule pause pod (ready < 1 s)
        Note over EN: pause pod → EG detects ready endpoint<br/>→ generates CDS cluster<br/>httproute/default/ws-proxy/rule/0
        H->>H: waitDeployment default/ws-proxy-backend
        H->>H: waitDeployment envoy-gateway-system/envoy-default-*
    end

    rect rgb(209,250,229)
        Note over H,EN: Phase 3 · EPP patches
        H->>NS: apply EnvoyPatchPolicy ws-proxy
        EG->>EN: Patch 1 · add DynamicModuleFilter → tcp-80 http_filters/0
        EG->>EN: Patch 2 · add upgrade_configs: websocket → tcp-80
        EG->>EN: Patch 3 · replace route in http-80 (cluster + timeout 0s + upgrade_configs)
        EG->>EN: Patch 4 · replace cluster → STATIC 127.0.0.1:10001
        EG-->>H: EPP Programmed=True
        H->>EN: poll /config_dump until DynamicModuleFilter + 127.0.0.1:10001 present
    end

    rect rgb(237,233,254)
        Note over H,EN: Phase 4 · Gate assertions
        H->>EN: Gate 1 · assert upgrade_type: websocket in config_dump
        EN-->>H: ✓ PASS
        H->>EN: Gate 3 · HTTP GET /v1/responses  Host: ws-proxy.example.com
        EN-->>H: ✓ 400
        H->>EN: Gate 2 · WS dial  DialOptions.Host: ws-proxy.example.com
        EN-->>H: 101 Switching Protocols
        loop 10 frames
            H->>EN: {"type":"ping","seq":i,"pad":"x"×i×100}
            EN-->>H: exact echo
        end
        H-->>H: ✓ all three gates PASS
    end
```

## Components

The integration creates:

- Envoy Gateway, installed by Helm.
- `EnvoyProxy`, pointing Envoy Gateway at the custom Envoy image.
- `GatewayClass`, `Gateway`, and `HTTPRoute`.
- `EnvoyPatchPolicy`, with four JSON patches (see `k8s/epp.tmpl.yaml`).
- `Service ws-proxy-backend` and the vendored pause Deployment.

The custom Envoy image (`Dockerfile.envoy`) contains:

- `/usr/local/bin/envoy`
- `/etc/envoy/dynamic-modules/libws-proxy.so` (built from `echo/cmd`)
- Debian bookworm slim as the glibc userspace

The pause image (`Dockerfile.pause`) contains:

- A single static binary built from `pause/main.go`: `func main() { select {} }`
- `FROM scratch` — nothing else in the image

## Why the pause placeholder Deployment

Envoy Gateway v1.8 does not generate a CDS cluster for an HTTPRoute backend
when there are no ready endpoints. An EPP `replace` patch on a non-existent
cluster fails with `ResourceNotFound` and `Programmed=False`.

The `ws-proxy-backend` Deployment runs the vendored pause image
(`transit-ws-proxy-eg-pause`). It is a static binary built from four lines of
Go (`func main() { select {} }`) in a `FROM scratch` image. It starts in under
a second, has no readiness probe (ready immediately), and its only role is to
give EG a ready endpoint so it generates
`httproute/default/ws-proxy/rule/0`. The EPP then replaces the entire cluster
object with a STATIC entry pointing at `127.0.0.1:10001`. No real traffic ever
reaches the pause container.

Vendoring the image rather than pulling `registry.k8s.io/pause:3.9` makes the
intent explicit in the repository: `Dockerfile.pause` and `pause/main.go` are
honest about what the placeholder is and why it exists.

The e2e waits for the placeholder Deployment before applying the EPP to
eliminate the race where EG has not yet created the cluster.

## Why EnvoyPatchPolicy

Envoy Gateway owns the generated xDS. This demo keeps Gateway API as the source
of truth and uses EnvoyPatchPolicy to:

1. Add the `DynamicModuleFilter` to the generated listener, which triggers
   `RegisterWithGroup` and starts the embedded echo server on `127.0.0.1:10001`.
2. Add `upgrade_configs: websocket` to the listener so Envoy accepts WS upgrades.
3. Replace the generated route with one that has `timeout: 0s` and
   `upgrade_configs: websocket`.
4. Replace the generated backend cluster with a STATIC cluster pointing at
   the embedded server.

The important generated names for Envoy Gateway v1.8 with `XDSNameSchemeV2`:

- backend cluster: `httproute/default/ws-proxy/rule/0`
- listener: `tcp-80`
- route configuration: `http-80`

EnvoyPatchPolicy status stores conditions under
`status.ancestors[*].conditions`, so the e2e polls those directly rather than
using `kubectl wait envoypatchpolicy --for=condition=Programmed`.

## Dynamic module loading

The Envoy image carries the `.so` at:

```
/etc/envoy/dynamic-modules/libws-proxy.so
```

`EnvoyProxy` configures:

- the custom Envoy image
- `GODEBUG=cgocheck=0`
- `ENVOY_DYNAMIC_MODULES_SEARCH_PATH=/etc/envoy/dynamic-modules`

The `EnvoyProxy` spec also declares the module under `dynamicModules` so Envoy
Gateway registers it globally before the first listener is created.

## Running

```
# full e2e (builds both images, creates cluster, runs gates, deletes cluster)
make -C integrations/ws-proxy-eg e2e

# reuse prebuilt images
make -C integrations/ws-proxy-eg e2e SKIP_IMAGE_BUILD=1 \
  IMAGE=transit-ws-proxy-eg:<tag> \
  PAUSE_IMAGE=transit-ws-proxy-eg-pause:<tag>

# install only (keeps cluster for manual inspection)
make -C integrations/ws-proxy-eg eg-install KEEP_CLUSTER=1

# publish short-lived ttl.sh images for CI
make -C integrations/ws-proxy-eg publish
# then in CI: K3D_SKIP_IMAGE_IMPORT=1 pulls from registry instead of importing
```
