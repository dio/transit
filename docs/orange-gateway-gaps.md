# Orange LLM Gateway — Gap Analysis for Claude Code / Anthropic v1/messages

> **Implementation status** (2026-06-05):
> GAP-1 ✅ implemented | GAP-2 ✅ implemented (via passthrough mode) | GAP-3 ✅ implemented | GAP-4 open

> Source: `docs/gateway-requirements.md` (fetched from Claude Code docs)
> Codebase snapshot: `examples/orange` on branch `main`
> Date: 2026-06-05

---

## Background

Claude Code requires an LLM gateway to expose the Anthropic Messages API format
(`/v1/messages`, `/v1/messages/count_tokens`) and to faithfully forward specific
request headers. Orange currently handles `POST /v1/messages` and `GET /v1/models`
but has four gaps that prevent full Claude Code compatibility.

---

## Gaps

### GAP-1 — `POST /v1/messages/count_tokens` not routed (Critical)

**Problem**
The `match` router (`internal/pipeline/match/match.go`) has no route for
`/v1/messages/count_tokens`. Any request to that path hits the catch-all 404
handler and returns a JSON error to the client. Claude Code calls this endpoint
before submitting large requests to check token budgets.

**Root cause**
`match.go` defines five named paths:

```go
pathV1ChatCompletions = "/v1/chat/completions"
pathV1Messages        = "/v1/messages"
pathV1Models          = "/v1/models"
pathV1Responses       = "/v1/responses"
pathMCP               = "/mcp"
```

`/v1/messages/count_tokens` is absent. Even if it were routed, the
`anthropicPassthrough` translator (`internal/translator/anthropic_anthropic.go`)
only ever sets `:path` to `<prefix>/messages` — it has no code path for the
`count_tokens` sub-path.

**Action items**

1. Add a constant `pathV1MessagesCountTokens = "/v1/messages/count_tokens"` in
   `match/match.go`.
2. Register a new `POST` route that calls `tagRequestForEndpoint` with a new
   `EndpointCountTokens = "count_tokens"` discriminator (or reuse
   `EndpointMessages` if no separate metering is needed — see verification note).
3. In `translator/anthropic_anthropic.go`, extend `RequestBody` to set
   `:path` to `<prefix>/messages/count_tokens` when the endpoint discriminator
   is `count_tokens`. The body is forwarded unchanged (no translation needed).
4. Extend the `adapter` in `adapt.go` if the translator lookup needs a new
   `schema:count_tokens` registry key, or rely on the existing `anthropic`
   fallback — verify via the `NewForRoute` logic in `translator/registry.go`.

**Verification**

- `curl -X POST http://localhost:8080/v1/messages/count_tokens \
    -H 'content-type: application/json' \
    -d '{"model":"claude-haiku-4-5","messages":[{"role":"user","content":"hi"}]}'`
  must return HTTP 200 with an Anthropic `token_count` response body, not 404.
- The access log must show `"path":"/v1/messages/count_tokens"`.
- Existing `POST /v1/messages` requests must be unaffected (regression test via
  `e2e/e2e_test.go`).

---

### GAP-2 — `anthropic-version` header stripped and not re-injected (Critical)

**Problem**
`adapt.go:54` removes `anthropic-version` from every upstream request:

```go
var stripRequestHeaders = []string{"authorization", "x-api-key", "anthropic-version"}
```

For OpenAI-to-Anthropic translation, the relevant translator injects its own
`anthropic-version`. But `anthropicPassthrough.RequestHeaders` in
`translator/anthropic_anthropic.go` returns `nil, nil` — no header is
re-added. The Anthropic upstream therefore receives no `anthropic-version`,
which breaks feature negotiation (streaming, extended thinking, etc.).

The gateway requirements doc states:

> The gateway must forward request headers: `anthropic-beta`, `anthropic-version`

**Root cause**
`adapt.handler()` calls `w.RemoveRequestHeader` for each entry in
`stripRequestHeaders` *before* calling `t.RequestHeaders(allRequestHeaders(r))`.
`allRequestHeaders(r)` reads from the original client request `r`, so the
translator does receive the value — it just discards it by returning nil.

**Action items**

