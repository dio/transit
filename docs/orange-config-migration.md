# Orange Config Migration: Old System → New System

**Goal:** Remove `config.go` (the old YAML-decode-and-validate system) and all code
that depends on it, leaving only the new system (`config_raw.go`, `config_compile.go`,
`config_decode.go`, `config_domain.go`, `config_appstate.go`, `config_usertable.go`,
`config_envelope.go`, `config_store.go`, `config_secret.go`, `config_intern.go`).

**Verification bar:** every demo in `examples/orange/demos/` works against the
migrated binary with adapted config files. Demos are the source of truth for
end-to-end correctness because they exercise real request paths without requiring
a full Envoy deployment.

---

## 1. Background

Two config systems coexist in `examples/orange/internal/config`:

| | Old system | New system |
|---|---|---|
| **Entry type** | `Config` (config.go) | `ConfigSnapshot` (config_domain.go) |
| **Decode entry point** | `Load([]byte)` | `AppState.LoadConfig([]byte)` / `AppState.ApplySnapshotEnvelope(env)` |
| **State holder** | Package-level `atomic.Pointer[Config]` singleton (`Get()`) | `AppState` (caller-owned, injectable) |
| **Key/profile lookup** | `cfg.LookupKey()`, `cfg.HasKeys()` | `AppState.Resolve(keyID, profileID)` → `ResolvedKey`, `ResolvedProfile` |
| **Provider auth** | `cfg.ProviderSecret(name)` | `CachedResolver.Resolve(ctx, authConfig.SecretRef)` |
| **Lifecycle** | `config.Start()`, `config.EnsureLogger()`, `config.EnableFileWatch()` | Caller wires `AppState` + file watcher separately |
| **YAML schema guard** | `config.schema.json` + jsonschema in `Load()` | Schema enforced by compile() errors |

The new system is already used by admin/control-plane paths (`internal/server/`,
`internal/egress/`, `internal/rls/`). The data-plane pipeline packages (`pick`,
`match`, `adapt`, `mcp`, `responsesws`) still use the old system.

---

## 2. Demo Inventory

These are the runnable artifacts that constitute the verification bar. Each one
exercises specific pipeline paths:

| Demo | Script | Pipeline path exercised |
|---|---|---|
| LLM send/stream | `demos/llm` | match → pick → adapt → upstream |
| LLM model list | `demos/llm models` | match (GET /v1/models) |
| MCP profile/server | `demos/mcp` | match → mcp handlers → mcp egress |
| Claude Code CLI | `demos/claude` | Messages API → match → adapt |
| Goose agent | `demos/goose` | Chat Completions → match → adapt |
| Codex agent | `demos/codex` | Chat Completions → match → adapt |
| Image generation | `demos/images` | Chat Completions → match → adapt |
| Tracing unit | `demos/tracing/validate` | pipeline/tracer package |
| Tracing live | `demos/tracing/validate --live` | full request + span attributes |
| Fallback routing | `demos/fallback/orange.yaml` + `demos/llm` | chain routing via match |
| Split routing | `demos/split/orange.yaml` + `demos/llm` | split routing via match |

---

## 3. Config Files That Need Adaptation

All shipped YAML configs use the old format and must be updated to the new format
before (or alongside) the pipeline migration. The old format is rejected by the new
system's `compile()` function.

### 3.1 Old-format fields and their new equivalents

| Old field | New field | Location |
|---|---|---|
| `models.{id}.endpoints:` | `models.{id}.endpoint_overrides:` | `llm.models` |
| `models.{id}.routing:` | moved to `keys.{id}.routing_overrides.{model}:` | top-level `keys` |
| `mcp.profiles:` | `profiles:` (top-level) | top of document |
| `mcp.servers.{id}.tools.include:` | `mcp.servers.{id}.tools_include:` | `mcp.servers` |
| `keys.{id}.workspace:` | _(removed — parsed from key ID)_ | `keys` |
| `keys.{id}.user:` | _(removed — parsed from key ID)_ | `keys` |
| `keys.{id}.llm.models.{model}.routing:` | `keys.{id}.routing_overrides.{model}:` | `keys` |

### 3.2 `examples/orange/orange.yaml`

Three changes needed:

