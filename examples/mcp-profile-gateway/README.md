# MCP Profile Gateway

`mcp-profile-gateway` is the L1 public gateway for MCP profile routing.

This implementation supports public catalog forwarding, profile `initialize`
fan-out, and profile `tools/list` fan-out:

```text
POST /mcp/s/{server-slug}
  -> owning L2 /mcp/s/{server-slug}

POST /mcp/{profile-id} method=tools/list
  -> fan out to profile member L2 catalog endpoints
  -> merge enabled tools as {prefix}.{tool}
```

Profile `initialize` returns one L1 `mcp-session-id` that encodes the successful
per-L2 backend sessions. Later profile requests decode that envelope and forward
only the corresponding backend session ID to each L2. The example uses a plain
base64 JSON envelope for readability; production should authenticate and
encrypt this value.

Profile `tools/call` routing is planned next. This example is separate from
`examples/mcp-catalog-router`, which is the L2 execution front end.

## Config

The dynamic module reads `MCP_PROFILE_GATEWAY_CONFIG`:

```json
{
  "timeout_millis": 800,
  "catalog_servers": {
    "aws-knowledge": {
      "url": "http://l2-a.local",
      "cluster": "l2-a-catalog"
    },
    "github": {
      "url": "http://l2-b.local",
      "cluster": "l2-b-catalog"
    }
  },
  "profiles": {
    "9b3f7d0a80c4aa6d-67261ca9ea3dadb2": {
      "name": "kiwi",
      "api_key": "profile-key",
      "servers": {
        "aws-knowledge": {
          "url": "http://l2-a.local",
          "prefix": "aws-knowledge",
          "credential_ref": "profile/aws-knowledge/user-123"
        }
      }
    }
  }
}
```

`catalog_servers[*].url` is the owning L2 base URL used for the outbound
`:scheme`, `host`, and `:path` callout headers. `cluster` is the Envoy cluster
that should carry the callout. If omitted, it defaults to
`mcp-profile-gateway-l2`.

The gateway forwards public catalog requests to:

```text
{url}/mcp/s/{server-slug}
```

Do not put concrete MCP backend hosts in this config. Concrete host selection
belongs to L2 `mcp-catalog-router` plus `cluster-router`.

## Direct Catalog Credentials

Direct public `/mcp/s/{server-slug}` requests have no profile context in this
first slice, so the gateway does not forward profile-bound credential refs or
credential envelopes on that path.

Profile `/mcp/{profile-id}` requests forward configured `credential_ref` and
`credential_envelope` values to the owning L2 server request.

In the dynamic module path, L2 forwarding uses Transit `HTTPCallout` and
`HTTPCalloutAllSettled` so Envoy owns routing, DNS, TLS, retries, and telemetry. The
pure Go HTTP handler remains available for unit tests and local debugging.
