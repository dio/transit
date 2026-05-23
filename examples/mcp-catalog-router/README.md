# MCP Catalog Router

`mcp-catalog-router` is the L2 catalog-server front end for MCP tiered routing.

It accepts cataloged server requests:

```text
POST /mcp/s/{server-slug}
```

and forwards them to a configured egress URL as:

```text
POST /mcp
x-mcp-server: {server-slug}
```

That egress URL can be backed by `examples/cluster-router`, which uses
`x-mcp-server` to select the concrete backend host and inject backend
credentials.

## Config

The dynamic module reads `MCP_CATALOG_ROUTER_CONFIG`:

```json
{
  "route_header": "x-mcp-server",
  "timeout_millis": 800,
  "servers": {
    "aws-knowledge": {
      "url": "http://l2-egress.local",
      "credential": "Bearer aws-token"
    },
    "github": {
      "url": "http://l2-egress.local",
      "credential": "Bearer github-token"
    }
  }
}
```

`route_header` defaults to `x-mcp-server`. The `credential` field is optional.
When present, it is applied as the backend `Authorization` header on the egress
request.

`url` is the base egress origin. The catalog router always sends backend MCP
requests to `url + "/mcp"`, so do not include `/mcp` or `/mcp/s/{server-slug}`
in the configured URL.

## Scope

This example is intentionally L2-only. It does not fetch profiles, enforce
profile auth, perform profile fan-out, or decide which L2 cluster owns a server.
Those are L1 `mcp-profile-gateway` responsibilities.

## Manual Real-Upstream Smoke

CI should use fake MCP backends, but the same config shape can point at a real
cataloged MCP upstream for manual testing. For example:

```sh
export MCP_CATALOG_ROUTER_CONFIG='{
  "route_header": "x-mcp-server",
  "servers": {
    "aws-knowledge": {
      "url": "https://proxy.staging.transit.so",
      "credential": "Bearer <token>"
    }
  }
}'
```

Use this only as a local/manual smoke. Do not add real hosted MCP servers or
public internet dependencies to CI e2e tests.
