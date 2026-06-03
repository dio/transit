# Orange Translator Design

This document defines the shape of the `Translator` abstraction in
`examples/orange/internal/translator`, the rationale for each design decision,
and the plan for the codemod that mechanically imports translators from
`ai-gateway/internal/translator`.

---

## Context

Orange is an OpenAI-first proxy: every client speaks OpenAI's API
(`/v1/chat/completions`). Orange must forward requests to heterogeneous
backends — native OpenAI, Azure OpenAI, Anthropic, AWS Bedrock (Converse and
native Anthropic), GCP Vertex AI (Gemini and Anthropic) — each with its own:

- **Path shape** — including model-in-path like GCP:
  `publishers/google/models/gemini-2.5-pro:generateContent`
- **Body schema** — Bedrock uses a different JSON envelope; Anthropic renames
  fields; GCP Gemini uses `GenerateContentRequest`
- **Request headers** — `anthropic-version`, `api-version`, AWS sigv4 headers
- **Response schema** — must be translated back to OpenAI format
- **Streaming framing** — OpenAI/Anthropic use SSE; AWS Bedrock uses binary
  Amazon EventStream; GCP Vertex wraps SSE differently

Auth credential injection (Bearer, API key, Anthropic headers) is already
handled by `auth.go` and stays separate. Translators own everything else.

---

## Translator Interface

```go
// internal/translator/translator.go

package translator

// Header is a name/value header pair. Use ":path", ":method", "content-length",
// "content-type" etc. for pseudo-headers and control headers.
type Header struct {
    Name  string
    Value string
}

// Translator handles the full request/response lifecycle for one backend provider.
// A single Translator instance is created per request; it may accumulate
// streaming state across multiple ResponseBody calls.
//
// Auth credential injection is NOT the translator's concern — auth.go handles
// that in a separate step after RequestHeaders returns.
type Translator interface {
    // RequestHeaders is called with the decoded incoming request headers before
    // the body is available. Translators may record metadata (e.g., streaming
    // flag from Accept or a custom header) but must NOT produce :path here
    // because the model name (needed for GCP/Bedrock paths) lives in the body.
    //
    // Returns headers to add/override on the upstream request.
    RequestHeaders(headers map[string]string) ([]Header, error)

    // RequestBody translates the OpenAI-format request body to the backend
    // format. This is where :path MUST be set (it can embed the model name),
    // along with content-length and content-type when the body is mutated.
    //
    // raw is the complete, buffered request body.
    // Returns mutated headers and the new body. If mutatedBody is nil, the
    // original body is forwarded unchanged.
    RequestBody(raw []byte) (newHeaders []Header, mutatedBody []byte, err error)

    // ResponseHeaders is called with the upstream response headers before any
    // body bytes arrive. Use this to rewrite content-type when changing the
    // streaming framing (e.g., Amazon EventStream → text/event-stream).
    ResponseHeaders(headers map[string]string) ([]Header, error)

    // ResponseBody is called once per body chunk for streaming responses, or
    // once with the full body for non-streaming responses. endOfStream is true
    // on the final (or only) call.
    //
    // chunk may be empty on intermediate calls (Envoy heartbeat). The
    // Translator must buffer partial frames internally when framing boundaries
    // do not align with chunk boundaries (e.g., Amazon EventStream).
    //
    // Returns header mutations (only meaningful on the first call or when
    // endOfStream is true) and the translated body bytes to emit downstream.
    // Return mutatedBody = []byte{} (non-nil empty slice) to suppress the raw
    // upstream bytes without emitting anything — important for streaming
    // translators that emit a reframed event later.
    ResponseBody(chunk []byte, endOfStream bool) (newHeaders []Header, mutatedBody []byte, err error)
}
```

### Why path mutation belongs in RequestBody

GCP Vertex AI paths embed the model name:

```
/v1/projects/PROJECT_ID/locations/us-central1/publishers/google/models/gemini-2.5-pro:generateContent
```

AWS Bedrock paths also embed the model:

```
/model/amazon.titan-text-express-v1/converse
```

The model name lives in the JSON body (`"model": "..."`) or in config
(`ProviderConfig.BackendModel`). It is not available during `RequestHeaders`. By always
setting `:path` in `RequestBody`, path mutation is uniform across all
providers — even passthrough providers still set it for consistency.

### Why streaming state is per-instance

Amazon EventStream uses binary length-prefixed framing. A single EventStream
message may span multiple Envoy body chunks. A per-request Translator instance
can keep a `[]byte` internal buffer and a decode cursor without any locking,
because extproc callbacks for a single stream are serial.

