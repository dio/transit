# Tiered Router Envoy Gateway E2E

Go `testify/suite` harness for the tiered-router integration. Gated by
environment variables; never runs automatically in a plain `go test ./...`.

## Running

### Install smoke test

Verifies Envoy Gateway installs cleanly with EnvoyPatchPolicy and Gateway
Namespace mode enabled. No custom images required.

```bash
make -C integrations/tiered-router-eg eg-install
```

### Full topology e2e

Builds a custom Envoy image and control-plane image by default:

```bash
make -C integrations/tiered-router-eg e2e
```

Reuse prebuilt images:

```bash
SKIP_IMAGE_BUILD=1 \
IMAGE=<envoy-image> \
CONTROL_PLANE_IMAGE=<control-plane-image> \
make -C integrations/tiered-router-eg e2e
```

Keep cluster alive for manual inspection after a run:

```bash
KEEP_CLUSTER=1 make -C integrations/tiered-router-eg e2e
```

Reuse an existing cluster without resetting it:

```bash
RESET_CLUSTER=0 KEEP_CLUSTER=1 make -C integrations/tiered-router-eg e2e
```

Run directly with `go test`:

```bash
cd integrations/tiered-router-eg && \
  RUN_TIERED_ROUTER_EG_E2E=1 \
  IMAGE=<envoy-image> \
  CONTROL_PLANE_IMAGE=<control-plane-image> \
  KEEP_CLUSTER=1 \
  GOWORK=off \
  go test -v -count=1 -timeout=15m ./e2e/
```

## Environment variables

| Variable | Default | Description |
|---|---|---|
| `RUN_TIERED_ROUTER_EG_E2E` | — | Set to `1` to run the full e2e suite |
| `RUN_TIERED_ROUTER_EG_INSTALL` | — | Set to `1` to run the EG install smoke test |
| `IMAGE` | required | Custom Envoy image with `libcluster-shard-router.so`, `libcluster-router.so` |
| `CONTROL_PLANE_IMAGE` | required | Control-plane image (demo binary) |
| `ENVOY_GATEWAY_VERSION` | `v1.8.0` | Envoy Gateway Helm chart version |
| `K3S_TAG` | `v1.31.6-k3s1` | k3s node image tag |
| `K3D_AGENTS` | `0` | Number of k3d agent nodes |
| `K3D_SKIP_IMAGE_IMPORT` | `0` | Set to `1` to pull images from a registry |
| `K3D_IMAGE_IMPORT_MODE` | `direct` | k3d image import mode |
| `KEEP_CLUSTER` | `0` | Set to `1` to keep the k3d cluster after the suite |
| `RESET_CLUSTER` | `1` | Set to `0` to reuse an existing cluster |

## What the suite proves

- Three separate Gateways (l1, l2-a, l2-b) in Gateway Namespace mode.
- L1 `EnvoyPatchPolicy` inserts the `cluster-shard-router-debug` listener
  filter and patches the L1 generated cluster with the cluster-shard-router
  extension (inline shard table).
- L2 `EnvoyPatchPolicy` inserts the `cluster-router-debug` listener filter
  and patches each L2 generated cluster with the cluster-router extension
  (per-shard model routes).
- `x-transit-tag: a-demo` routes through L1 → L2-A → upstream A.
- `x-transit-tag: b-demo` routes through L1 → L2-B → upstream C.
- Shard identity, provider, profile, BYOK key ID, and auth reach the upstream.
- Redacted L2 cluster-router dump does not expose bearer tokens.

## Cluster name discovery

The suite discovers two EG-generated backend cluster names from Envoy admin
`/config_dump`:
- `l1-public` cluster: used by `epp-l1.tmpl.yaml` (`{{.ClusterName}}`)
- `l2-a` and `l2-b` clusters: used by `epp-l2.tmpl.yaml` (`{{.ClusterName}}`)

With `XDSNameSchemeV2` each name follows:

```text
httproute/<namespace>/<httproute>/rule/<n>
```

The listener patches target the fixed name `tcp-80`.
