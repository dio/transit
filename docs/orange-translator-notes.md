# Orange Translator — Implementation Notes

Supplementary to `orange-translator-design.md`. Records decisions made during
implementation that are not yet reflected in the design doc.

---

## Codemod Phase 2 — Extended Sync Scope

The codemod (`examples/orange/codemod/`) owns the **entire surface that must
stay in sync with ai-gateway**. Phase 2 implementation should include three
sync modes, not just translator transformation:

| Mode | Source | Target | Transform |
|---|---|---|---|
| `sync-apischema` | `ai-gateway/internal/apischema/{openai,anthropic,awsbedrock}/` | `examples/orange/internal/apischema/` | package-path rewrite only |
| `sync-helpers` | `ai-gateway/internal/translator/{util,openai_helper,anthropic_helper,...}.go` | `orange/internal/translator/` | import rewrite + metrics/tracing drop |
| `transform-translators` | `ai-gateway/internal/translator/openai_*.go` | `orange/internal/translator/` | full AST rewrite (existing Phase 2/3 scope) |

### Scheduled sync → PR workflow

```
cron: weekly (or triggered on ai-gateway release tag)
  → go run ./examples/orange/codemod --sync --upstream /path/to/ai-gateway
  → git diff --exit-code || (
      git checkout -b sync/ai-gateway-$(git -C $upstream rev-parse --short HEAD)
      git commit -am "chore: sync from ai-gateway@$(git -C $upstream rev-parse HEAD)"
      gh pr create \
        --title "chore: sync from ai-gateway@<sha>" \
        --body "$(grep -r CODEMOD-TODO examples/orange/internal/translator/ | head -40)"
    )
```

PR body should include:
- ai-gateway commit SHA and date
- `grep CODEMOD-TODO` hit list (locations needing human review)
- Diff summary: files added/removed/changed

Reviewers merge or hand-complete any new `CODEMOD-TODO` markers before
merging.

The initial copy performed in Phase 1 (the apischema packages + 6 helpers)
becomes the first golden test case for `sync-apischema` and `sync-helpers`.

---

## Logs / Metrics / Traces — Envoy-side delivery (deferred)

The helpers copied from ai-gateway silently drop all observability:
- `metrics.*` calls → replaced with `// CODEMOD-TODO: token usage dropped`
- `tracingapi.*` calls → removed entirely

These are not simply deleted; they need to be re-delivered **via Envoy**:

- **Metrics** — emit as Envoy stats counters/gauges via the dynamic module
  ABI (`envoy_dynamic_module_http_set_filter_state_bytes` or equivalent).
  Do NOT call Prometheus or OTel exporters directly from the Go plugin.
- **Traces** — attach span attributes to the Envoy-side trace via
  `x-b3-*` or W3C `traceparent` headers; do not open a gRPC OTLP
  connection from within the plugin.
- **Logs** — route through `up.Logger` or the admin server, not directly
  to `os.Stderr` or a sidecar.

This adaptation belongs in a dedicated phase (after Phase 4 filter wiring)
once the extproc callback surface is stable. Each `CODEMOD-TODO` marker in
the helpers is the exact hook point for this work.

---

## Import-path adaptations applied in Phase 1 copy

| ai-gateway import | orange equivalent | Reason |
|---|---|---|
| `github.com/envoyproxy/ai-gateway/internal/apischema/*` | `github.com/dio/transit/examples/orange/internal/apischema/*` | Copied into transit2; `internal` visibility requires same module |
| `github.com/envoyproxy/ai-gateway/internal/json` | `encoding/json` | Sonic wrapper; stdlib is deterministic enough for orange |
| `github.com/envoyproxy/ai-gateway/internal/internalapi` | inline: `Header` (local), `ModelNameOverride = string`, `ResponseModel = string` | Package had only these types in scope |
| `github.com/envoyproxy/ai-gateway/internal/metrics` | dropped + `// CODEMOD-TODO` | Token usage will be re-wired via Envoy stats |
| `github.com/envoyproxy/ai-gateway/internal/tracing/tracingapi` | dropped | Tracing will be re-wired via Envoy trace headers |

## Design-doc discrepancy: sjson options location

The design doc codemod section states:
> `sjsonOptions` / `sjsonOptionsInPlace` — Already present in `util.go` (copied verbatim); no action needed

