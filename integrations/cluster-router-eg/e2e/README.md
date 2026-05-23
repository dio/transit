# Cluster Router Envoy Gateway E2E

Go `testify/suite` harness for the cluster-router integration. Gated by
environment variables; never runs automatically in a plain `go test ./...`.

## Running

### Install smoke test

Verifies Envoy Gateway installs cleanly with EnvoyPatchPolicy enabled. No
custom images required.

```bash
make -C integrations/cluster-router-eg eg-install
```

### Full topology e2e

Builds a custom Envoy image and control-plane image by default:

```bash
make -C integrations/cluster-router-eg e2e
```

Reuse prebuilt images:

```bash
SKIP_IMAGE_BUILD=1 \
IMAGE=<envoy-image> \
CONTROL_PLANE_IMAGE=<control-plane-image> \
make -C integrations/cluster-router-eg e2e
```

Keep cluster alive for manual inspection after a run:

```bash
KEEP_CLUSTER=1 make -C integrations/cluster-router-eg e2e
```

Reuse an existing cluster without resetting it:

```bash
RESET_CLUSTER=0 KEEP_CLUSTER=1 make -C integrations/cluster-router-eg e2e
```

Run directly with `go test`:

```bash
cd integrations/cluster-router-eg && \
  RUN_CLUSTER_ROUTER_EG_E2E=1 \
  IMAGE=<envoy-image> \
  CONTROL_PLANE_IMAGE=<control-plane-image> \
  KEEP_CLUSTER=1 \
  GOWORK=off \
  go test -v -count=1 -timeout=15m ./e2e/
```

## Environment variables

| Variable | Default | Description |
|---|---|---|
| `RUN_CLUSTER_ROUTER_EG_E2E` | — | Set to `1` to run the full e2e suite |
| `RUN_CLUSTER_ROUTER_EG_INSTALL` | — | Set to `1` to run the EG install smoke test |
| `IMAGE` | required | Custom Envoy image with `libcluster-router.so` |
| `CONTROL_PLANE_IMAGE` | required | Control-plane image (upstream servers + demo control) |
| `ENVOY_GATEWAY_VERSION` | `v1.8.0` | Envoy Gateway Helm chart version |
| `K3S_TAG` | `v1.31.6-k3s1` | k3s node image tag |
| `K3D_AGENTS` | `0` | Number of k3d agent nodes |
| `K3D_SKIP_IMAGE_IMPORT` | `0` | Set to `1` to pull images from a registry |
| `K3D_IMAGE_IMPORT_MODE` | `direct` | k3d image import mode |
| `KEEP_CLUSTER` | `0` | Set to `1` to keep the k3d cluster after the suite |
| `RESET_CLUSTER` | `1` | Set to `0` to reuse an existing cluster |

## What the suite proves

- Envoy Gateway installs with EnvoyPatchPolicy.
- `EnvoyPatchPolicy` patches the EG-generated backend cluster with the
  cluster-router extension.
- `x-model: gpt-fast` reaches upstream A with OpenAI auth.
- `x-model: claude-safe` reaches upstream B with Anthropic auth.
- Control plane updates routing config at runtime (adds `gpt-slow`,
  `kimi-fast`).
- Updated model routes work without changing Gateway API or restarting Envoy.
- Redacted control-plane dump does not expose bearer tokens.

## Cluster name discovery

The suite reads the EG-generated backend cluster name from the Envoy admin
`/config_dump` endpoint and passes it into `epp.tmpl.yaml` as `{{.ClusterName}}`.
With `XDSNameSchemeV2` the name follows the pattern:

```text
httproute/<namespace>/<httproute>/rule/<n>
```

The listener patch always targets the fixed name `tcp-80`.