**a) `models.claude-haiku-4-5.endpoints` → `endpoint_overrides`:**
```yaml
# Before
claude-haiku-4-5:
  provider: anthropic
  name: claude-haiku-4-5-20251001
  endpoints:
    chat_completions: anthropic_openai_compat

# After
claude-haiku-4-5:
  provider: anthropic
  name: claude-haiku-4-5-20251001
  endpoint_overrides:
    chat_completions: anthropic_openai_compat
```

**b) Keys section — remove `workspace`/`user`/`llm` wrappers, flatten routing:**
```yaml
# Before
keys:
  demo/maya/sk-fallback:
    workspace: demo
    user: maya
    llm:
      models:
        claude-haiku-4-5:
          routing:
            chain:
              retry:
                retry_on: "connect-failure,reset,5xx,retriable-status-codes"
                per_try_timeout_ms: 10000
              children:
                - target: { provider: fallback_p1, name: claude-haiku-4-5 }
                - target: { provider: fallback_p2, name: claude-haiku-4-5 }
                - target: { provider: fallback_p3, name: claude-haiku-4-5 }
                - target: { provider: vertex_anthropic, name: claude-opus-4@20250514 }
  demo/maya/sk-split:
    workspace: demo
    user: maya
    llm:
      models:
        claude-haiku-4-5:
          routing:
            split:
              children:
                - weight: 34
                  target: { provider: split_p1, name: claude-haiku-4-5 }
                - weight: 33
                  target: { provider: split_p2, name: claude-haiku-4-5 }
                - weight: 33
                  target: { provider: split_p3, name: claude-haiku-4-5 }

# After
keys:
  demo/maya/sk-fallback:
    routing_overrides:
      claude-haiku-4-5:
        chain:
          retry:
            retry_on: "connect-failure,reset,5xx,retriable-status-codes"
            per_try_timeout_ms: 10000
          children:
            - target: { provider: fallback_p1, name: claude-haiku-4-5 }
            - target: { provider: fallback_p2, name: claude-haiku-4-5 }
            - target: { provider: fallback_p3, name: claude-haiku-4-5 }
            - target: { provider: vertex_anthropic, name: claude-opus-4@20250514 }
  demo/maya/sk-split:
    routing_overrides:
      claude-haiku-4-5:
        split:
          children:
            - weight: 34
              target: { provider: split_p1, name: claude-haiku-4-5 }
            - weight: 33
              target: { provider: split_p2, name: claude-haiku-4-5 }
            - weight: 33
              target: { provider: split_p3, name: claude-haiku-4-5 }
```

**c) MCP section — lift `profiles` to top level, flatten server tool lists:**
```yaml
# Before
mcp:
  profiles:
    default:
      tools:
        kiwi:
          include: ["search-flight"]
        aws-knowledge:
          include: ["read_documentation", "search_documentation"]
          optional: true
        github:
          include: ["search_repositories"]
          optional: true
      auth:
        github:
          type: bearer
          secret_ref: env://GITHUB_TOKEN
    kiwi-only:
      tools:
        kiwi: {}
  servers:
    kiwi:
      endpoint: https://mcp.kiwi.com
      namespace: kiwi
      tools:
        include: ["search-flight"]
    aws-knowledge:
      endpoint: https://knowledge-mcp.global.api.aws
      namespace: aws
      tools:
        include: ["read_documentation", "search_documentation"]
    github:
      endpoint: https://api.githubcopilot.com/mcp/
      namespace: github
      auth:
        type: bearer
        secret_ref: env://GITHUB_TOKEN
      tools:
        include: ["search_repositories", "get_file_contents"]

# After
profiles:
  default:
    tools:
      kiwi:
        include: ["search-flight"]
      aws-knowledge:
        include: ["read_documentation", "search_documentation"]
        optional: true
      github:
        include: ["search_repositories"]
        optional: true
    auth:
      github:
        type: bearer
        secret_ref: env://GITHUB_TOKEN
  kiwi-only:
    tools:
      kiwi: {}

mcp:
  servers:
    kiwi:
      endpoint: https://mcp.kiwi.com
      namespace: kiwi
      tools_include: ["search-flight"]
    aws-knowledge:
      endpoint: https://knowledge-mcp.global.api.aws
      namespace: aws
      tools_include: ["read_documentation", "search_documentation"]
    github:
      endpoint: https://api.githubcopilot.com/mcp/
      namespace: github
      auth:
        type: bearer
        secret_ref: env://GITHUB_TOKEN
      tools_include: ["search_repositories", "get_file_contents"]
```