**Actual location**: these vars are defined in ai-gateway's `translator.go`, not
`util.go`. Since ai-gateway's `translator.go` is being *replaced* (not copied),
orange's `util.go` copy does not contain them.

**Resolution**: when the codemod generates `openai_*.go` files that reference
`sjsonOptions`, those vars must be provided. They should be added to orange's
`translator.go` (alongside the interface) once `github.com/tidwall/sjson` is
confirmed as a `go.mod` dependency (it was added in Phase 1).

---

## Codemod Known Failures (Phase 3)

Discovered during the Phase 3 codemod run over all six `openai_*.go` provider
files. Each item is a concrete bug or gap in `examples/orange/codemod/` that
requires a fix before the codemod can be used for automated sync.

### 1. Test-file transform always fails ("could not find primary translator struct")

**Mode**: `-mode transform` on `*_test.go` files.
**Root cause**: the analysis phase (`analyze.go`) looks for a struct that
implements the translator interface. Test files define no such struct; the
analysis returns an error immediately and nothing is emitted.
**Impact**: all six `_test.go` output files had to be written from scratch.
**Fix needed**: detect `_test.go` inputs and apply a lighter-weight rewrite
(import-path substitution + signature adaptation only, no struct analysis).

### 2. Wrong Extra key for Anthropic-version providers

**Affected files (generated)**: `openai_awsanthropic.go`, `openai_gcpanthropic.go`.
**Root cause**: `transform.go` maps `apiVersion` struct fields to
`cfg.Extra["azure_api_version"]` regardless of provider. For Anthropic
providers the correct key is `"anthropic_version"`.
**Impact**: both files had a MANUAL-FIX to correct the key.
**Fix needed**: in `transform.go`, detect provider name from the source filename
and map `apiVersion` → `"anthropic_version"` for `*awsanthropic` and
`*gcpanthropic` sources.

### 3. Missing `apischema/gcp` package

**Root cause**: the codemod rewrites `github.com/envoyproxy/ai-gateway/internal/apischema/gcp`
to `github.com/dio/transit/examples/orange/internal/apischema/gcp`, but that
package did not exist in the orange tree.
**Impact**: `openai_gcpvertexai.go` failed to compile until the package was
created by copying from ai-gateway.
**Fix needed**: the `sync-apischema` codemod mode (described in these notes
under "Extended Sync Scope") must be run before `transform-translators`; or the
codemod's pre-flight check should error with a clear message when a required
apischema package is absent.

### 4. `json.Marshaler` interface vs function type

**Affected file (generated)**: `openai_gcpvertexai.go`.
**Root cause**: `ai-gateway/internal/json` defines `Marshaler` as a function
type (`type Marshaler func(v any) ([]byte, error)`). `transform.go` replaces
the import with `encoding/json`, but `encoding/json.Marshaler` is an interface,
not a function type. Code that calls `json.Marshaler(someFunc)` as a type
conversion breaks.
**Impact**: `openai_gcpvertexai.go` required a manual fix to introduce a local
`jsonMarshalerFunc` function type.
**Fix needed**: `transform.go` should recognize uses of `internal/json.Marshaler`
as a function type and either inline the function type or emit a local alias
rather than substituting `encoding/json.Marshaler`.

### 5. Token-usage stub drops leave orphaned bare statements (syntax errors)

**Affected files (generated)**: `openai_awsbedrock.go`, `openai_gcpvertexai.go`.
**Root cause**: when `transform.go` removes a multi-return call such as:
```go
usage, err = metrics.ExtractTokenUsageFromExplicitCaching(a, b, c, d)
```
it deletes the `metrics.*` call but leaves the surrounding assignment and its
arguments as bare statements, which are not valid Go.
**Impact**: both files had syntax errors that had to be manually excised.
**Fix needed**: `transform.go` should detect assignment statements whose RHS is
a `metrics.*` call and remove the entire assignment statement (including LHS
variables), or replace it with a `_ = …` blank discard that is at least
syntactically valid.

---

## Codemod Phase 4 — Decisions and Changes (2026-06-03)

### `modelNameOverride` → `backendModel` rename

The generated translator struct fields and their internal references have been
renamed from `modelNameOverride` to `backendModel` to match `ProviderConfig.BackendModel`.
The codemod now does this via a `strings.ReplaceAll` in `rewriteContent` (step 10).

