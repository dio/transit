# Prelim 1 — `keys[]` materialized blob loading

**Status**: prerequisite for orange LLM fallback (see `docs/orange-fallback-prompt.md`).
**Depends on**: nothing.
**Parallelisable with**: prelim 2.

## Why this comes first

`orange` today resolves `model → provider` from a single global table
(`llm.models{}` in `examples/orange/orange.yaml`,
`Config.LookupModel` at `examples/orange/internal/config/config.go:229`).
Fallback only makes sense per-key, because different callers author
different chains. Before any Chain/Target work, orange needs:

1. A per-key materialized blob structure.
2. Key-id extraction from the `Authorization: Bearer …` header.
3. A single-lookup resolver with no inheritance (per
   `docs/orange-policies-llm-mcp.md` § "Key resolution at request time").

Refer to `docs/orange-policies-llm-mcp.md` § 1 for the schema shape and
`pear/design/15-policy-model.md` § "Resolution pipeline" for the
data-plane contract.

## Scope

### Config

- Add `Config.Keys map[string]*KeyBlob` and the `KeyBlob` type in
  `internal/config/config.go` (or a new `internal/config/keys.go`):

  ```go
  type KeyBlob struct {
      Workspace string
      User      string
      LLM       KeyLLM
  }

  type KeyLLM struct {
      Models map[string]*Model // same shape as cfg.Models today
  }
  ```

- Extend `config.schema.json` with the `keys` block; the existing
  top-level `llm.models` remains valid (legacy mode — see below).

- Loader: parse `keys[id]` entries with `workspace`/`user`/`llm.models`.
  Validate that the key id starts with `<workspace>/<user>/` and matches
  the declared `workspace` / `user` fields. Reject on mismatch.

### Resolution

- Add `Config.LookupKey(keyID string) (*KeyBlob, bool)` — one map read,
  no cascade.

- Add a header-phase helper `match.ResolveKey(authHeader string)` that
  parses the bearer token, derives the opaque key id, and calls
  `LookupKey`. Reject with `orange.unknown_key` (HTTP 401, dynamic
  metadata `orange.reject_reason = unknown_key`) when the key id is
  absent from `keys[]`.

- Update `match.LookupModel(model, endpoint)` (callsite
  `internal/pipeline/match/match.go:227` and
  `internal/pipeline/responsesws/responsesws.go:439`) to a new
  signature:

  ```go
  func (c *Config) LookupModelForKey(keyBlob *KeyBlob, model, endpoint string)
      (provider, backendModel string, ok bool)
  ```

  Existing `LookupModel` stays as the legacy/no-keys path.

### Legacy mode

If the loaded config has **no** `keys[]` entries, fall back to the
current global `Config.LookupModel` path. This keeps the static
`examples/orange/orange.yaml` working without edits during the
transition. Once `keys[]` is present, unknown keys are rejected — no
implicit fallthrough.

### Attribution

When a request resolves through a `KeyBlob`, write the attribution
tuple `(workspace, user, key_id)` to dynamic metadata under
`orange.attribution.*` so access logs and downstream filters can read
it without re-parsing.

## Deliverables

- `internal/config/config.go` (or new file) — `KeyBlob`, `KeyLLM`,
  `Config.Keys`, `LookupKey`, validation.
- `config.schema.json` — `keys` block.
- `internal/pipeline/match/match.go` — header-phase `ResolveKey`,
  `LookupModelForKey` wiring, 401 reject.
- `internal/pipeline/responsesws/responsesws.go` — same lookup change.
- Tests in `internal/config/config_test.go`:
  - Legacy mode (no keys[]) still resolves models.
  - Known key resolves to its blob's model entry.
  - Unknown key → 401 with `orange.unknown_key`.
  - Workspace/user mismatch on load → loader error.
- Tests for `match.ResolveKey` (table-driven).

## Out of scope

- Routing trees (Chain/Split/Target). Models stay single-target in this
  prelim — `KeyLLM.Models` is the same shape as today's `cfg.Models`.
- Provider bindings (prelim 2).
- MCP per-profile changes.
- BYOK credential overrides.
- confpack-style packed memory representation (`docs/orange-policies-llm-mcp.md`
  § 5.1) — defer; keep plain Go structs behind a small interface so
  the packed form can slot in later without rippling through `match`.

## Conventions

- Use `gsed` (not `sed`) for any in-place schema edits.
- Don't add helper packages; keep additions in `internal/config` and
  the existing pipeline packages.

## Acceptance

`go test ./examples/orange/...` passes with the new tests, and a
hand-crafted `orange.yaml` containing one `keys[id]` entry resolves
correctly for its declared model, rejects unknown keys, and falls back
to legacy behavior when `keys[]` is omitted entirely.