### 3.3 `examples/orange/orange-no-env.yaml`

Same MCP changes as 3.2c: lift `mcp.profiles` to top level, change
`tools.include` arrays to `tools_include`.

### 3.4 `examples/orange/demos/split/orange.yaml`

Same key format change as 3.2b. No MCP section. `models: {}` stays as-is.

### 3.5 `examples/orange/demos/fallback/orange.yaml`

Same key format change as 3.2b. No MCP section. `models: {}` stays as-is.

### 3.6 `examples/orange/minimal.yaml`

No changes needed. Only has `llm.providers` and `llm.models`; no keys, MCP, or
`endpoints` fields.

---

## 4. Gaps: New Helpers to Add Before Migration

The pipeline packages call several methods on `*Config` that have no direct
counterpart in the new system yet. These must be added first.

### 4.1 `GlobalConfig.LookupModel(modelID string) (*ModelRecord, bool)`

Add to `config_domain.go`. Replaces `cfg.LookupModel(model, binding)` which
returned `(provider, backend, binding string)`. Callers resolve the provider
from `m.Provider` (`*ProviderRecord`) and the backend name from `m.APIName`.

### 4.2 `GlobalConfig.V1Models() []V1Model` and `V1Model` type

Add `V1Model` and `GlobalConfig.V1Models()` to `config_domain.go`. Replaces
`cfg.OpenAIV1Models()`. Build the list from `g.Models` sorted by key, using
the provider kind as `OwnedBy`.

### 4.3 Secret resolution at request time

Old: `cfg.ProviderSecret(name) string` — synchronous, cached internally.

New: `CachedResolver.Resolve(ctx, ref string) (string, error)` already exists in
`config_secret.go`. Inject a `*CachedResolver` (or the `SecretResolver` interface)
into pipeline handlers instead of calling the config singleton.

### 4.4 Binding support (add to new system)

The old system has `Provider.Bindings []Binding` and `ModelEntry.Binding string`.
The new system's `RawProvider` and `RawModel` have no binding fields yet.

Add to `config_raw.go`:
```go
type RawBinding struct {
    Name     string `yaml:"name"     json:"name"`
    Endpoint string `yaml:"endpoint" json:"endpoint"`
}
// In RawProvider:
Bindings []RawBinding `yaml:"bindings,omitempty" json:"bindings,omitempty"`
// In RawModel:
Binding string `yaml:"binding,omitempty" json:"binding,omitempty"`
```

Add `Bindings []Binding` and `Binding` type to `config_domain.go` and compile
them in `config_compile.go`. This gates the `demos/tracing/validate --live` path
and the `valid_bindings.yaml` → `demos/llm` binding-routing path.

### 4.5 `ResolvedProfile.AuthForServer(serverID string) *AuthConfig`

Add to `config_usertable.go`. Replaces `cfg.MCPCredential(routeName, serverName)`.
Falls back to `nil` when no profile override exists; caller then uses the
`ServerRecord.Auth` value.

### 4.6 `AppState` as injectable dependency (replacing `config.Get()`)

Define a narrow interface in each pipeline package covering only what that package
calls:

```go
// example for match package
type configSnapshot interface {
    Snapshot() *config.ConfigSnapshot
}
```

Wire the real `*config.AppState` in production; pass a stub in tests. This removes
all `config.Get()` call sites and makes each package independently testable.

### 4.7 `MCPToolSelector` → `[]ToolFilter`

`pipeline/mcp/selectors.go` uses `config.MCPToolSelector` to filter tools. Replace
with `[]config.ToolFilter` from `ResolvedProfile.ToolFilters`.

---

## 5. Migration Phases

Execute phases in order. Each phase leaves the build green and demos passing before
the next phase begins. The old `config.go` is deleted only at the end (Phase 7).

---

### Phase 0: Pre-migration additions to new system

*Scope: `internal/config/` only. No pipeline files touched. Config files adapted.*

