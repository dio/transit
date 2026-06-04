# Orange MCP Agent Prompt

Prompt for an agent to implement Orange as an LLM + MCP proxy.

---

**Task:** Implement the first Orange MCP sidecar slice.

**Repo:** `/Users/dio/src/dio/transit2`.

**Goal:** Make `examples/orange` an LLM + MCP proxy. Add an Orange-managed MCP
sidecar under `examples/orange/internal/pipeline/mcp`, following the existing
`examples/orange/internal/pipeline/responsesws` pattern. The sidecar must shape
MCP streamable-HTTP/SSE protocol traffic, but all MCP-server egress must go
back through Envoy.

---

## Current State (as of 2026-06-04)

PRs 1–4 are implemented. The following files exist and have unit tests:

- `examples/orange/internal/pipeline/mcp/mcp.go` — filter registration, sidecar init
- `examples/orange/internal/pipeline/mcp/handlers.go` — POST/GET/DELETE handlers, fan-out
- `examples/orange/internal/pipeline/mcp/session.go` — encrypted session/event envelopes
- `examples/orange/internal/pipeline/mcp/crypto.go` — PBKDF2/AES-GCM
- `examples/orange/internal/pipeline/mcp/sse.go` — SSE parser and writer
- `examples/orange/internal/pipeline/mcp/selectors.go` — deny-wins tool selectors
- `examples/orange/internal/pipeline/mcp/egress.go` — egress filter (validate/strip)
- `examples/orange/internal/pipeline/mcp/sidecar.go` — embedded HTTP server lifecycle
- `examples/orange/internal/pipeline/mcp/jsonrpc.go` — JSON-RPC helpers
- `examples/orange/internal/pipeline/mcp/records.go` — bounded/redacted records
- `examples/orange/internal/config/config.go` — MCP config types (`MCPConfig`, `MCPProfile`, `MCPServer`, `MCPToolSelector`)
- `examples/orange/orange.yaml` — MCP config with profiles and servers
- `examples/orange/mcp-demo` — curl demo script (executable)

**The actual config schema uses `mcp.profiles` and `mcp.servers`** (not `mcp.routes/backends` as originally proposed).

Remaining for demo readiness:
1. **Envoy YAML wiring** — inbound `/mcp` route → `orange-mcp-loopback` cluster (10004), egress listener on 10005 with `orange-mcp-egress-match`, backend clusters.
2. **E2E tests** — `examples/orange/e2e/` has no MCP coverage yet.
3. **Demo docs** — `examples/orange/README.md` MCP section.

Start with the Envoy wiring; the rest follows.

---

## Required Reading

Read these before editing code:

- `AGENTS.md` at repo root.
- `docs/orange-pipeline-sdk/orange-mcp-sidecar.md`
- `docs/orange-pipeline-sdk/orange-websocket-sidecar.md`
- `docs/orange-pipeline-sdk/plan.md`, especially WS-E.fan and WS-G.
- `docs/orange-pipeline-sdk/mcp-fit-notes.md`, especially WS-E and WS-G.
- `examples/orange/internal/pipeline/responsesws/`
- `/Users/dio/src/dio/ai-gateway/internal/mcpproxy/`

Use these skills before implementation:

- `transit-example-creator`
- `transit-e2e-authoring`
- `transit-e2e-runner`
- `transit-envoy-dynamic-modules`

AI Gateway `internal/mcpproxy` is a pattern reference, not a dependency. Do not
import it or subtree-copy it wholesale.

---

## Architecture To Build

Target path:

```text
client
  -> Envoy inbound listener
  -> MCP streamable-HTTP/SSE route to orange-mcp loopback
  -> orange-mcp sidecar
       handles initialize/session envelope
       owns backend session IDs and last-event IDs
       fans out initialize/list methods when needed
       resolves tools/call to one configured backend
       writes x-orange-mcp-* headers on egress requests
       dials local Envoy MCP egress listener
  -> Envoy MCP egress listener
  -> orange-mcp-egress-match filter
       validates and strips x-orange-mcp-* headers
       writes Orange MCP decision metadata/filter-state
  -> Orange MCP route/cluster path
  -> configured MCP backend
```

This is the same sidecar strategy as `orange-responsesws`: embedded server
started by `up.Register(..., up.WithGroup(g))`, loopback inbound cluster,
separate Envoy egress listener, internal headers validated and stripped before
backend egress.

Non-negotiables:

- No provider-direct or MCP-server-direct dial as the default.
- No CGO in handler code.
- `down/abi_impl` remains blank-imported only in `examples/orange/cmd/main.go`.
- Envoy owns egress TLS, routing, access logs, stats, and trace context.
- `x-orange-mcp-*` headers are internal only; sidecar overwrites them, egress
  filter validates and strips them.
- Records must not log raw JSON-RPC params/results, provider credentials,
  authorization headers, raw backend session IDs, or unredacted internal
  headers.

---

## Implementation Plan

Implement this as small PR-sized slices. Stop after each slice with tests
green.