---

## Provider Configuration

```go
// internal/translator/config.go

// ProviderConfig carries all static, per-upstream configuration that a
// Translator needs at construction time. Dynamic per-request data (e.g., the
// actual model name) is extracted from the request body inside RequestBody.
type ProviderConfig struct {
    // BackendSchema identifies the translator to instantiate.
    // Registry key: "openai", "anthropic", "awsbedrock", "awsanthropic",
    // "azureopenai", "gcpvertexai", "gcpanthropic".
    BackendSchema string

    // PathPrefix is the upstream base path, e.g. "/v1" for OpenAI,
    // "/openai/deployments" for Azure. Set to "" to use the provider default.
    PathPrefix string

    // BackendModel replaces the "model" field in every request body.
    // Leave empty to forward the client-supplied model name unchanged.
    BackendModel string

    // Extra holds provider-specific knobs.
    // Common keys:
    //   "azure_api_version"     — Azure OpenAI api-version query param
    //   "anthropic_version"     — anthropic-version header value (GCP/AWS)
    //   "aws_region"            — AWS region for SigV4 signing
    //   "gcp_project_id"        — GCP project ID for Vertex AI paths
    //   "gcp_location"          — GCP region, e.g. "us-central1"
    Extra map[string]string
}
```

---

## Registry

```go
// internal/translator/registry.go

// Factory constructs a new Translator for a single request.
// cfg is read-only; the returned Translator may keep mutable streaming state.
type Factory func(cfg ProviderConfig) Translator

var registry = map[string]Factory{}

// Register associates name with factory f. Called from provider init() functions.
func Register(name string, f Factory) {
    if _, dup := registry[name]; dup {
        panic("translate: duplicate provider registration: " + name)
    }
    registry[name] = f
}

// New returns a fresh Translator for the named provider.
func New(name string, cfg ProviderConfig) (Translator, error) {
    f, ok := registry[name]
    if !ok {
        return nil, fmt.Errorf("translate: unknown provider %q", name)
    }
    return f(cfg), nil
}
```

---

## File Layout

```
examples/orange/internal/translator/
├── translator.go          — Translator interface + Header type (orange's simplified non-generic variant)
├── config.go              — ProviderConfig
├── registry.go            — Register / New
├── openai_openai.go       — OpenAI→OpenAI passthrough
├── openai_awsbedrock.go   — OpenAI→AWS Bedrock Converse (EventStream)
├── openai_awsanthropic.go — OpenAI→AWS Bedrock native Anthropic
├── openai_azureopenai.go  — OpenAI→Azure OpenAI (path only, body passthrough)
├── openai_gcpvertexai.go  — OpenAI→GCP Vertex AI Gemini
├── openai_gcpanthropic.go — OpenAI→GCP Vertex Anthropic
├── openai_helper.go       — OpenAI→Anthropic/Gemini field mapping (copied verbatim)
├── anthropic_helper.go    — Anthropic↔OpenAI field mapping (copied verbatim)
├── gemini_helper.go       — Gemini-specific conversions (copied verbatim)
├── jsonschema_helper.go   — JSON Schema→Gemini Schema (copied verbatim)
└── util.go                — SSE helpers + sjson option sets (copied verbatim)
                             NOTE: anthropic_usage.go intentionally excluded —
                             token usage extraction is owned by pipeline/meter.
```

The layout mirrors `ai-gateway/internal/translator/` exactly — one flat package,
`{client}_{backend}.go` naming throughout. Each provider file has an `init()` that
calls `Register(...)` from within the same `translator` package. No blank imports
are needed since all files share a single package.

---

## How translate.go Wires It

```
Request flow (Envoy extproc phases):

  [RequestHeaders phase]
    1. Read upstream name from dynamic metadata ("orange.upstream")
    2. Load ProviderConfig from config.Get(upstream)
    3. t = translator.New(cfg.BackendSchema, cfg)   ← per-request instance
    4. hdrs, err = t.RequestHeaders(incomingHeaders)
    5. Apply hdrs to up.Writer
    6. auth.InjectAuth(w)    ← Bearer/APIKey/Anthropic; AWSAuth is a no-op here

  [RequestBody phase]
    7.  newHdrs, body, err = t.RequestBody(raw)
    8.  Apply newHdrs (includes :path, content-length, content-type)
    9.  effectiveBody = body if body != nil, else raw
    10. If auth implements BodyAwareAuthHandler:       ← AWSAuth signs here
            auth.InjectAuthWithBody(w, SigningRequest{
                Method: "POST",
                Path:   pathFromHeaders(newHdrs),
                Host:   upstreamHost,
                Body:   effectiveBody,
            })
    11. Replace request body with effectiveBody (if mutated)

  [ResponseHeaders phase]
    12. newHdrs, err = t.ResponseHeaders(upstreamHeaders)
    13. Apply newHdrs (e.g., content-type: text/event-stream)

  [ResponseBody phase — called 1..N times]
    14. newHdrs, out, err = t.ResponseBody(chunk, eos)
    15. Apply newHdrs if non-nil
    16. If out != nil, replace chunk with out
```