**Code changes:**
1. Add `Binding` support to `RawProvider`/`RawModel` (§4.4) and compile into
   `ProviderRecord.Bindings` and `ModelRecord.Binding`.
2. Add `GlobalConfig.LookupModel()` (§4.1).
3. Add `V1Model` type and `GlobalConfig.V1Models()` (§4.2).
4. Add `ResolvedProfile.AuthForServer()` (§4.5).
5. Confirm `AppState.LoadConfig` and `AppState.Resolve` are fully covered by tests.

**Config file adaptation (§3):**
- Update `orange.yaml`: rename `endpoints` → `endpoint_overrides`, flatten keys,
  lift MCP profiles.
- Update `orange-no-env.yaml`: lift MCP profiles, flatten server tool lists.
- Update `demos/split/orange.yaml`: flatten keys.
- Update `demos/fallback/orange.yaml`: flatten keys.

**Verification:**
```bash
# Confirm new-system tests pass (old system tests still pass separately)
go test ./examples/orange/internal/config/...

# Confirm the adapted config files parse cleanly under the new system
go run ./examples/orange/cmd/orange -- --config examples/orange/orange.yaml --dry-run
go run ./examples/orange/cmd/orange -- --config examples/orange/demos/split/orange.yaml --dry-run
go run ./examples/orange/cmd/orange -- --config examples/orange/demos/fallback/orange.yaml --dry-run
```

---

### Phase 1: Migrate `pipeline/pick`

*Owner of config lifecycle (Start, EnableFileWatch, EnsureLogger).*

**Files:** `pick.go`, `pick_test.go`, `testdata/*.yaml`

**Steps:**
1. Add `AppState *config.AppState` to `pick.Options`. Remove calls to
   `config.EnsureLogger()`, `config.Start()`, `config.EnableFileWatch()`.
   Wire the caller (`internal/server/root.go` or `server.go`) to create and
   populate an `AppState` before constructing pick.
2. Replace every `config.Get()` in `pick.go` with `opts.AppState.Snapshot()`.
   Return an error (not a panic) if snapshot is nil.
3. Replace test setup (`orangecfg.EnvVar` + `orangecfg.MustReload()` — 8 sites)
   with `appState.LoadConfig(fileBytes)`.
4. Update `testdata/*.yaml` to the new format (flat `tools_include`, top-level
   `profiles:`, key `routing_overrides`). Delete old-format files.

**Verification:**
```bash
go test ./examples/orange/internal/pipeline/pick/...

# Start orange with the adapted orange.yaml and run the LLM demo
ORANGE_CONFIG=examples/orange/orange.yaml ./examples/orange/demos/llm models
ORANGE_CONFIG=examples/orange/orange.yaml ./examples/orange/demos/llm "hi"
```

---

### Phase 2: Migrate `pipeline/adapt`

*Uses `config.Get()`, `config.Provider`, `cfg.ProviderSecret()`.*

**Files:** `adapt.go`, `authcache.go`, `adapt_test.go`

**Steps:**
1. Replace `config.Provider` in all function signatures with `*config.ProviderRecord`.
2. Replace `config.Get()` with an injected `*config.AppState` (or `configSnapshot`
   interface) via `adapt.Options`.
3. Replace `cfg.ProviderSecret(upstream)` with
   `resolver.Resolve(ctx, provider.Auth.SecretRef)` where `resolver` is an injected
   `config.SecretResolver`.
4. `authcache.go`: replace all four `config.Provider` parameter types with
   `*config.ProviderRecord`. Read `provider.Auth.Type` (now `config.AuthConfig.Type`)
   and `provider.Auth.SecretRef` directly.
5. `adapt_test.go`: replace `config.Provider{...}` literals with
   `&config.ProviderRecord{...}`. Remove `config.EnvVar`/`MustReload()` setup;
   pass a pre-loaded `AppState` and a no-op resolver.

**Verification:**
```bash
go test ./examples/orange/internal/pipeline/adapt/...

# All auth types must route correctly
ORANGE_CONFIG=examples/orange/orange.yaml ./examples/orange/demos/llm "hi"
ORANGE_CONFIG=examples/orange/orange.yaml ./examples/orange/demos/llm --api messages "hi"
ORANGE_CONFIG=examples/orange/orange.yaml ./examples/orange/demos/claude -p "hi"
```

---