**Scope**: applies only to the generated `openai_*.go` translator files. Helper
files (`openai_helper.go`, `anthropic_helper.go`) retain `modelNameOverride` as
a local function parameter name — this is independent and correct.

All six already-generated translator files (`openai_openai.go`,
`openai_awsbedrock.go`, `openai_awsanthropic.go`, `openai_gcpanthropic.go`,
`openai_azureopenai.go`, `openai_gcpvertexai.go`) have been updated in place.

### `sync-translators` mode added to codemod

New mode: `go run ./examples/orange/codemod -mode sync-translators -upstream
/path/to/ai-gateway -out-root .`

Discovers all non-helper, non-test `openai_*.go` files in
`ai-gateway/internal/translator/` and runs the full transform pipeline on each.
Replaces the need to invoke `-mode transform` once per file. Errors are
collected and reported at the end; partial output is still written so `CODEMOD-TODO`
markers can be inspected even when goimports reports syntax errors.

### `anthropic_usage.go` excluded from `sync-helpers`

`anthropic_usage.go` (and its test) removed from `helperFiles` in `syncpkg.go`
and deleted from `examples/orange/internal/translator/`. Reasons:

1. The ai-gateway source file is effectively empty (package declaration only).
2. Token usage extraction is owned by `pipeline/meter`, which runs as an
   independent Envoy filter and uses a head+tail ring buffer over the raw SSE or
   JSON response stream — no translator involvement needed.
3. The companion `anthropic_usage_test.go` had all tests skipped with
   `CODEMOD-TODO` markers since the underlying `metrics.*` calls were dropped.

### Request body type detection (`RequestBodyParamType`)

`injectBodyParse` previously hardcoded `openai.ChatCompletionRequest` as the
body variable type, which would produce incorrect code for embeddings
(`openai.EmbeddingRequest`), completions, and responses files.

`TranslatorShape` now carries a `RequestBodyParamType string` field, populated by
`extractRequestBodyParamType` in `analyze.go`. The function looks for any
`openai.*Request`-shaped parameter in `RequestBody`'s param list; falls back to
`openai.ChatCompletionRequest`. `injectBodyParse` uses this type directly.

---

## Observability in Original Translators

What the ai-gateway translators carry that the codemod drops:

| Concern | ai-gateway mechanism | Codemod action | Orange equivalent |
|---|---|---|---|
| **Token usage** | `metrics.TokenUsage` 4th return value from `ResponseBody`; `metrics.ExtractTokenUsageFromExplicitCaching` in response parse | Dropped; `CODEMOD-TODO` comment inserted | `pipeline/meter` — independent Envoy filter scanning SSE/JSON |
| **Tracing** | `tracingapi.ChatCompletionSpan` parameter to `ResponseBody`; `span.RecordXxx(...)` calls | Dropped entirely | Not yet wired; will use Envoy-native trace headers |
| **Logs** | `*slog.Logger` struct field; `o.logger.Debug(...)` in response body; controlled by `SetRedactionConfig` | Dropped entirely | Not yet wired; will route through `up.Logger` |

None of these should be re-introduced into the translator layer. They belong in
the filter layer (`pipeline/meter`, future pipeline/trace, future pipeline/log).

### Metrics detail

**`metrics.TokenUsage`** (`internal/metrics/metrics.go`)

A struct with 6 token-count fields, each with an accompanying "was it set" boolean
so callers can distinguish zero from absent:

| Field | Meaning |
|---|---|
| `inputTokens` | Tokens consumed from the request prompt |
| `outputTokens` | Tokens generated in the response |
| `totalTokens` | Sum of input + output (set explicitly by some backends) |
| `cachedInputTokens` | Prompt tokens served from the KV cache (cache read) |
| `cacheCreationInputTokens` | Tokens written to cache (Anthropic prompt caching) |
| `reasoningTokens` | Extended thinking / chain-of-thought tokens (Claude 3.7+, o1+) |

**`metrics.ExtractTokenUsageFromExplicitCaching`**

```go
func ExtractTokenUsageFromExplicitCaching(
    inputTokens, outputTokens int64,
    cacheReadTokens, cacheCreationTokens *int64,
) TokenUsage
```