The Translator instance (`t`) must be stored in the filter's per-stream state
between phases. The `up` package provides a context or state bag for this.

---

## Codemod: `examples/orange/codemod`

### Goal

Mechanically transform each `openai_{backend}.go` file in
`ai-gateway/internal/translator/` into a ready-to-compile
`examples/orange/internal/translator/openai_{backend}.go` file.
"Ready-to-compile" means it compiles with at most `TODO` comments where
human judgment is required (e.g., metric recording, tracing). Logic that
compiles and works correctly is the target; no half-finished stubs.

### Program outline

```
examples/orange/codemod/
├── main.go
├── analyze.go      — parse source file, extract translator shape
├── transform.go    — rewrite AST nodes
├── emit.go         — render output file
└── testdata/       — golden input/output pairs for unit tests
```

### Phase 1 — Analysis (`analyze.go`)

For a given `openai_{backend}.go` input file:

1. `go/parser.ParseFile` with `parser.ParseComments`.
2. Find the primary translator struct: the one that implements
   `Translator[openai.ChatCompletionRequest, ...]`.
3. Collect its four methods: `RequestBody`, `ResponseHeaders`, `ResponseBody`,
   `ResponseError`.
4. Find the constructor: `func New*(...) OpenAIChatCompletionTranslator`.
5. Collect the struct fields (to know what config knobs the provider needs).
6. Record all imports used by those methods.

```go
type TranslatorShape struct {
    StructName     string
    ConstructorName string
    Fields         []FieldInfo     // struct fields with types
    RequestBodyFn  *ast.FuncDecl
    ResponseHeadersFn *ast.FuncDecl
    ResponseBodyFn *ast.FuncDecl
    ResponseErrorFn *ast.FuncDecl
    Imports        []*ast.ImportSpec
}
```

### Phase 2 — Transformation (`transform.go`)

Apply these mechanical rewrites using `golang.org/x/tools/go/ast/astutil`:

| Source pattern | Orange equivalent |
|---|---|
| `func (t *S) RequestBody(raw []byte, body *openai.ChatCompletionRequest, flag bool) ([]internalapi.Header, []byte, error)` | `func (t *S) RequestBody(raw []byte) ([]Header, []byte, error)` — add `var body openai.ChatCompletionRequest; _ = json.Unmarshal(raw, &body)` at top of body; drop `flag` |
| `func (t *S) ResponseBody(respHeaders map[string]string, body io.Reader, endOfStream bool, span tracingapi.ChatCompletionSpan) ([]internalapi.Header, []byte, metrics.TokenUsage, internalapi.ResponseModel, error)` | `func (t *S) ResponseBody(chunk []byte, endOfStream bool) ([]Header, []byte, error)` — replace `body io.Reader` usage with `bytes.NewReader(chunk)`; drop `span`, `tokenUsage`, `responseModel` returns |
| `func (t *S) ResponseHeaders(headers map[string]string) ([]internalapi.Header, error)` | `func (t *S) ResponseHeaders(headers map[string]string) ([]Header, error)` — signature-compatible, just replace import |
| `internalapi.Header{Name: k, Value: v}` | `Header{Name: k, Value: v}` |
| `metrics.TokenUsage{...}` refs | Delete (or add `// TODO: wire token usage`) |
| `tracingapi.*` refs | Delete |
| `RequestHeadersSetter` implementation (`SetRequestHeaders`) | Move extracted header values into constructor args; add to `ProviderConfig.Extra` mapping in a `// TODO` comment |
| `sjsonOptions` / `sjsonOptionsInPlace` | Already present in `util.go` (copied verbatim); no action needed |
| `New*Translator(modelNameOverride ...string)` constructor | Add `cfg ProviderConfig` as first arg; wire `cfg.BackendModel` and `cfg.Extra["..."]` to struct fields; rename internal `modelNameOverride` fields to `backendModel` |
| `ResponseError` method | Keep as unexported helper `func (t *S) responseError(...)` called from `ResponseBody` when `len(chunk) > 0 && isErrorStatus(...)` (add `// TODO: detect error status from stored response headers`) |