### Phase 3: Migrate `pipeline/mcp`

*Uses `config.Get()`, `Config`, `MCPServer`, `MCPProfile`, `MCPToolSelector`,
`cfg.MCPCredential()`.*

**Files:** `mcp.go`, `egress.go`, `handlers.go`, `selectors.go`,
`egress_test.go`, `handlers_test.go`, `selectors_test.go`

**Steps:**
1. **`selectors.go`/`selectors_test.go`:** Replace `config.MCPToolSelector` with
   `[]config.ToolFilter`. The tool-filter logic reads the same `Include`/`Optional`
   fields.
2. **`handlers.go`:** Replace `config.Config` parameter in `lookupMCPRoute` with
   `*config.ConfigSnapshot`. Replace `config.MCPServer` with `*config.ServerRecord`.
   Replace `config.MCPProfile` with `config.ResolvedProfile`. Change `opts.config`
   from `func() *config.Config` to `func() *config.ConfigSnapshot`.
3. **`egress.go`:** Replace `config.Get()` with `appState.Snapshot()`. Replace
   `cfg.MCPCredential(routeName, backendName)` with:
   - Load `ResolvedProfile` from stream context (set by match in Phase 4).
   - Call `profile.AuthForServer(backendName)` (§4.5).
   - If non-nil, `resolver.Resolve(ctx, auth.SecretRef)`.
   - Otherwise fall back to `snap.Global.Servers[backendName].Auth.SecretRef`.
4. **`mcp.go`:** Change `config: config.Get` to `config: appState.Snapshot`.
5. **Test files:** Replace old struct construction with new-system types. Replace
   `config.Load(testMCPYAML)` + `config.MustReload()` with
   `appState.LoadConfig([]byte(testMCPYAML))`.

**Verification:**
```bash
go test ./examples/orange/internal/pipeline/mcp/...

# MCP profile routing — requires orange.yaml with adapted profiles section
ORANGE_CONFIG=examples/orange/orange.yaml ./examples/orange/demos/mcp profile=default
ORANGE_CONFIG=examples/orange/orange.yaml ./examples/orange/demos/mcp profile=default list
ORANGE_CONFIG=examples/orange/orange.yaml ./examples/orange/demos/mcp server=kiwi
```

---

### Phase 4: Migrate `pipeline/match`

*Most deeply coupled. Carries `KeyBlob` in stream context, uses `LookupKey`,
`LookupModel`, `OpenAIV1Models`, `HasKeys`, routing node types.*

**Files:** `match.go`, `sampler.go`, `match_test.go`, `sampler_test.go`,
`testdata/match_test.yaml`, `testdata/match_keys_test.yaml`,
`testdata/match_bindings_test.yaml`, `testdata/match_split_test.yaml`

**Steps:**
1. **Stream context key type:** Replace `config.KeyBlob` stored under `KeyBlobKey`
   with `config.ResolvedKey`. Update every read site.
2. **`config.Get()` calls (4+ sites):** Inject `*config.AppState` via `match.Options`.
3. **Key authentication path:**
   - Replace `cfg.HasKeys()` with `snap.Keys != nil && len(snap.Keys) > 0`.
   - Replace `cfg.LookupKey(id)` with `appState.Resolve(keyID, profileID)`
     → `*ResolvedRequest` → `req.Key` (`ResolvedKey`). Store in stream context.
4. **Model routing path:**
   - Replace `cfg.LookupModel(model, binding)` with
     `snap.Global.LookupModel(modelID)` → `*ModelRecord`.
   - Provider is `m.Provider` (`*ProviderRecord`), backend is `m.APIName`,
     binding is `m.Binding`.
   - Routing overrides come from `ResolvedKey.RoutingOverrides[modelID]`
     (`*RoutingConfig`). Walk `RoutingConfig` trees instead of `RoutingNode` trees.
5. **`sampler.go`:** Replace `config.SplitNode` with `*config.SplitConfig`.
   `Children []SplitChild` becomes `Children []WeightedRoutingConfig`. `Weight`
   field is unchanged. The embedded `RoutingNode` becomes `Node RoutingConfig`.
6. **`GET /v1/models` handler:** Replace `cfg.OpenAIV1Models()` with
   `snap.Global.V1Models()`. Replace `config.V1ModelList` with `[]config.V1Model`.