Used by Anthropic and AWS Bedrock translators (where caching is explicit in the
response body). Computes:

```
total_input = input_tokens + cache_creation_input_tokens + cache_read_input_tokens
```

This normalizes the usage report so that callers always see the true total input
cost regardless of whether tokens were served from cache or freshly computed.
The raw cache sub-fields are preserved in `CachedInputTokens` and
`CacheCreationInputTokens` for cost attribution.

The returned `TokenUsage` is passed to `RecordTokenUsage(ctx, usage, reqHeaders)`
which emits these OTel metrics (GenAI semantic conventions):

| OTel metric | Type | Unit |
|---|---|---|
| `gen_ai.client.token.usage` | Histogram | tokens |
| `gen_ai.server.request.duration` | Histogram | seconds |
| `gen_ai.server.time_to_first_token` | Histogram | seconds |
| `gen_ai.server.time_per_output_token` | Histogram | seconds |

Labels include `gen_ai.system` (provider name), `gen_ai.request.model`,
`gen_ai.response.model`, `gen_ai.operation.name`.

**Orange's `pipeline/meter` vs ai-gateway metrics**

Orange uses Envoy native counters (`orange_input_tokens`, `orange_output_tokens`)
via the dynamic module ABI — no OTel SDK dependency, zero added latency, emitted
directly into Envoy's stats subsystem. It also writes dynamic metadata
(`orange_meter.input_tokens`, `orange_meter.output_tokens`) so other filters
can read usage without an OTel collector.

Trade-off: orange loses per-model labels, latency histograms, TTFT, and cache
sub-field attribution that ai-gateway provides. These can be added later via
`up.SetMetadata` + a future `pipeline/trace` filter.

### Tracing detail

**`tracingapi.ChatCompletionSpan`** (`internal/tracing/tracingapi/api.go`)

A generic interface `Span[openai.ChatCompletionResponse, openai.ChatCompletionStreamResponse]`
passed to `ResponseBody`. Used to record span attributes in OpenInference format:

- Request attributes (model name, messages, temperature) — recorded on the request path
- Response attributes (response model, finish reason, token counts) — recorded in `ResponseBody`
- Streaming: `RecordResponseChunks` accumulates partial chunks; attributes written on `endOfStream`

Backed by `go.opentelemetry.io/otel/trace.Span`. ai-gateway ships an
OpenInference attribute mapper under `internal/tracing/openinference/`.

Orange has no equivalent yet. The hook points are the `CODEMOD-TODO` comment
locations in generated files where `span.RecordXxx(...)` calls were dropped.

### Logs / Redaction detail

**`*slog.Logger` + `SetRedactionConfig`**

Every ai-gateway translator struct carries a `*slog.Logger` field (nil by default,
set via `SetRedactionConfig`). Used for a single debug log in `ResponseBody`:

```go
o.logger.Debug("response body processing", slog.Any("response", string(jsonBody)))
```

`SetRedactionConfig(debugLogEnabled, enableRedaction bool, logger *slog.Logger)`
also wires the `redaction` package, which sanitizes PII from request and response
body fields (message content, tool arguments) when `enableRedaction=true`. The
redaction pass runs before the debug log is written.

The codemod drops `SetRedactionConfig`, `RedactBody`, and `RedactAnthropicBody`
entirely. Orange does not currently log response bodies. If needed, route through
`up.Logger` (Envoy-native) and perform redaction at the filter level, not inside
the translator.

---

## AST-based Generation vs Text-level Transforms

The codemod uses text-level transforms (with `go/ast` for structural metadata
only) rather than full AST mutation + `go/printer`. This is intentional:
`go/printer` ties comment positions to byte offsets; structural AST mutations
(dropping fields, changing return types, reordering nodes) cause comments to
drift or disappear entirely.

The clean alternative would be `github.com/dave/dst` (Decorated Syntax Tree),
which attaches comments to nodes rather than byte positions and survives
structural edits. That would enable:

1. Parse → `dst.File`
2. Walk and mutate nodes (drop fields, rename identifiers, change signatures)
3. `dstprinter.Fprint` → clean output with comments intact

Reach for `dst` if the text transforms start producing invalid code on new
ai-gateway files regularly. For the current scope (six chat-completion
translators + embeddings/completions/responses), text transforms are adequate.