**Special case — `RequestHeadersSetter`:**

Some translators implement `SetRequestHeaders(headers map[string]string)` to
extract a request header (e.g., `anthropic-beta`) and store it for use in
`RequestBody`. In orange this is handled by `RequestHeaders`:

```go
// Generated output
func (t *S) RequestHeaders(headers map[string]string) ([]Header, error) {
    // TODO: extracted from SetRequestHeaders in source
    t.anthropicBeta = headers["anthropic-beta"]
    return nil, nil
}
```

The codemod detects `SetRequestHeaders` implementations and emits the body as
`RequestHeaders`, adding the appropriate struct field if missing.

### Phase 3 — Emit (`emit.go`)

1. Set output package name to `translator` (all files share the flat package).
2. Collect required imports: subtract removed ones (`internalapi`, `metrics`,
   `tracingapi`), add `bytes` and `encoding/json`. No `translator` package import
   is needed — the output file lives in the same package.
3. Add `init()` function. Because the generated file lives in the same `translator`
   package as `registry.go`, no package qualifier is needed:
   ```go
   func init() {
       Register("awsbedrock", func(cfg ProviderConfig) Translator {
           return NewChatCompletionOpenAIToAWSBedrockTranslator(cfg)
       })
   }
   ```
4. Write via `go/printer` then run `goimports` on the output.

### Invocation

```bash
# Sync API schema structs (run first):
go run ./examples/orange/codemod -mode sync-apischema \
    -upstream /path/to/ai-gateway -out-root .

# Sync shared helper utilities:
go run ./examples/orange/codemod -mode sync-helpers \
    -upstream /path/to/ai-gateway -out-root .

# Transform ALL openai_*.go translator files at once (recommended):
go run ./examples/orange/codemod -mode sync-translators \
    -upstream /path/to/ai-gateway -out-root .

# Transform a single file (for debugging or targeted regeneration):
go run ./examples/orange/codemod -mode transform \
    -src /path/to/ai-gateway/internal/translator/openai_awsbedrock.go \
    -out examples/orange/internal/translator/openai_awsbedrock.go
```

`sync-translators` discovers all non-helper, non-test `openai_*.go` files in the
upstream `internal/translator/` directory and runs the full transform pipeline on
each. Existing files are overwritten; the operation is idempotent.

### Files the codemod explicitly does NOT handle

These require human authoring:

| File | Reason |
|---|---|
| `openai_awsbedrock.go` streaming `ResponseBody` | Amazon EventStream binary framing is deeply stateful; generated stub compiles but has `// TODO: implement EventStream decode` |
| `openai_gcpvertexai.go` SSE framing | Multiple delimiter variants (`\r\r`, `\r\n\r\n`) need manual verification |
| AWS SigV4 signing | Not present in ai-gateway translators at all (done by a sidecar); needs a separate `awscredentials` helper |
| `openai_awsbedrock_embeddings.go`, `openai_azureopenai_embeddings.go`, `openai_gcpvertexai_embeddings.go`, `openai_embeddings.go` | Embeddings not in orange's initial scope; codemod skips `*_embeddings.go` |
| `openai_speech.go`, `openai_responses.go`, `openai_completions.go` | Non-chat-completion operations out of scope; codemod skips these |
| `anthropic_*.go`, `cohere_*.go`, `imagegeneration_*.go` | Non-OpenAI client formats; orange is OpenAI-first and skips these entirely |

The codemod emits `// CODEMOD-TODO:` markers at every location it could not
mechanically transform, so `grep CODEMOD-TODO` gives the exact review list.

---

## Shared Utilities to Copy

Before running the codemod, copy these verbatim from `ai-gateway/internal/translator/`
into `examples/orange/internal/translator/` (same filenames, same package).
No interface rewrites are needed — they are pure conversion logic:

| Source in ai-gateway | Target in orange | What it is |
|---|---|---|
| `util.go` | `internal/translator/util.go` | SSE helpers + sjson option sets |
| `openai_helper.go` | `internal/translator/openai_helper.go` | OpenAI→Anthropic/Gemini field mapping |
| `anthropic_helper.go` | `internal/translator/anthropic_helper.go` | Anthropic↔OpenAI field mapping |
| `gemini_helper.go` | `internal/translator/gemini_helper.go` | Gemini-specific conversions |
| `jsonschema_helper.go` | `internal/translator/jsonschema_helper.go` | JSON Schema→Gemini Schema |

