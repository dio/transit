# MCP Profile Tiered Router E2E

Go `testify/suite` harness for the L1/L2 MCP profile topology. Gated by
environment variables; never runs automatically in a plain `go test ./...`.

## Running

### Install smoke test

Verifies Envoy Gateway installs cleanly with EnvoyPatchPolicy and Gateway
Namespace mode enabled. No custom images required.

```bash
make -C integrations/mcp-profile-tiered-router-eg eg-install
```

### Full topology e2e

Builds a custom Envoy image with Transit modules and a demo image with the
catalog-router app and fake MCP backends by default:

```bash
make -C integrations/mcp-profile-tiered-router-eg e2e
```

Reuse prebuilt images:

```bash
SKIP_IMAGE_BUILD=1 \
IMAGE=<envoy-image> \
CONTROL_PLANE_IMAGE=<demo-image> \
make -C integrations/mcp-profile-tiered-router-eg e2e
```

Keep cluster alive for manual debugging after a run:

```bash
KEEP_CLUSTER=1 make -C integrations/mcp-profile-tiered-router-eg e2e
```

Reuse an existing cluster without resetting it:

```bash
RESET_CLUSTER=0 KEEP_CLUSTER=1 make -C integrations/mcp-profile-tiered-router-eg e2e
```

Run directly with `go test` (the Makefile sets `DEMO_IMAGE=$(CONTROL_PLANE_IMAGE)` for the test):

```bash
cd integrations && \
  RUN_MCP_PROFILE_TIERED_ROUTER_EG_E2E=1 \
  IMAGE=<envoy-image> \
  DEMO_IMAGE=<demo-image> \
  KEEP_CLUSTER=1 \
  GOWORK=off \
  go test -v -count=1 -timeout=25m ./mcp-profile-tiered-router-eg/e2e/
```

## Environment variables

| Variable | Default | Description |
|---|---|---|
| `RUN_MCP_PROFILE_TIERED_ROUTER_EG_E2E` | — | Set to `1` to run the full e2e suite |
| `RUN_MCP_PROFILE_TIERED_ROUTER_EG_INSTALL` | — | Set to `1` to run the EG install smoke test |
| `IMAGE` | required | Custom Envoy image with `libmcp-profile-gateway.so`, `libmcp-catalog-router.so`, `libcluster-router.so` |
| `DEMO_IMAGE` | required | Demo image with catalog-router app and fake-mcp binary |
| `ENVOY_GATEWAY_VERSION` | `v1.8.0` | Envoy Gateway Helm chart version |
| `K3S_TAG` | `v1.31.6-k3s1` | k3s node image tag |
| `K3D_AGENTS` | `0` | Number of k3d agent nodes |
| `K3D_SKIP_IMAGE_IMPORT` | `0` | Set to `1` to pull images from a registry instead of importing |
| `K3D_IMAGE_IMPORT_MODE` | `direct` | k3d image import mode |
| `KEEP_CLUSTER` | `0` | Set to `1` to keep the k3d cluster after the suite finishes |
| `RESET_CLUSTER` | `1` | Set to `0` to reuse an existing cluster |

## Topology wired by the suite

The test brings up three Gateways in `transit-dataplane`:

```text
l1    EnvoyProxy/l1    mcp-profile-gateway .so filter + l1-placeholder-backend Service
l2-a  EnvoyProxy/l2-a  catalog-router demo app (kiwi, aws-knowledge) + cluster-router .so
l2-b  EnvoyProxy/l2-b  catalog-router demo app (microsoft, github)   + cluster-router .so
```

Setup phases:

1. Apply namespaces, EnvoyProxy specs, demo workloads, Gateways, HTTPRoutes.
2. Discover the EG-generated init cluster name on each L2 shard from
   `/config_dump` and apply `epp-l2` — this patches the dedicated
   `l2-{a,b}-cluster-router-init` cluster with the cluster-router extension so
   the `/__cluster-router/config` dump endpoint serves the active shard config.
   Real catalog traffic flows through the demo catalog-router app unaffected.
3. Discover the L1 callout cluster names for `l1-l2a-catalog` and
   `l1-l2b-catalog` HTTPRoutes, build the real `MCP_PROFILE_GATEWAY_CONFIG`
   JSON with those cluster names, re-apply EnvoyProxy specs, and wait for
   rollout.
4. Apply `epp-l1` to insert the `mcp-profile-gateway` filter into the L1
   listener, and wait for `Programmed=True`.
5. Open port-forwards and run the test matrix.

## Test matrix

Active (run when images are present):

- L1 profile auth failure reaches no L2
- `initialize` fans out to all 4 backends, returns composite session ID
- `tools/list` merges all 4 servers' tools into one namespaced list
- Public `/mcp/s/aws-knowledge` forwarded to L2-A
- Public `/mcp/s/github` forwarded to L2-B
- `tools/call github.search` routed to L2-B github backend only
- `tools/call kiwi.search-flight` routed to L2-A kiwi backend only
- `tools/call` for unknown tool prefix returns JSON-RPC error code -32602
- L2-A direct `/mcp/s/github` rejected (cross-shard)
- L2-B direct `/mcp/s/kiwi` rejected (cross-shard)
- cluster-router dump shows `route_header: x-mcp-server`
- L1 `/dump` and L2 cluster-router dumps do not expose API keys or bearer tokens

Skipped (pending fake-mcp failure injection):

- `initialize` partial backend failure
- `initialize` all-backend failure
- `tools/list` with one failing backend returns healthy tools from others
- `tools/list` with all backends failing returns `{"tools":[]}`
- `tools/call` for backend absent from partial-initialize session reaches no L2
- `tools/call` proxies backend 2xx JSON-RPC errors
- `tools/call` converts backend transport failure to downstream error

Skipped (pending second profile config with restricted `enabled_tools`):

- `tools/list` preserves enabled-tool filtering
- `tools/call` disabled tool returns JSON-RPC error