7. **Test files:** Update YAML fixtures to new format. Replace `config.EnvVar` +
   `config.MustReload()` with `appState.LoadConfig(bytes)`.

**Verification:**
```bash
go test ./examples/orange/internal/pipeline/match/...

# Core model routing
ORANGE_CONFIG=examples/orange/orange.yaml ./examples/orange/demos/llm models
ORANGE_CONFIG=examples/orange/orange.yaml ./examples/orange/demos/llm "hi"
ORANGE_CONFIG=examples/orange/orange.yaml ./examples/orange/demos/llm --api messages "hi"

# Key-scoped routing: fallback chain
ORANGE_API_KEY=demo/maya/sk-fallback \
  ORANGE_CONFIG=examples/orange/orange.yaml ./examples/orange/demos/llm --model claude-haiku-4-5 "hi"

# Key-scoped routing: weighted split
ORANGE_API_KEY=demo/maya/sk-split \
  ORANGE_CONFIG=examples/orange/orange.yaml ./examples/orange/demos/llm --model claude-haiku-4-5 "hi"

# Standalone demo configs
ORANGE_CONFIG=examples/orange/demos/fallback/orange.yaml ./examples/orange/demos/llm --model claude-haiku-4-5 "hi"
ORANGE_CONFIG=examples/orange/demos/split/orange.yaml ./examples/orange/demos/llm --model claude-haiku-4-5 "hi"

# Tracing
./examples/orange/demos/tracing/validate
```

---

### Phase 5: Migrate `pipeline/responsesws`

*Uses `config.Get()`, `Provider`, `KeyBlob`, `HasKeys`, all four `Lookup*` methods.*

**Files:** `egress.go`, `responsesws.go`, `egress_test.go`

**Steps:**
1. Inject `*config.AppState` and `config.SecretResolver` into the handler via
   options struct or constructor. Remove all `config.Get()` calls.
2. Replace `config.Provider` with `*config.ProviderRecord`.
3. Replace `config.KeyBlob` with `config.ResolvedKey` (read from stream context
   set by match in Phase 4, or resolved fresh via `appState.Resolve(keyID, "")`).
4. Replace `cfg.HasKeys()` with `snap.Keys != nil && len(snap.Keys) > 0`.
5. Replace the four `Lookup*` methods:
   - `cfg.LookupModelProvider(model, binding)` → `snap.Global.LookupModel(model)`,
     read `m.Provider.Kind`.
   - `cfg.LookupModel(model, binding)` → same, read `m.Provider` and `m.APIName`.
   - `cfg.LookupModelForKey(kb, model, binding)` → check
     `resolvedKey.RoutingOverrides[model]`; if set, use its `Target.Provider`;
     otherwise fall back to global lookup.
   - `cfg.LookupModelProviderForKey(kb, model, binding)` → same, return provider
     name only.
6. Replace `cfg.ProviderSecret(provider)` with
   `resolver.Resolve(ctx, providerRec.Auth.SecretRef)`.
7. `egress_test.go`: remove `config.EnvVar`/`InitLogger()`/`MustReload()` setup;
   pass a pre-loaded `AppState` and a no-op resolver.

**Verification:**
```bash
go test ./examples/orange/internal/pipeline/responsesws/...

# Responses API (WebSocket path)
ORANGE_CONFIG=examples/orange/orange.yaml ./examples/orange/demos/llm --api responses "hi"
ORANGE_CONFIG=examples/orange/orange.yaml ./examples/orange/demos/llm --api responses --stream "count 1 to 5"

# Goose exercises the chat path; codex may use responses
ORANGE_CONFIG=examples/orange/orange.yaml ./examples/orange/demos/goose -p "hi"
```

---

### Phase 6: Patch `internal/server/resources/config_service.go`

Line 76 calls `config.Load(yamlBytes)` to validate incoming YAML before storing it.
Replace with a dry-run call to the new system:

```go
// Before
if _, err := config.Load(yamlBytes); err != nil {
    return err
}

// After — validate by compiling without publishing
if err := appState.ValidateConfig(yamlBytes); err != nil {
    return err
}
```