`anthropic_usage.go` is intentionally excluded: the ai-gateway source file is
empty (package declaration only) and token usage extraction is owned by
`pipeline/meter` in orange.

After copying, adjust the `package` declaration and any internal import paths.
No other changes are required.

---

## Test Files

Every translator and helper in ai-gateway ships with a `*_test.go` companion.
The decision per category:

| Test file category | Decision |
|---|---|
| Helper tests: `util_test.go`, `openai_helper_test.go`, `anthropic_helper_test.go`, `gemini_helper_test.go`, `jsonschema_helper_test.go` | **Copy verbatim** alongside the helpers. They cover pure conversion logic with no interface dependencies — same rationale as copying the helpers themselves. |
| `anthropic_usage_test.go` | **Skip** — paired with excluded `anthropic_usage.go`. |
| In-scope translator tests: `openai_openai_test.go`, `openai_azureopenai_test.go`, `openai_awsbedrock_test.go`, `openai_awsanthropic_test.go`, `openai_gcpvertexai_test.go`, `openai_gcpanthropic_test.go` | **Codemod-transform** them in parallel with their `.go` counterparts. The same interface rewrites (`internalapi.Header` → `Header`, drop `metrics`/`tracingapi` args, etc.) apply to test setup code. The codemod runs over `openai_*_test.go` files using the same rewrite rules; tests that exercise removed return values (token usage, response model, spans) get a `// CODEMOD-TODO: re-enable when feature wired` skip. |
| Out-of-scope translator tests: `openai_awsbedrock_embeddings_test.go`, `openai_azureopenai_embeddings_test.go`, `openai_gcpvertexai_embeddings_test.go`, `openai_embeddings_test.go`, `openai_completions_test.go`, `openai_speech_test.go`, `openai_responses_test.go` | **Skip** — paired with their out-of-scope `.go` files. |
| Non-OpenAI client tests: `anthropic_*_test.go`, `cohere_*_test.go`, `imagegeneration_*_test.go` | **Skip** — paired with their out-of-scope `.go` files. |

The codemod's `-pattern` flag matches `openai_*.go` and `openai_*_test.go`
together, so a single batch run produces both production code and its tests in
the same package.

---

## File Coverage Audit

Cross-checked against the current `ai-gateway/internal/translator/` directory
(53 files total). Every file has an explicit decision:

| Group | Files | Decision |
|---|---|---|
| Interface | `translator.go` | Replaced by orange's hand-written `translator.go` |
| Shared helpers | `util.go`, `openai_helper.go`, `anthropic_helper.go`, `gemini_helper.go`, `jsonschema_helper.go` (+ their `_test.go`) | Copy verbatim via `sync-helpers` |
| Excluded helper | `anthropic_usage.go` (+ `_test.go`) | Skip — empty source; token usage owned by `pipeline/meter` |
| In-scope translators | `openai_openai.go`, `openai_azureopenai.go`, `openai_awsbedrock.go`, `openai_awsanthropic.go`, `openai_gcpvertexai.go`, `openai_gcpanthropic.go` (+ their `_test.go`) | Codemod |
| Out-of-scope operations | `openai_completions.go`, `openai_embeddings.go`, `openai_responses.go`, `openai_speech.go`, `openai_awsbedrock_embeddings.go`, `openai_azureopenai_embeddings.go`, `openai_gcpvertexai_embeddings.go` (+ their `_test.go`) | Skip |
| Non-OpenAI clients | `anthropic_anthropic.go`, `anthropic_awsanthropic.go`, `anthropic_awsbedrock.go`, `anthropic_gcpanthropic.go`, `anthropic_openai.go`, `cohere_rerank_v2.go`, `imagegeneration_openai_openai.go` (+ their `_test.go`) | Skip |

---

## GCP Auth: Application Default Credentials

GCP Vertex AI (both Gemini and Anthropic endpoints) requires:

```http
Authorization: Bearer <short-lived-oauth-access-token>
```

Unlike static API keys, this token expires (typically 1 hour) and must be
refreshed automatically. The existing `BearerAuth{Token: staticString}` in
`auth.go` is not adequate for GCP.

### New handler: `GCPAuth`

Add to `auth.go`:

