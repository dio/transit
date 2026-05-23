# MCP Profile Tiered Router E2E

Go `testify/suite` harness for the L1/L2 MCP profile topology. Gated by
environment variables; never runs automatically in a plain `go test ./...`.

## Running

### Install smoke test

Verifies Envoy Gateway installs cleanly with EnvoyPatchPolicy and Gateway
Namespace mode enabled. No custom images required.

```bash
RUN_MCP_PROFILE_TIERED_ROUTER_EG_INSTALL=1 \
  go test -v -count=1 -timeout=10m ./mcp-profile-tiered-router-eg/e2e/
```

Or via Makefile:

```bash
make -C integrations/mcp-profile-tiered-router-eg eg-install
```

### Full topology e2e

Requires a custom Envoy image with the Transit modules and a demo image with
the fake MCP backends.

```bash
IMAGE=<envoy-image-with-transit-modules> \
DEMO_IMAGE=<fake-mcp-backend-image> \
RUN_MCP_PROFILE_TIERED_ROUTER_EG_E2E=1 \
  go test -v -count=1 -timeout=25m ./mcp-profile-tiered-router-eg/e2e/
```

Or via Makefile:

```bash
IMAGE=... DEMO_IMAGE=... make -C integrations/mcp-profile-tiered-router-eg e2e
```

## Environment Variables

| Variable | Default | Description |
|---|---|---|
| `RUN_MCP_PROFILE_TIERED_ROUTER_EG_E2E` | — | Set to `1` to run the full e2e suite |
| `RUN_MCP_PROFILE_TIERED_ROUTER_EG_INSTALL` | — | Set to `1` to run the EG install smoke test |
| `IMAGE` | required | Custom Envoy image with `libmcp-profile-gateway.so`, `libmcp-catalog-router.so`, `libcluster-router.so` |
| `DEMO_IMAGE` | required | Demo image with `fake-mcp` binary |
| `ENVOY_GATEWAY_VERSION` | `v1.8.0` | Envoy Gateway Helm chart version |
| `K3S_TAG` | `v1.31.6-k3s1` | k3s node image tag |
| `K3D_AGENTS` | `0` | Number of k3d agent nodes |
| `K3D_SKIP_IMAGE_IMPORT` | — | Set to `1` to pull images from a registry instead of importing |
| `K3D_IMAGE_IMPORT_MODE` | — | k3d image import mode (e.g. `direct`) |
| `KEEP_CLUSTER` | — | Set to `1` to keep the k3d cluster after the suite finishes |
| `RESET_CLUSTER` | `1` | Set to `0` to reuse an existing cluster |

## Test Matrix Coverage

Cases implemented and active (run when images are present):

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

Cases skipped pending fake-mcp failure injection support:

- `initialize` partial backend failure (session contains only successful backends)
- `initialize` all-backend failure (gateway returns downstream error)
- `tools/list` with one failing backend returns healthy tools from others
- `tools/list` with all backends failing returns successful `{"tools":[]}`
- `tools/call` for a backend absent from partial-initialize session reaches no L2
- `tools/call` proxies backend 2xx JSON-RPC errors as-is
- `tools/call` converts backend transport or non-2xx failures to downstream errors

Cases skipped pending second profile config with restricted enabled_tools:

- `tools/list` preserves enabled-tool filtering
- `tools/call` disabled tool returns JSON-RPC error

See `../README.md` Test Matrix for the full required and nice-to-have cases and
the MCPProxy Parity section for the behavioral targets that inform the skipped cases.