### PR 1 — Config Shape And Registration Skeleton ✅ DONE

Implemented config schema (`profiles`/`servers`, not the originally proposed `routes`/`backends`):

```yaml
mcp:
  profiles:
    default:
      servers: [kiwi, aws-knowledge, github]
  servers:
    kiwi:
      cluster: orange-mcp-kiwi
      endpoint: https://mcp.kiwi.com
      credential_ref: env://MCP_KIWI_TOKEN
      tools:
        include: ["search-flight"]
    aws-knowledge:
      cluster: orange-mcp-aws-knowledge
      endpoint: https://knowledge-mcp.global.api.aws
      tools:
        include: ["aws____read_documentation"]
```

The `mcp.profiles` map names a logical profile (multi-server fan-out).
The `mcp.servers` map names individual Envoy-routed MCP servers.
The `/mcp/s/<server>` path routes to a single-server synthetic profile.

Files landed: `config.go`, `config.schema.json`, `config_test.go`, testdata,
`orange.yaml`, `e2e/testdata/orange.yaml`, `cmd/main.go` (blank import), all
`pipeline/mcp/` files for filter constants and sidecar skeleton.

### PR 2 — Session Envelope, SSE Parser, And Tool Selectors ✅ DONE

Implemented:

- `crypto.go` — PBKDF2/AES-GCM envelope, fallback decrypt key support.
- `session.go` — encrypted public `mcp-session-id` (route, subject, per-backend
  session IDs, capability flags) and `Last-Event-Id` (backend name + event ID).
- `sse.go` — SSE parser: CR/LF/CRLF normalization, UTF-8 BOM, JSON-RPC `data:`
  lines, `application/json` accepted as one message.
- `selectors.go` — deny-wins tool selector: exact include/exclude, regexp.

All have `*_test.go` coverage.

### PR 3 — Sidecar HTTP Handlers ✅ DONE

Implemented in `handlers.go` and `records.go`:

- POST: initialize fan-out, tools/list merge, tools/call routing, client
  response routing, single-backend broadcast fallback.
- GET: backend SSE streams merged, event IDs rewritten, heartbeat emitted.
- DELETE: best-effort backend session close.
- Records: bounded, redacted (no raw params/results, credentials, auth headers,
  raw backend session IDs, or raw internal headers).

### PR 4 — Egress Filter ✅ DONE (Go code) / ⏳ PENDING (Envoy YAML)

Go code implemented in `egress.go` and `egress_test.go`:

- Validates `x-orange-mcp-route`, `x-orange-mcp-backend`, etc.
- Strips all `x-orange-mcp-*` headers before backend egress.
- Sets dynamic metadata for access logs.

**Still needed — Envoy YAML wiring** in both `examples/orange/e2e/testdata/envoy.tmpl.yaml`
and the live `examples/orange/orange.yaml` (if it doubles as the static Envoy
config for the demo):

Inbound HCM changes:
- Add `orange-mcp` no-op filter to start its group.
- Add route: `prefix: /mcp` → cluster `orange-mcp-loopback`.

New static cluster `orange-mcp-loopback`:
- `127.0.0.1:10004`, plain HTTP, no TLS.

New egress listener `orange-mcp-egress` on `127.0.0.1:10005`:
- HCM with `orange-mcp-egress-match` filter before router.
- Route table: per-backend cluster entries (one static cluster per MCP server in
  the config, e.g. `orange-mcp-kiwi`, `orange-mcp-aws-knowledge`, `orange-mcp-github`).
- TLS upstream clusters for real backends (HTTPS with system CA, `auto_host_sni`).

Internal headers to strip (egress filter already handles this in Go; the YAML
`request_headers_to_remove` on the route or the filter itself must be
consistent):

```
x-orange-mcp-route
x-orange-mcp-backend
x-orange-mcp-method
x-orange-mcp-request-id
x-orange-mcp-tool
x-orange-mcp-session
x-orange-mcp-last-event-id
```

Tests after wiring:

```sh
cd examples && GOWORK=off go test ./orange/internal/pipeline/mcp -count=1
```

Exit:

- Egress filter unit tests green (already passing).
- e2e Envoy template includes loopback cluster and egress listener.
- Demo `./mcp-demo profile=default initialize` reaches the sidecar through
  Envoy (even if backends are stubs).

### PR 5 — Orange E2E ⏳ PENDING

Add MCP coverage to `examples/orange/e2e/` alongside the existing LLM tests.
The e2e harness is in `examples/internal/e2etest/`; follow `e2e_test.go`.

**Prerequisite:** PR 4 Envoy YAML wiring must land first.

Stub MCP servers: spin up in-process `httptest.Server` instances in
`TestMain` that speak minimal MCP (initialize → session, tools/list, tools/call,
GET/SSE, DELETE). Wire them into the e2e Envoy template as the
`orange-mcp-*` backend clusters pointing to `127.0.0.1:<stub port>`.
No external network required; use local HTTP clusters in the Envoy config for
stubs.