```go
// GCPAuth obtains a short-lived Bearer token via Application Default
// Credentials and injects it. Token refresh is handled by the underlying
// google.DefaultTokenSource, which caches and refreshes automatically.
type GCPAuth struct {
    // source is initialised once at startup via credentials.DetectDefault.
    source google.TokenSource
}

func NewGCPAuth(ctx context.Context, scopes ...string) (*GCPAuth, error) {
    ts, err := google.DefaultTokenSource(ctx, scopes...)
    if err != nil {
        return nil, err
    }
    return &GCPAuth{source: ts}, nil
}

func (a *GCPAuth) InjectAuth(w *up.Writer) {
    tok, err := a.source.Token()
    if err != nil || !tok.Valid() {
        return // let the upstream return a 401; orange can log/metric this
    }
    w.SetRequestHeader("authorization", "Bearer "+tok.AccessToken)
}
```

The required scope for Vertex AI is `https://www.googleapis.com/auth/cloud-platform`.

### ADC resolution order (no code required)

`google.DefaultTokenSource` checks these in order — orange needs zero explicit
configuration when running in the right environment:

| Environment | Credential source |
|---|---|
| Local development | `~/.config/gcloud/application_default_credentials.json` after `gcloud auth application-default login` |
| Cloud Run / GCE | Metadata server (instance service account) |
| GKE with Workload Identity | Metadata server bound to Kubernetes SA → GSA |
| Outside GCP | Workload Identity Federation (AWS IAM, Azure AD, OIDC) |
| Last resort | `GOOGLE_APPLICATION_CREDENTIALS` env var → service account JSON key |

### ProviderConfig wiring

In the orange config YAML, GCP providers specify no `secret` field. Instead,
`GCPAuth` is constructed once at startup and injected as the auth handler:

```yaml
upstreams:
  - name: vertex-gemini
    backend_schema: gcpvertexai
    extra:
      gcp_project_id: my-project
      gcp_location: us-central1
    # no secret: ADC is used automatically
```

`auth.go` selects `GCPAuth` when `backend_schema` is `gcpvertexai` or
`gcpanthropic` and no static secret is configured. If a static token is
explicitly provided in `secret`, `BearerAuth` is used instead (useful for
short-lived tokens injected by an external credential helper).

### Missing credentials UX

When `NewGCPAuth` fails at startup (no ADC found), orange should surface a
clear error:

```
orange: GCP Application Default Credentials not found for upstream "vertex-gemini".
Run: gcloud auth application-default login
Or set GOOGLE_APPLICATION_CREDENTIALS to a service account key file.
```

This matches the UX convention used by Terraform and other GCP-native tools.

---

## AWS Auth: SigV4 Signing

AWS Bedrock (both Converse and native Anthropic endpoints) requires every
request to be signed with AWS Signature Version 4:

```http
Authorization: AWS4-HMAC-SHA256 Credential=.../aws4_request,
               SignedHeaders=content-type;host;x-amz-date,
               Signature=<hex>
X-Amz-Date: 20260602T120000Z
X-Amz-Security-Token: <session-token>   # only for temporary credentials
```

### The body-hash problem

SigV4 includes a SHA-256 hash of the **request body** in the string-to-sign.
This means auth cannot be fully injected in `RequestHeaders` (phase 1) — the
translated body only exists after `RequestBody` (phase 2) runs.

`UNSIGNED-PAYLOAD` is a workaround (skip body hash) but AWS's own best-practice
guidance discourages it for non-streaming calls, and some Bedrock endpoints
reject it. Orange should sign the real payload.

### Solution: `BodyAwareAuthHandler` optional interface

Extend `auth.go` with a second optional interface:

```go
// BodyAwareAuthHandler is implemented by auth handlers that require the final
// request body to compute their credentials (e.g., AWS SigV4).
// The filter calls InjectAuthWithBody in the RequestBody phase, after the
// translator has produced the final body and :path.
type BodyAwareAuthHandler interface {
    InjectAuthWithBody(w *up.Writer, req SigningRequest) error
}

// SigningRequest carries everything a body-aware auth handler needs.
type SigningRequest struct {
    Method string // always "POST" for LLM APIs
    Path   string // final upstream path (after translator :path rewrite)
    Host   string // upstream hostname
    Body   []byte // final translated body (nil means original body passes through)
}
```

`AWSAuth` implements **both** `backendAuthHandler` (no-op `InjectAuth`) **and**
`BodyAwareAuthHandler` (`InjectAuthWithBody` does the actual signing). The
filter checks for the optional interface:

```go
// In the RequestBody phase, after translator returns:
if baw, ok := authHandler.(translate.BodyAwareAuthHandler); ok {
    req := translate.SigningRequest{
        Method: "POST",
        Path:   extractPath(newHdrs, storedRequestPath),
        Host:   upstreamHost,
        Body:   effectiveBody, // mutatedBody if non-nil, else raw
    }
    if err := baw.InjectAuthWithBody(w, req); err != nil {
        // surface error upstream
    }
}
```

