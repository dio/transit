# MCP Profile Router

This example proves MCP profile routing as a Transit dynamic module before we
put the same idea behind the tiered Envoy Gateway demo.

It is intentionally local. There is no k3d in this first cut. The profile
endpoint is served by `libmcp-profile-router.so` inside Envoy, and backend
egress also goes back through Envoy routes. The goal is to make the MCP
semantics obvious:

- one profile URL represents several backend MCP servers
- the client authenticates once to the profile
- the proxy opens an MCP session with each backend during `initialize`
- later requests carry one composite `mcp-session-id`
- the proxy forwards the right per-backend `mcp-session-id`
- `tools/list` fans out and merges tools
- `tools/call github.search` routes to GitHub only
- backend credentials are delivered per server and redacted from dumps

## Why Sessions Matter

Envoy AI Gateway's MCP proxy keeps a composite client session. The client sees
one `mcp-session-id`, but the proxy tracks the backend session IDs inside it.
That lets a single profile endpoint aggregate several stateful MCP servers.

This example adopts the same shape in a small form:

```text
client
  |
  | initialize
  v
Envoy + libmcp-profile-router.so
  |
  +--> initialize github -> github session
  +--> initialize kiwi -> kiwi session
  |
  | returns one composite mcp-session-id
  v
client
```

Later `tools/list` and `tools/call` requests must include that composite
session ID. The dynamic module decodes it and forwards each backend's own
session ID.

The example does not encrypt the composite session ID yet. The integration plan
should add encrypted or signed session state before this is production-shaped.

## Run

```sh
make -C examples/mcp-profile-router build
```

That builds both:

- `dist/mcp-profile-router`, the demo CLI
- `libmcp-profile-router.so`, the Transit module loaded by Envoy

Start two local MCP backends:

```sh
./examples/mcp-profile-router/dist/mcp-profile-router backend \
  --addr :8081 \
  --id github \
  --tools search,repo_read \
  --expected-auth "Bearer github-token"

./examples/mcp-profile-router/dist/mcp-profile-router backend \
  --addr :8082 \
  --id kiwi \
  --tools search_flights \
  --expected-auth "Bearer kiwi-token"
```

For an all-in-one local CLI run without Envoy, start the profile aggregator:

```sh
./examples/mcp-profile-router/dist/mcp-profile-router aggregator \
  --addr :8080 \
  --profile engineering \
  --api-key profile-key \
  --server 'github=http://127.0.0.1:8081=github=Bearer github-token,kiwi=http://127.0.0.1:8082=kiwi=Bearer kiwi-token'
```

The tokens above are fake. For a later real GitHub MCP backend smoke test, use
`gh auth token` as the GitHub backend credential and keep the Kiwi credential
separate.

List tools:

```sh
./examples/mcp-profile-router/dist/mcp-profile-router tools-list \
  --profile-url http://127.0.0.1:8080/mcp/profiles/engineering \
  --api-key profile-key
```

Expected excerpt:

```json
{
  "jsonrpc": "2.0",
  "id": 2,
  "result": {
    "tools": [
      {
        "name": "github.repo_read"
      },
      {
        "name": "github.search"
      },
      {
        "name": "kiwi.search_flights"
      }
    ]
  }
}
```

Call one namespaced tool:

```sh
./examples/mcp-profile-router/dist/mcp-profile-router tools-call github.search \
  --profile-url http://127.0.0.1:8080/mcp/profiles/engineering \
  --api-key profile-key \
  --arguments '{"query":"transit"}'
```

Expected excerpt:

```json
{
  "result": {
    "structuredContent": {
      "server": "github",
      "tool": "search",
      "auth_ok": true
    }
  }
}
```

## Test

```sh
make -C examples/mcp-profile-router test
make -C examples/mcp-profile-router e2e
```

The e2e target builds `libmcp-profile-router.so` through the example Makefile,
then the Go test starts in-process GitHub and Kiwi MCP backends and Envoy. It
checks that `tools/list` merges both backends, that backend egress goes through
Envoy routes, and that `tools/call github.search` reaches only GitHub.
