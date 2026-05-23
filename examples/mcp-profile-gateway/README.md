# MCP Profile Gateway

`mcp-profile-gateway` is the L1 public gateway for MCP profile routing.

```text
POST /mcp/s/{server-slug}
  -> owning L2 /mcp/s/{server-slug}

POST /mcp/{profile-id} method=initialize
  -> fan out initialize to all profile member L2 endpoints
  -> encode per-backend session IDs into one opaque L1 mcp-session-id

POST /mcp/{profile-id} method=tools/list
  -> fan out to profile member L2 catalog endpoints
  -> merge enabled tools as {prefix}.{tool}

POST /mcp/{profile-id} method=tools/call
  -> decode L1 session, resolve {prefix}.{tool} to owning server
  -> forward to single L2 with backend session ID and credential headers
```

This example is separate from `examples/mcp-catalog-router`, which is the L2
execution front end.

## Session Envelope

Profile `initialize` fans out to all member servers and encodes the successful
per-backend session IDs into one opaque `mcp-session-id` returned to the
client. The current format is a prefixed base64url JSON value:

```text
mcp-profile-gateway.<base64url({"profile_id":"...","backends":{"server-id":"backend-session","...":"..."}})>
```

This encoding is intentionally readable for the example.

**Production requirements:** a production envelope must be authenticated and
encrypted (for example AEAD or a signed JWT) and must bind the profile ID,
each backend server ID, audience, subject, and expiry. It must never expose
raw backend session IDs as client-visible plaintext. A forged or replayed
envelope allows a client to impersonate any backend session. Short expiry and
key rotation are required.

`tools/call` requires the L1 session and rejects calls for backends that were
absent from the `initialize` fan-out result (i.e. backends that failed
initialization are not callable even if their prefix resolves correctly).

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

## Debug Endpoints

`GET /healthz` — returns 204 when the module is alive.

`GET /dump` — returns a redacted JSON snapshot of gateway state. Raw
credential values, API keys, and session IDs are never included. Example:

```json
{
  "catalog_servers": {
    "aws-knowledge": {"target": "http://l2-a.local", "last_request": "ok"},
    "github":        {"target": "http://l2-b.local", "last_request": "initialized"}
  },
  "profiles": {
    "9b3f7d0a80c4aa6d": {
      "name": "kiwi",
      "auth_configured": true,
      "servers": {
        "aws-knowledge": {
          "prefix": "aws",
          "enabled_tools_count": 3,
          "credential_ref_configured": true
        },
        "github": {
          "prefix": "github",
          "credential_envelope_configured": true
        }
      }
    }
  }
}
```

`enabled_tools_count` is present only when `enabled_tools` is explicitly
configured; its absence means all tools from that backend are enabled.