Add `AppState.ValidateConfig(yamlBytes []byte) error` to `config_appstate.go`:
it decodes and compiles but does not call `snapshot.Store`. All other symbols
in this file (`SnapshotEnvelope`, `SnapshotStore`, etc.) are already new-system
types and need no changes.

---

### Phase 7: Delete old system

Once Phases 0–6 all pass and all demos work:

**Delete files:**
```
examples/orange/internal/config/config.go
examples/orange/internal/config/config_test_exports.go
examples/orange/internal/config/config_test.go
examples/orange/internal/config/config.schema.json
examples/orange/internal/config/testdata/valid_minimal.yaml
examples/orange/internal/config/testdata/valid_full.yaml
examples/orange/internal/config/testdata/valid_keys.yaml
examples/orange/internal/config/testdata/valid_bindings.yaml
examples/orange/internal/config/testdata/invalid_bad_provider_ref.yaml
examples/orange/internal/config/testdata/invalid_key_id_mismatch.yaml
examples/orange/internal/config/testdata/invalid_missing_endpoint.yaml
examples/orange/internal/config/testdata/invalid_missing_kind.yaml
examples/orange/internal/config/testdata/invalid_missing_secret_ref.yaml
examples/orange/internal/config/testdata/invalid_unknown_auth_type.yaml
examples/orange/internal/config/testdata/v1models.response.schema.json
examples/orange/internal/pipeline/pick/testdata/one_provider.yaml
examples/orange/internal/pipeline/pick/testdata/one_binding.yaml
examples/orange/internal/pipeline/pick/testdata/two_providers.yaml
examples/orange/internal/pipeline/pick/testdata/bindings_provider.yaml
examples/orange/internal/pipeline/pick/testdata/unreferenced_binding.yaml
```

**Final verification — run every demo:**
```bash
# Full build
go build ./examples/orange/...
go test ./examples/orange/...

# LLM demos
ORANGE_CONFIG=examples/orange/orange.yaml ./examples/orange/demos/llm models
ORANGE_CONFIG=examples/orange/orange.yaml ./examples/orange/demos/llm "hi"
ORANGE_CONFIG=examples/orange/orange.yaml ./examples/orange/demos/llm --stream "count 1 to 5"
ORANGE_CONFIG=examples/orange/orange.yaml ./examples/orange/demos/llm --api messages "hi"
ORANGE_CONFIG=examples/orange/orange.yaml ./examples/orange/demos/llm --api responses "hi"
ORANGE_CONFIG=examples/orange/orange.yaml ./examples/orange/demos/llm embed "hello world"

# Agent demos
ORANGE_CONFIG=examples/orange/orange.yaml ./examples/orange/demos/claude -p "hi"
ORANGE_CONFIG=examples/orange/orange.yaml ./examples/orange/demos/goose -p "hi"

# Image demo
ORANGE_CONFIG=examples/orange/orange.yaml ./examples/orange/demos/images

# MCP demo
ORANGE_CONFIG=examples/orange/orange.yaml ./examples/orange/demos/mcp profile=default

# Key-scoped routing demos
ORANGE_API_KEY=demo/maya/sk-fallback \
  ORANGE_CONFIG=examples/orange/orange.yaml ./examples/orange/demos/llm --model claude-haiku-4-5 "hi"
ORANGE_API_KEY=demo/maya/sk-split \
  ORANGE_CONFIG=examples/orange/orange.yaml ./examples/orange/demos/llm --model claude-haiku-4-5 "hi"

# Standalone demo configs
ORANGE_CONFIG=examples/orange/demos/fallback/orange.yaml \
  ./examples/orange/demos/llm --model claude-haiku-4-5 "hi"
ORANGE_CONFIG=examples/orange/demos/split/orange.yaml \
  ./examples/orange/demos/llm --model claude-haiku-4-5 "hi"

# Tracing
./examples/orange/demos/tracing/validate
```

---

## 6. File Inventory

### Adapt (config files — Phase 0)
```
examples/orange/orange.yaml               endpoint_overrides, flat keys, lifted profiles
examples/orange/orange-no-env.yaml        lifted profiles, flat tools_include
examples/orange/demos/split/orange.yaml   flat keys (routing_overrides)
examples/orange/demos/fallback/orange.yaml flat keys (routing_overrides)
```

