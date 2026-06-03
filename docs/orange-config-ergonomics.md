# orange config ergonomics

Design for `orange.yaml`: provider definitions, model routing, the
`/v1/models` discovery endpoint, the auth handler registry, and the
contracts the pipeline relies on.

## Config shape

```yaml
providers:
  openai_direct:
    kind: openai
    endpoint: https://api.openai.com
    auth:
      type: bearer
      secret_ref: env://OPENAI_API_KEY

  anthropic_direct:
    kind: anthropic
    endpoint: https://api.anthropic.com
    extra:
      anthropic_version: "2023-06-01"
    auth:
      type: anthropic
      secret_ref: env://ANTHROPIC_API_KEY

  azure_gpt4o:
    kind: openai
    backend_schema: azureopenai
    endpoint: https://my-resource.openai.azure.com
    path_prefix: "/openai/deployments/my-deployment"
    extra:
      azure_api_version: "2025-01-01-preview"
    auth:
      type: bearer
      secret_ref: env://AZURE_OPENAI_KEY

  bedrock_claude:
    kind: anthropic
    backend_schema: awsanthropic
    endpoint: https://bedrock-runtime.us-west-2.amazonaws.com
    extra:
      aws_region: us-west-2
    auth:
      type: aws

  vertex_gemini:
    kind: openai
    backend_schema: gcpvertexai
    endpoint: https://us-central1-aiplatform.googleapis.com
    extra:
      gcp_project: my-project
      gcp_location: us-central1
    auth:
      type: gcp

  deepinfra:
    kind: openai
    endpoint: https://api.deepinfra.com
    path_prefix: "/v1/openai"
    auth:
      type: bearer
      secret_ref: env://DEEPINFRA_API_KEY

  groq:
    kind: openai
    endpoint: https://api.groq.com
    path_prefix: "/openai/v1"
    auth:
      type: bearer
      secret_ref: env://GROQ_API_KEY

models:
  gpt-4o:
    provider: openai_direct
  gpt-4.1:
    provider: openai_direct
  claude-3-5-sonnet-20241022:
    provider: anthropic_direct
  claude-sonnet:                       # client-facing alias
    provider: anthropic_direct
    name: claude-3-5-sonnet-20241022   # what the backend receives
  bedrock-claude-3-5-sonnet:
    provider: bedrock_claude
    name: anthropic.claude-3-5-sonnet-20241022-v2:0
  gemini-1.5-pro:
    provider: vertex_gemini
  deepinfra/microsoft/phi-4:           # provider-prefixed client ID
    provider: deepinfra
    name: microsoft/phi-4              # what DeepInfra's body expects
  deepinfra/meta-llama/Llama-3.3-70B-Instruct:
    provider: deepinfra
    name: meta-llama/Llama-3.3-70B-Instruct
  groq/llama-3.1-8b-instant:           # flat-name provider, still prefixed
    provider: groq
    name: llama-3.1-8b-instant
```

The file has exactly two top-level sections.

### `providers`

A map from provider name to provider definition. The name is referenced by
`models[].provider` and written into dynamic metadata as `orange.upstream`.