### New handler: `AWSAuth`

```go
// AWSAuth signs requests with AWS Signature Version 4.
// InjectAuth is a no-op; all signing happens in InjectAuthWithBody.
type AWSAuth struct {
    creds   aws.CredentialsProvider // from aws-sdk-go-v2 config.LoadDefaultConfig
    region  string
    service string // "bedrock-runtime" for all Bedrock endpoints
    signer  *v4.Signer
}

func NewAWSAuth(ctx context.Context, region string) (*AWSAuth, error) {
    cfg, err := config.LoadDefaultConfig(ctx, config.WithRegion(region))
    if err != nil {
        return nil, err
    }
    return &AWSAuth{
        creds:   cfg.Credentials,
        region:  region,
        service: "bedrock-runtime",
        signer:  v4.NewSigner(),
    }, nil
}

func (a *AWSAuth) InjectAuth(_ *up.Writer) {} // no-op; signing needs body

func (a *AWSAuth) InjectAuthWithBody(w *up.Writer, req SigningRequest) error {
    creds, err := a.creds.Retrieve(context.Background())
    if err != nil {
        return err
    }
    u := &url.URL{Scheme: "https", Host: req.Host, Path: req.Path}
    hr, err := http.NewRequest(req.Method, u.String(), bytes.NewReader(req.Body))
    if err != nil {
        return err
    }
    if err := a.signer.SignHTTP(context.Background(), creds, hr,
        hexSHA256(req.Body), a.service, a.region, time.Now()); err != nil {
        return err
    }
    // Push signed headers back into the Envoy request
    w.SetRequestHeader("authorization", hr.Header.Get("Authorization"))
    w.SetRequestHeader("x-amz-date", hr.Header.Get("X-Amz-Date"))
    if tok := hr.Header.Get("X-Amz-Security-Token"); tok != "" {
        w.SetRequestHeader("x-amz-security-token", tok)
    }
    return nil
}

func hexSHA256(b []byte) string {
    h := sha256.Sum256(b)
    return hex.EncodeToString(h[:])
}
```

Import: `github.com/aws/aws-sdk-go-v2/aws/signer/v4` and
`github.com/aws/aws-sdk-go-v2/config`.

### AWS credential chain (no explicit config needed)

`config.LoadDefaultConfig` resolves credentials in this order:

| Environment | Credential source |
|---|---|
| Local development | `~/.aws/credentials` profile, or `AWS_ACCESS_KEY_ID` + `AWS_SECRET_ACCESS_KEY` env vars |
| EC2 / ECS | Instance metadata service (IMDS) |
| EKS with IRSA | Pod-scoped IAM role via OIDC (web identity token file) |
| Outside AWS | Cross-account role assumption via `AWS_ROLE_ARN` + `AWS_WEB_IDENTITY_TOKEN_FILE` |

Temporary credentials (STS `AssumeRole`, IRSA) are refreshed automatically by
the SDK.

### ProviderConfig wiring

```yaml
upstreams:
  - name: bedrock-claude
    backend_schema: awsbedrock
    extra:
      aws_region: us-east-1
    # no secret: SDK credential chain is used automatically
```

`auth.go` selects `NewAWSAuth(ctx, cfg.Extra["aws_region"])` when
`backend_schema` is `awsbedrock` or `awsanthropic`.

### Missing credentials UX

```
orange: AWS credentials not found for upstream "bedrock-claude" (region: us-east-1).
Set AWS_ACCESS_KEY_ID + AWS_SECRET_ACCESS_KEY, or configure an IAM role.
```

---

## Azure OpenAI Auth

Azure OpenAI supports two authentication methods:

**Static API key** — handled by the existing `APIKeyAuth` with `Header: "api-key"`:

```yaml
upstreams:
  - name: azure-gpt4o
    backend_schema: azureopenai
    secret: "<azure-api-key>"
    extra:
      azure_api_version: "2025-01-01-preview"
```

`auth.go` sets `api-key: <secret>` on every request. No new handler needed.

**AAD / Entra ID Bearer token** — for managed workloads (AKS, Azure VMs, CI/CD):

### New handler: `AzureAuth`