### Add (new-system additions — Phase 0)
```
examples/orange/internal/config/config_domain.go    LookupModel, V1Model, V1Models, Binding type
examples/orange/internal/config/config_raw.go       RawBinding, bindings on RawProvider/RawModel
examples/orange/internal/config/config_compile.go   compile Bindings into ProviderRecord
examples/orange/internal/config/config_appstate.go  ValidateConfig (Phase 6)
examples/orange/internal/config/config_usertable.go AuthForServer helper
```

### Modify (pipeline migration — Phases 1–6)
```
examples/orange/internal/pipeline/pick/pick.go
examples/orange/internal/pipeline/pick/pick_test.go
examples/orange/internal/pipeline/adapt/adapt.go
examples/orange/internal/pipeline/adapt/authcache.go
examples/orange/internal/pipeline/adapt/adapt_test.go
examples/orange/internal/pipeline/mcp/mcp.go
examples/orange/internal/pipeline/mcp/egress.go
examples/orange/internal/pipeline/mcp/handlers.go
examples/orange/internal/pipeline/mcp/selectors.go
examples/orange/internal/pipeline/mcp/egress_test.go
examples/orange/internal/pipeline/mcp/handlers_test.go
examples/orange/internal/pipeline/mcp/selectors_test.go
examples/orange/internal/pipeline/match/match.go
examples/orange/internal/pipeline/match/sampler.go
examples/orange/internal/pipeline/match/match_test.go
examples/orange/internal/pipeline/match/sampler_test.go
examples/orange/internal/pipeline/match/testdata/match_test.yaml
examples/orange/internal/pipeline/match/testdata/match_keys_test.yaml
examples/orange/internal/pipeline/match/testdata/match_bindings_test.yaml
examples/orange/internal/pipeline/match/testdata/match_split_test.yaml
examples/orange/internal/pipeline/responsesws/egress.go
examples/orange/internal/pipeline/responsesws/responsesws.go
examples/orange/internal/pipeline/responsesws/egress_test.go
examples/orange/internal/server/resources/config_service.go
```

### Delete (Phase 7)
```
examples/orange/internal/config/config.go
examples/orange/internal/config/config_test_exports.go
examples/orange/internal/config/config_test.go
examples/orange/internal/config/config.schema.json
examples/orange/internal/config/testdata/valid_minimal.yaml
examples/orange/internal/config/testdata/valid_full.yaml
examples/orange/internal/config/testdata/valid_keys.yaml
examples/orange/internal/config/testdata/valid_bindings.yaml
examples/orange/internal/config/testdata/invalid_bad_provider_ref.yaml
examples/orange/internal/config/testdata/invalid_key_id_mismatch.yaml
examples/orange/internal/config/testdata/invalid_missing_endpoint.yaml
examples/orange/internal/config/testdata/invalid_missing_kind.yaml
examples/orange/internal/config/testdata/invalid_missing_secret_ref.yaml
examples/orange/internal/config/testdata/invalid_unknown_auth_type.yaml
examples/orange/internal/config/testdata/v1models.response.schema.json
examples/orange/internal/pipeline/pick/testdata/one_provider.yaml
examples/orange/internal/pipeline/pick/testdata/one_binding.yaml
examples/orange/internal/pipeline/pick/testdata/two_providers.yaml
examples/orange/internal/pipeline/pick/testdata/bindings_provider.yaml
examples/orange/internal/pipeline/pick/testdata/unreferenced_binding.yaml
```

### No changes required
```
examples/orange/cmd/orange/main.go
examples/orange/cmd/module/main.go
examples/orange/minimal.yaml
examples/orange/internal/egress/client.go
examples/orange/internal/egress/watcher.go
examples/orange/internal/rls/
examples/orange/internal/server/server.go
examples/orange/internal/server/egress_emulate.go
examples/orange/internal/server/egress_emulate_repl.go
examples/orange/demos/llm              (no changes — curl-only)
examples/orange/demos/mcp              (no changes — curl-only)
examples/orange/demos/claude           (no changes — env-var wrapper)
examples/orange/demos/goose            (no changes — env-var wrapper)
examples/orange/demos/codex            (no changes — env-var wrapper)
examples/orange/demos/images           (no changes — curl-only)
examples/orange/demos/tracing/validate (no changes — runs unit tests + curl)
```