1. In `translator/anthropic_anthropic.go`, update `anthropicPassthrough` to
   store the configured `anthropic_version` from `cfg.Extra`:

   ```go
   type anthropicPassthrough struct {
       messagesPath    string
       anthropicVersion string // from cfg.Extra["anthropic_version"]
   }
   ```

2. Update the `init()` factory to populate it:

   ```go
   return &anthropicPassthrough{
       messagesPath:     path.Join("/", cfg.PathPrefix, "messages"),
       anthropicVersion: cfg.Extra["anthropic_version"],
   }
   ```

3. In `RequestHeaders`, re-inject the header, preferring the client-supplied
   value over the config fallback:

   ```go
   func (a *anthropicPassthrough) RequestHeaders(hdrs map[string]string) ([]Header, error) {
       v := hdrs["anthropic-version"]
       if v == "" {
           v = a.anthropicVersion
       }
       if v == "" {
           return nil, nil
       }
       return []Header{{Name: "anthropic-version", Value: v}}, nil
   }
   ```

   This satisfies both the gateway requirement (forward client header) and the
   existing `orange.yaml` fallback (`extra.anthropic_version: "2023-06-01"`).

**Verification**

- Send `POST /v1/messages` without an `anthropic-version` header; confirm the
  upstream receives `anthropic-version: 2023-06-01` (from config fallback).
- Send with `anthropic-version: 2023-06-01`; confirm it is forwarded as-is.
- Send with a newer `anthropic-version`; confirm it is forwarded unchanged (not
  overridden by the config fallback).
- Confirm that OpenAI-to-Anthropic translation paths are unaffected: those
  translators inject their own value independently.

---

### GAP-3 — No passthrough indicator in access log (Important)

**Problem**
The access log (`envoy.tmpl.yaml:70-88`) includes `endpoint`, `model`,
`upstream`, and `provider`, but has no field to distinguish a *passthrough*
request (client speaks Anthropic, upstream is Anthropic, body forwarded as-is)
from a *translated* request (e.g. OpenAI Chat Completions → Anthropic Messages).
This matters for audit, debugging, and cost attribution when orange routes
traffic to multiple backend types.

**Action items**

1. In `adapt.handler()` (`pipeline/adapt/adapt.go`), after determining the
   provider's effective backend schema, write a boolean dynamic metadata key:

   ```go
   isPassthrough := prov.EffectiveBackendSchema() == "anthropic"
   if isPassthrough {
       w.SetMetadata(match.MetadataNamespace, "passthrough", true)
   }
   ```

   Alternatively, expose the backend schema itself so log consumers can derive
   it: `w.SetMetadata(match.MetadataNamespace, "backend_schema", schema)`.

2. Add the field to the LLM access log stanza in `envoy.tmpl.yaml`:

   ```yaml
   passthrough: "%DYNAMIC_METADATA(orange:passthrough)%"
   ```

   (or `backend_schema` if that approach is preferred in step 1).

3. Mirror the field in the e2e test template
   (`e2e/testdata/envoy.tmpl.yaml`) so it stays in sync.

**Verification**

- A direct Anthropic request (`claude-haiku-4-5` via `anthropic` provider) must
  log `"passthrough": "true"` (or `"backend_schema": "anthropic"`).
- An OpenAI-routed request (`gpt-4o-mini`) must log `"passthrough": "-"` (Envoy
  substitutes `-` for absent metadata).
- A cross-provider request (e.g. `vertex/claude-opus-4` via `gcpanthropic`)
  must log `"passthrough": "-"` — it is translated, not forwarded as-is.

---

### GAP-4 — `display_name` missing from `/v1/models` response (Minor)

**Problem**
When Claude Code starts with `CLAUDE_CODE_ENABLE_GATEWAY_MODEL_DISCOVERY=1`, it
calls `GET /v1/models` and uses the `display_name` field from each entry for the
model picker label. Orange's `V1Model` struct (`config/config.go:276`) has no
`display_name` field, so the picker falls back to the raw model ID for every
entry.

**Action items**