Scenarios:

- `POST /mcp/default initialize` → sidecar via Envoy → both stub backends
  initialized → public `mcp-session-id` returned.
- one stub backend fails initialize → other succeeds → all-failed error
  returned (current `handleInitialize` requires all backends).
- `tools/list` with valid session → merged prefixed tool names from both stubs.
- `tools/call kiwi__search-flight` → reaches only the kiwi stub.
- GET `/mcp/default` with valid session → SSE stream with heartbeat + stub
  notification events; `Last-Event-Id` is rewritten.
- `Last-Event-Id` reconnect → stub receives its backend event ID.
- DELETE with valid session → both stubs receive DELETE.
- No `x-orange-mcp-*` headers visible on stub requests.
- `/mcp/s/kiwi initialize` → single-server path reaches only kiwi stub.

Run:

```sh
cd examples && GOWORK=off go test ./orange/... -count=1
make -C examples/orange e2e
```

Exit:

- Orange is demonstrably an LLM + MCP proxy locally.
- Egress-via-Envoy is visible in test assertions (stub request headers logged).

### PR 6 — Docs And Demo ⏳ PENDING

Update:

- `examples/orange/README.md` — add MCP section with:
  - Prerequisites (running Envoy with MCP wiring, `ORANGE_MCP_SESSION_KEYS` for
    production or `orange-generated` for dev ephemeral key).
  - Demo workflow using `examples/orange/mcp-demo`:

```sh
# Initialize a profile (all configured backends):
./mcp-demo profile=default
# → prints: export ORANGE_MCP_SESSION_ID='<token>'

export ORANGE_MCP_SESSION_ID='<token>'

# List merged tools across backends:
./mcp-demo profile=default list

# Call a prefixed tool:
./mcp-demo profile=default call kiwi__search-flight '{"origin":"SFO","destination":"JFK"}'

# Open SSE event stream:
./mcp-demo profile=default stream

# Initialize a single server directly:
./mcp-demo server=github initialize

# Delete session:
./mcp-demo profile=default delete
```

- `docs/orange-pipeline-sdk/orange-mcp-sidecar.md` — update if the
  implementation diverges from the design (config schema, egress path format,
  session/event envelope shape).
- `docs/orange-pipeline-sdk/plan.md` — mark MCP sidecar workstream landed once
  e2e passes.

Do not claim provider-direct remote MCP passthrough. Orange-managed MCP (all
egress through Envoy) is the supported v1 path.

---

## AI Gateway Pattern Checklist

Use `/Users/dio/src/dio/ai-gateway/internal/mcpproxy/` for these patterns:

- `crypto.go`: PBKDF2/AES-GCM envelope and fallback decrypt.
- `session.go`: composite session IDs, per-backend event IDs, heartbeat,
  notification multiplexing, DELETE close.
- `sse.go`: robust SSE parser and writer.
- `config.go`: deny-wins tool selectors and config-change signaler.
- `handlers.go`: initialize fan-out, tools/call `isError=true`
  classification, server-to-client request ID rewriting.

Do not copy:

- AI Gateway filterapi config types.
- AI Gateway metrics/tracing interfaces.
- AI Gateway internal headers.
- AI Gateway authorization engine unless an Orange policy surface explicitly
  requires it later.

---

## Verification Commands

Use focused tests while developing:

```sh
cd examples && GOWORK=off go test ./orange/internal/config ./orange/internal/pipeline/mcp -count=1
cd examples && GOWORK=off go test ./orange/... -count=1
make -C examples/orange e2e
```

If e2e needs Envoy setup, follow the existing Orange Makefile and e2e harness.
Do not sleep in tests; use bounded waits.

---

## Definition Of Done

Already true (PRs 1–4 Go code):

- [x] `examples/orange/internal/pipeline/mcp` exists and is registered from
  `examples/orange/cmd/main.go`.
- [x] Orange config declares MCP profiles/servers/tool selectors.
- [x] Sidecar starts with `up.Group`, has readiness/shutdown tests, dials only
  the local Envoy egress listener by default.
- [x] Egress filter validates and strips all internal MCP headers.
- [x] Session envelope and `Last-Event-Id` are encrypted and tested.
- [x] `initialize`, `tools/list`, `tools/call`, GET/SSE, and DELETE covered by
  unit tests.
- [x] Records are bounded and redacted.
- [x] No direct provider/server MCP dial introduced as the default path.

Still needed for demo readiness:

- [ ] Envoy YAML wiring: inbound `/mcp` → loopback (10004), egress listener
  (10005) with `orange-mcp-egress-match`, per-server backend clusters.
- [ ] `examples/orange/e2e/` MCP scenarios pass (`make e2e`).
- [ ] Backend MCP servers never see `x-orange-mcp-*` (asserted in e2e).
- [ ] `examples/orange/README.md` MCP demo section.
- [ ] `./mcp-demo profile=default initialize` produces a public session through
  Envoy (smoke test with stub or real backends).