| Field | Required | Purpose |
|---|---|---|
| `kind` | yes | Client schema the gateway speaks for this provider (`openai`, `anthropic`). Determines which client API the provider serves. |
| `endpoint` | yes | Upstream URL. Hostname drives SNI selection and `:authority` rewrite. |
| `backend_schema` | no | Translator registry key. Defaults to `kind`. Set when the client schema differs from the backend wire format (e.g. `kind: openai, backend_schema: awsbedrock`). |
| `path_prefix` | no | Defaults to `/v1`. Set for non-standard providers (Azure, Bedrock, Vertex). |
| `extra` | no | Free-form string map. Both translators and auth handlers read keys they understand (`anthropic_version`, `aws_region`, `azure_api_version`, `gcp_project`, `gcp_location`). |
| `auth` | yes | Credential injection. See [Auth](#auth). |

### `models`

A map keyed by the model ID the client sends. Each entry resolves to a
provider and, optionally, a backend model name.

| Field | Required | Purpose |
|---|---|---|
| `provider` | yes | Provider name; must exist in `providers`. |
| `name` | no | Model name to send to the backend. Defaults to the map key. |
| `metadata` | no | Free-form bag emitted verbatim under `metadata` in `/v1/models`. See [Per-model `metadata`](#per-model-metadata). |

The map is the source of truth for both routing and `/v1/models`
advertising. Duplicate keys are a YAML parse error; references to unknown
providers are a config-load error.

Aliases are first-class: `claude-sonnet` and `claude-3-5-sonnet-20241022`
can both point at the same provider but resolve to different backend names.
The Bedrock entry above shows the reverse — a short client-facing ID
mapping to a long, version-pinned backend model ID.

**Compound IDs and provider prefixes.** Map keys are arbitrary strings;
slashes, dots, and colons are all fine. A common convention is to prefix
the client-facing ID with the provider name when the same upstream
model could plausibly be reached through multiple providers — for
example `deepinfra/microsoft/phi-4` versus a hypothetical
`together/microsoft/phi-4`. The prefix is purely a namespacing convention
chosen by the operator; orange does not parse or interpret it. The body
the backend receives is always `name:` if set, otherwise the map key
verbatim.

For DeepInfra-style providers that serve many upstream models through one
OpenAI-compatible endpoint, every model entry uses `name:` to drop the
gateway-side prefix:

```yaml
models:
  deepinfra/microsoft/phi-4:
    provider: deepinfra
    name: microsoft/phi-4              # body forwarded to DeepInfra
```

Client sends `{"model": "deepinfra/microsoft/phi-4", ...}`; the
`openai_openai` translator rewrites the body's `model` field to
`microsoft/phi-4` before forwarding. The client-facing ID continues to
appear in `/v1/models` and in `orange.model` metadata for telemetry; the
backend never sees the prefix.

### `auth`

```yaml
auth:
  type: bearer
  secret_ref: env://OPENAI_API_KEY
```

| Field | Required | Purpose |
|---|---|---|
| `type` | yes | Auth handler name; must be registered. |
| `secret_ref` | depends on handler | URI-style reference to a credential, resolved at config load. Currently only `env://NAME` is supported. |

`secret_ref` carries the redaction contract — tap, logs, and metrics skip
fields named `secret_ref`. Static-credential handlers require it; chain
handlers (`aws`, `gcp`) do not, because they discover credentials at runtime.

Handler-specific knobs (region, project, scope) live in `provider.extra`,
not on `auth`. This keeps the `Auth` struct minimal and consistent with how
translators already read schema-specific values from `extra`.

#### Built-in handlers

| `type` | Static fields | `provider.extra` fields it reads | Effect on request |
|---|---|---|---|
| `bearer` | `secret_ref` | — | `Authorization: Bearer <secret>` |
| `x-api-key` | `secret_ref` | — | `x-api-key: <secret>` |
| `anthropic` | `secret_ref` | `anthropic_version` | `x-api-key: <secret>` + `anthropic-version: <version>` |
| `aws` | — | `aws_region` (required) | AWS SigV4 signing in the body phase |
| `gcp` | — | — | ADC bearer token; refreshed by `cloud.google.com/go/auth` |

Adding a sixth handler is a single `auth.Register("name", factory)` call in
a new file under `internal/pipeline/adapt/auth/`. See
[Auth handler registry](#auth-handler-registry).

---

## Defaults

These values are constants in code, not config fields:

| Constant | Value |
|---|---|
| Model field in request body | `"model"` |
| Provider `path_prefix` | `/v1` |
| Unknown model status | `404` |
| Unknown model error code | `orange.model_not_found` |
| Missing-model error code | `orange.model_required` |
| Headers stripped from client request | `authorization`, `x-api-key`, `anthropic-version` |
| Body buffer cap | 1 MiB |

Each is deployment-invariant: every orange instance strips the same client
auth headers, every OpenAI-shaped backend reads `model` from the body, every
miss returns 404. Pushing them into YAML adds surface area without unlocking
a real use case.

---

## `/v1/models` endpoint

OpenAI-compatible clients call `GET /v1/models` to discover available
models. The minimal response orange emits, for entries with no extra
configuration:

```json
{
  "object": "list",
  "data": [
    { "id": "gpt-4o",                     "object": "model", "owned_by": "openai_direct" },
    { "id": "claude-3-5-sonnet-20241022", "object": "model", "owned_by": "anthropic_direct" },
    { "id": "claude-sonnet",              "object": "model", "owned_by": "anthropic_direct" },
    { "id": "gemini-1.5-pro",             "object": "model", "owned_by": "vertex_gemini" }
  ]
}
```

`id` is the map key (the client-facing name, never the backend `name`
override). `owned_by` is the resolved provider name. `created` is omitted
until a concrete client asks for it.

### Per-model `metadata`

The standard OpenAI shape covers `id`/`object`/`owned_by` (and historically
`created`). Several gateways extend it with richer per-model info — most
notably DeepInfra, whose [openai-models response][deepinfra-models] adds
`root`, `parent`, and a nested `metadata` carrying `description`,
`context_length`, `max_tokens`, `pricing`, and `tags`.

[deepinfra-models]: https://docs.deepinfra.com/api-reference/models/openai-models

orange supports a `metadata` block on each model entry as a free-form
passthrough. Whatever the operator writes is emitted verbatim under the
`metadata` key of that model's `/v1/models` entry:

```yaml
models:
  groq/llama-3.1-8b-instant:
    provider: groq
    name: llama-3.1-8b-instant
    metadata:
      description: "Llama 3.1 8B served by Groq for low-latency inference."
      context_length: 131072
      max_tokens: 8192
      tags: ["chat", "fast"]
```

Renders as:

```json
{
  "id": "groq/llama-3.1-8b-instant",
  "object": "model",
  "owned_by": "groq",
  "metadata": {
    "description": "Llama 3.1 8B served by Groq for low-latency inference.",
    "context_length": 131072,
    "max_tokens": 8192,
    "tags": ["chat", "fast"]
  }
}
```

orange does not enforce any schema on `metadata`; the operator decides
which conventions to follow. Matching DeepInfra's keys lets clients written
against DeepInfra read orange's response unchanged. Standard OpenAI clients
ignore unknown fields, so the addition is non-breaking.

`root` and `parent` are not first-class — operators who need them can put
them inside `metadata` or, if a concrete client requires them at the
top level, we add named fields to `ModelEntry`. Default: keep them out.

### Wiring

The `match` filter already intercepts every request and issues local
replies for routing errors. `GET /v1/models` is one more branch in
`requestHandler`:

```
requestHandler:
  GET  /v1/models              → serialize cfg.Models → SendLocalResponse 200
  POST /v1/chat/completions    → create StreamPromise, tag stream
  POST /v1/messages            → create StreamPromise, tag stream
  anything else                → pass through
```

The serializer iterates `cfg.Models` in deterministic order (sorted by
key), emits `{id, object, owned_by}` plus `metadata` when set, and is
built at request time so it stays in sync with config reloads.

No new filter, no new cluster, no Envoy route change.

---

## Pipeline contract: model name delivery

The model travels through the pipeline in two values, set by `match` and
read by `adapt`:

```
client body: {"model": "claude-sonnet", ...}
       │
       ▼ match (body phase)
  LookupModel("claude-sonnet") → ("anthropic_direct", "claude-3-5-sonnet-20241022")
  dynamic metadata:
    orange.upstream = "anthropic_direct"
    orange.model    = "claude-sonnet"                    ← client ID for telemetry
    orange.backend_model = "claude-3-5-sonnet-20241022"  ← what the backend sees
       │
       ▼ adapt (request-headers phase)
  translator.New(prov.EffectiveBackendSchema(), translator.ProviderConfig{
      BackendSchema: ...,
      PathPrefix:    ...,
      BackendModel:  "claude-3-5-sonnet-20241022",       ← from metadata
      Extra:         prov.Extra,
  })
       │
       ▼ translator
  Owns every body / header / path transformation. For OpenAI-shape backends
  it rewrites the body's "model" field to BackendModel. For Vertex/Bedrock
  it embeds BackendModel into :path. For Anthropic-on-Vertex it strips the
  body field and embeds in :path.
```

The pipeline never has to know whether the model travels in the body or the
path — that knowledge lives in one place per backend (the translator). The
contract `match` exposes is "here is the effective backend model name in
metadata"; the translator reads it from `cfg.BackendModel` at construction
time.

The field is named `BackendModel`, not `ModelNameOverride`, because with
per-model `name:` entries it is no longer overriding anything — it is the
resolved name to send.

---

## Request body field injection and enforcement

Translators sometimes need to add or validate body fields that the client
did not send. Two distinct cases:

### Gateway-internal enrichment

Some fields serve the gateway's own operation and should be injected
silently without exposing them to config.

**`stream_options.include_usage: true`** (OpenAI streaming requests): the
`meter` filter needs the usage object in the final SSE chunk to count
tokens. The `openai_openai` translator injects this on every streaming
request. The client gains nothing from knowing about it; no operator would
want to turn it off (they would lose token accounting). Hardcoded in the
translator, not surfaced to config.

Rule: if the injected value serves the gateway's own needs and a correct
invariant value exists → hardcode in the translator.

### Backend-required fields

Some backends require fields the client may omit. Three candidate
approaches:

| Approach | Tradeoff |
|---|---|
| Inject a default silently | Caps client output without their knowledge; wrong default is a silent runtime failure. |
| Surface to `provider.extra` | Honest, visible, overridable — but moves responsibility to the operator who may not know the right value either. |
| Validate and reject at match time | Surfaces the contract violation to the client where it belongs; a clear 400 is better than a silent cap or an opaque upstream error. |

**`max_tokens` for Vertex-hosted Anthropic**: the Anthropic Messages API
on Vertex requires `max_tokens`; requests without it are rejected. If the
client omits it, orange returns:

```json
HTTP 400
{
  "error": "request body is missing required field `max_tokens` for this backend",
  "code": "orange.missing_required_field"
}
```

No config knob. The contract is client-facing: any client that talks to an
Anthropic backend must send `max_tokens`. The gateway documents which
backends enforce this; it does not paper over client mistakes with silent
defaults.

Rule: if the missing field is a client API contract violation that the
backend will reject → validate at match time and return 400; do not inject
a default and do not add a config knob.

### Summary

| Field | Backend | Handled by | Surfaced to config? |
|---|---|---|---|
| `stream_options.include_usage` | OpenAI, Vertex | Translator — silently injected | No |
| `max_tokens` | Vertex Anthropic | match — validated, 400 on miss | No |

---

## Auth handler registry

Auth handlers register themselves at `init()` time:

```go
// internal/pipeline/adapt/auth/registry.go
package auth

import "fmt"

type Handler interface {
    InjectAuth(w *up.Writer)
}

type Factory func(a Auth, p config.Provider) (Handler, error)

var registry = map[string]Factory{}

func Register(name string, f Factory) {
    if _, dup := registry[name]; dup {
        panic("auth: duplicate registration: " + name)
    }
    registry[name] = f
}

func New(a Auth, p config.Provider) (Handler, error) {
    f, ok := registry[a.Type]
    if !ok {
        return nil, fmt.Errorf("auth: unknown type %q", a.Type)
    }
    return f(a, p)
}
```

Each handler ships its own file with an `init()`:

```go
// internal/pipeline/adapt/auth/aws.go
package auth

func init() {
    Register("aws", func(_ Auth, p config.Provider) (Handler, error) {
        region := p.Extra["aws_region"]
        if region == "" {
            return nil, fmt.Errorf("auth: aws requires extra.aws_region")
        }
        return newAWSAuth(context.Background(), region)
    })
}
```

This mirrors `internal/translator/registry.go` exactly. The handler owns
its own config validation; the pipeline knows only the `Handler` interface
and a name.

`BodyAwareAuthHandler` (currently used by AWS SigV4 for body-signed auth)
stays as an optional capability detected by type assertion in `adapt`:

```go
if baw, ok := h.(auth.BodyAwareHandler); ok {
    baw.InjectAuthWithBody(w, signingRequest)
}
```

This keeps the common case (`InjectAuth(w)` in the header phase) on the
narrow interface and lets body-aware handlers opt in.

### Decoupled axes

With the registry in place, `kind`, `backend_schema`, and `auth.type` move
independently:

- `kind` — what wire format the gateway accepts from the client
- `backend_schema` — which translator runs (defaults to `kind`)
- `auth.type` — which credentials the upstream receives

For example, a self-hosted OpenAI-compatible inference server that requires
AWS SigV4 is `kind: openai, backend_schema: openai, auth.type: aws`. None
of those combinations require special casing in core code.

---

## Go types

### `config.Config`

```go
type Config struct {
    Providers map[string]Provider   `yaml:"providers"`
    Models    map[string]ModelEntry `yaml:"models"`

    resolvedSecrets map[string]string
}

type Provider struct {
    Kind          string            `yaml:"kind"`
    BackendSchema string            `yaml:"backend_schema,omitempty"`
    Endpoint      string            `yaml:"endpoint"`
    PathPrefix    *string           `yaml:"path_prefix,omitempty"`
    Extra         map[string]string `yaml:"extra,omitempty"`
    Auth          Auth              `yaml:"auth"`
}

type ModelEntry struct {
    Provider string         `yaml:"provider"`
    Name     string         `yaml:"name,omitempty"`
    Metadata map[string]any `yaml:"metadata,omitempty"`
}

type Auth struct {
    Type      string `yaml:"type"`
    SecretRef string `yaml:"secret_ref,omitempty"`
}
```

### `Config.LookupModel`

```go
// LookupModel returns the provider name and effective backend model name for
// the given client model ID. Empty provider means no match.
func (c *Config) LookupModel(model string) (provider, backendModel string) {
    e, ok := c.Models[model]
    if !ok {
        return "", ""
    }
    if e.Name != "" {
        return e.Provider, e.Name
    }
    return e.Provider, model
}
```

### `translator.ProviderConfig`

```go
type ProviderConfig struct {
    BackendSchema string
    PathPrefix    string
    BackendModel  string            // formerly ModelNameOverride
    Extra         map[string]string
}
```

### Validation at load

- Every `models[*].provider` exists in `providers`.
- Every `providers[*].kind` resolves to a registered translator (after
  applying `backend_schema` defaulting).
- Every `providers[*].auth.type` exists in the auth registry.
- Every `secret_ref` resolves successfully.
- Each handler runs its own factory check (e.g., `aws` requires
  `extra.aws_region`).

Validation failures panic at module init; the demo prioritises loud boot
failures over silent runtime errors.

---

## Implementation plan

1. **`internal/config/config.go`**
   - Replace `Config` with the two-section shape above.
   - Add `ModelEntry`.
   - Rename `Auth.Secret` → `Auth.SecretRef`.
   - Rewrite `LookupModel` to return `(provider, backendModel string)`.
   - Delete `ClassifyCfg`, `TranslateCfg`, `HostpickCfg`, `ModelMatch`,
     `applyDefaults`.
   - Add load-time validation that calls into the auth registry to fail
     fast on unknown `auth.type` and missing handler-specific fields.

2. **`internal/pipeline/adapt/auth/` (new package)**
   - `registry.go` — `Register`, `New`, `Handler`, `Factory`,
     `BodyAwareHandler`.
   - `bearer.go`, `apikey.go`, `anthropic.go`, `aws.go`, `gcp.go` — one
     `init()` each. Move existing code in `adapt/auth.go` and
     `adapt/authcache.go` into these files; keep the per-upstream cache
     inside the package.

3. **`internal/translator/`**
   - Rename `ProviderConfig.ModelNameOverride` → `BackendModel` across all
     translators and tests. The codemod-generated files
     (`openai_*.go`) reference the field directly; one global rename pass.

4. **`internal/pipeline/match/match.go`**
   - Add `GET /v1/models` branch in `requestHandler`. Serialize
     `cfg.Models` keys with `owned_by` set from each entry's `provider`;
     call `SendLocalResponse`.
   - Hardcode `field = "model"` and the `on_miss` constants; drop
     `cfg.Classify` reads.
   - On the body path, publish three metadata keys: `orange.upstream`,
     `orange.model` (client ID), `orange.backend_model`.

5. **`internal/pipeline/adapt/adapt.go`**
   - Read `orange.backend_model` from dynamic metadata; pass it as
     `translator.ProviderConfig.BackendModel`.
   - Replace `cfg.Translate.StripRequestHeaders` with a package-level
     constant slice (`authorization`, `x-api-key`, `anthropic-version`).
   - Replace `buildAuthHandler` with `auth.New(prov.Auth, prov)`.

6. **`orange.yaml`** — rewrite to the shape at the top of this doc. Add the
   yaml-language-server hint for IDE validation:
   ```yaml
   # yaml-language-server: $schema=./orange.schema.json
   ```
   The schema lives at `examples/orange/orange.schema.json` and enforces
   required fields, known enum values, `secret_ref` format, and the
   `if/then` constraint that `bearer`/`x-api-key`/`anthropic` require
   `secret_ref` while `aws`/`gcp` do not.

7. **Tests**
   - Config tests: cover map-shaped `models`, alias resolution, missing
     provider reference, unknown `auth.type`.
   - `match` tests: add `GET /v1/models` happy path; keep body-phase tests
     unchanged in structure (assertions move to the new metadata keys).
   - `adapt` tests: cover the `BackendModel` plumbing; cover each registry
     handler factory directly.
   - `translator` tests: rename references to `BackendModel`.