1. Add `DisplayName` to `V1Model`:

   ```go
   type V1Model struct {
       ID          string         `json:"id"`
       Object      string         `json:"object"`
       OwnedBy     string         `json:"owned_by"`
       DisplayName string         `json:"display_name,omitempty"`
       Metadata    map[string]any `json:"metadata,omitempty"`
   }
   ```

2. Populate it from `ModelEntry.Metadata["display_name"]` in
   `Config.OpenAIV1Models()`:

   ```go
   var displayName string
   if v, ok := e.Metadata["display_name"].(string); ok {
       displayName = v
   }
   data = append(data, V1Model{
       ID:          id,
       Object:      "model",
       OwnedBy:     e.Provider,
       DisplayName: displayName,
       Metadata:    e.Metadata,
   })
   ```

3. Add `display_name` entries to `orange.yaml` models where a human-friendly
   label is desired, e.g.:

   ```yaml
   claude-haiku-4-5:
     provider: anthropic
     name: claude-haiku-4-5-20251001
     metadata:
       display_name: "Claude Haiku 4.5"
   ```

4. Update `internal/config/testdata/v1models.response.schema.json` to include
   `display_name` as an optional string property.

**Verification**

- `curl http://localhost:8080/v1/models | jq '.data[] | {id, display_name}'`
  must return `display_name` for models that have it set in config.
- Models without `display_name` in metadata must omit the field (not emit
  `"display_name": ""`).
- The existing `config_test.go` model-list test must still pass.

---

## Items confirmed not broken

| Concern | Status | Evidence |
|---------|--------|---------|
| `anthropic-beta` forwarded | ✅ | Not in `stripRequestHeaders`; passes through |
| `X-Claude-Code-Session-Id` forwarded | ✅ | Not in `stripRequestHeaders` |
| `X-Claude-Code-Agent-Id` forwarded | ✅ | Not in `stripRequestHeaders` |
| `X-Claude-Code-Parent-Agent-Id` forwarded | ✅ | Not in `stripRequestHeaders` |
| `GET /v1/models` endpoint | ✅ | Routed in `match.go:146` |
| Token metering for Anthropic | ✅ | `meter_anthropic_messages.go` handles both SSE and JSON |
| SSE streaming (`stream: true`) | ✅ | `adapt.bodyHandler` forces `accept-encoding: identity` |

---

## Priority order for implementation

| Priority | Gap | Status | Risk if skipped |
|----------|-----|--------|----------------|
| 1 | GAP-1: `/v1/messages/count_tokens` | ✅ Done | Claude Code token-count calls return 404 |
| 2 | GAP-2: `anthropic-version` not forwarded | ✅ Done | Feature negotiation fails; extended thinking and beta features break |
| 3 | GAP-3: Passthrough field in access log | ✅ Done | No audit trail distinguishing passthrough from translated requests |
| 4 | GAP-4: `display_name` in model list | Open | Model picker shows raw IDs instead of friendly names |

## Implementation notes (2026-06-05)

**GAP-1** — Added `pathV1MessagesCountTokens` route and `EndpointCountTokens` discriminator to
`match/match.go`. Registered `"anthropic:count_tokens"` translator key in
`translator/anthropic_anthropic.go` that sets `:path` to `<prefix>/messages/count_tokens`.
Added `count-tokens` command to `demos/llm` for quick manual testing.

**GAP-2** — Resolved as a side effect of the passthrough mode implementation (GAP-3).
In passthrough mode `anthropic-version` is not stripped so it flows through to Anthropic
as-is. In normal mode `AnthropicAuth.InjectAuth` was already re-injecting the configured
version — no change needed there.

**GAP-3 / Passthrough mode** — `x-orange-api-key` presence is the per-request switch
(value ignored for now — validation deferred). Implemented in `adapt/adapt.go`:
- Passthrough: strip `x-orange-api-key` only; skip stripping `authorization`/`x-api-key`/
  `anthropic-version`; skip `InjectAuth` and `InjectAuthWithBody`; write
  `orange:passthrough = "true"` to dynamic metadata.
- Normal: existing behaviour unchanged.
Added `passthrough` and `gateway_client` fields to both `envoy.tmpl.yaml` files.
`gateway_client` is wired but never set (renders as `-`) until the key resolver is designed.