```go
// AzureAuth obtains an Entra ID Bearer token via DefaultAzureCredential and
// injects it. Token refresh is handled internally by the Azure SDK.
type AzureAuth struct {
    cred *azidentity.DefaultAzureCredential
}

func NewAzureAuth() (*AzureAuth, error) {
    cred, err := azidentity.NewDefaultAzureCredential(nil)
    if err != nil {
        return nil, err
    }
    return &AzureAuth{cred: cred}, nil
}

func (a *AzureAuth) InjectAuth(w *up.Writer) {
    tok, err := a.cred.GetToken(context.Background(), policy.TokenRequestOptions{
        Scopes: []string{"https://cognitiveservices.azure.com/.default"},
    })
    if err != nil {
        return // let upstream return 401; orange logs/metrics this
    }
    w.SetRequestHeader("authorization", "Bearer "+tok.Token)
}
```

Import: `github.com/Azure/azure-sdk-for-go/sdk/azidentity` and
`github.com/Azure/azure-sdk-for-go/sdk/azcore/policy`.

`GetToken` caches the token internally and refreshes before expiry — call it on
every request. `AzureAuth` implements only `backendAuthHandler` (no body signing),
matching the same shape as `GCPAuth`.

### AAD credential chain (no explicit config needed)

`DefaultAzureCredential` resolves credentials in this order:

| Environment | Credential source |
|---|---|
| Local development | `az login` (Azure CLI) or `azd auth login` |
| Service principal | `AZURE_CLIENT_ID` + `AZURE_TENANT_ID` + `AZURE_CLIENT_SECRET` or cert |
| AKS with Workload Identity | Federated workload identity via pod-injected OIDC token |
| Azure VM / Container Apps | Managed Identity (system-assigned or user-assigned) |

Set `AZURE_TOKEN_CREDENTIALS=prod` to restrict to env vars, workload identity,
and managed identity only (safe for production; prevents `az login` fallback).

### ProviderConfig wiring

```yaml
upstreams:
  - name: azure-gpt4o-aad
    backend_schema: azureopenai
    extra:
      azure_api_version: "2025-01-01-preview"
    # no secret: DefaultAzureCredential is used automatically
```

`auth.go` selects `NewAzureAuth()` when `backend_schema` is `azureopenai` and no
static `secret` is configured.

### Missing credentials UX

```
orange: Azure Entra ID credentials not found for upstream "azure-gpt4o-aad".
Run: az login
Or set AZURE_CLIENT_ID + AZURE_TENANT_ID + AZURE_CLIENT_SECRET for a service principal.
```

---

## Alibaba Cloud (DashScope / Model Studio) Auth

DashScope's OpenAI-compatible endpoint at
`dashscope-intl.aliyuncs.com/compatible-mode/v1` uses a plain Bearer token:

```http
Authorization: Bearer <dashscope-api-key>
```

There is no request signing scheme (no SigV4 equivalent). DashScope API keys
(`sk-…`) are issued directly by Model Studio and are independent of Alibaba
Cloud RAM/STS credentials — RAM AccessKey pairs cannot substitute for a
DashScope `sk-` key.

This means DashScope requires no new auth handler. Use the existing `BearerAuth`:

```yaml
upstreams:
  - name: dashscope-qwen
    backend_schema: dashscope
    secret: "${DASHSCOPE_API_KEY}"
```

`auth.go` selects `BearerAuth{Token: secret}` when `backend_schema` is
`dashscope`, injecting `Authorization: Bearer <key>`.

The standard environment variable is `DASHSCOPE_API_KEY` (set automatically by
all official Alibaba Cloud SDKs and LiteLLM). No credential chain or token
refresh is needed — the API key is static.

### Missing credentials UX

```
orange: DashScope API key not configured for upstream "dashscope-qwen".
Set the DASHSCOPE_API_KEY environment variable or provide a secret in config.
```

---

## Migration Order

1. **Copy helpers** — copy `util.go`, `openai_helper.go`, `anthropic_helper.go`,
   `gemini_helper.go`, `jsonschema_helper.go` verbatim into
   `examples/orange/internal/translator/` and adjust the `package` declaration.
   (`anthropic_usage.go` is intentionally excluded — see File Layout above.)
2. **Implement base interface** — write `translator.go`, `config.go`,
   `registry.go` by hand.
3. **Wire filter** — update `translate.go` to instantiate and drive the
   Translator per-request.
4. **Run codemod on passthrough providers** — `openai_openai.go`,
   `openai_azureopenai.go`. These are the simplest; verify golden output.
5. **Run codemod on Anthropic** — `openai_anthropic.go` (if it exists) or the
   combined helper files.
6. **Run codemod on AWS/GCP** — hand-complete the `// CODEMOD-TODO:` stubs for
   EventStream and Vertex SSE.
7. **E2E tests** — one integration test per provider using recorded
   request/response fixtures.
