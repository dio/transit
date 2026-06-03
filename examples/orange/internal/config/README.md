# config

Loads and validates the orange proxy configuration.

The file shape (`orange.yaml`) is described by `config.schema.json`, which is
embedded at build time and applied at load. Structural errors come from the
schema; semantic errors (cross-references, secret resolution) are checked
after the typed unmarshal.

## Shape

```yaml
providers:
  <name>:
    kind: openai | anthropic               # client-facing API schema
    backend_schema: <enum>                  # optional, defaults to kind
    endpoint: https://...                   # upstream base URL
    path_prefix: /v1                        # optional, defaults to /v1
    extra: { key: value, ... }              # translator/auth config
    auth:
      type: bearer | x-api-key | anthropic | aws | gcp
      secret_ref: env://VAR_NAME            # required for bearer/x-api-key/anthropic

models:
  <client-facing model id>:
    provider: <name>                        # must exist in providers
    name: <backend model id>                # optional override
    metadata: { ... }                       # emitted verbatim by /v1/models
```

`backend_schema` enum: `openai`, `anthropic`, `azureopenai`, `awsbedrock`,
`awsanthropic`, `gcpvertexai`, `gcpanthropic`. It picks the translator; `kind`
picks the client API schema served to callers.

## Loading

| Entry point             | Use case                                              |
| ----------------------- | ----------------------------------------------------- |
| `Load([]byte)`          | Raw bytes — embedded fixtures, inline tests, etc.     |
| `LoadFrom(provider)`    | Any `ConfigProvider`                                  |
| `NewFileProvider(path)` | Local file                                            |
| `NewEnvVarProvider(EV)` | File path read from env var `EV`                      |
| `NewRemoteProvider(URL)`| `http(s)://` fetch                                    |
| `Get()`                 | Singleton driven by `ORANGE_CONFIG`; panics on error  |
| `LoadFile(path)`        | Convenience wrapper used by tests                     |

`ConfigProvider` is a one-method interface:

```go
type ConfigProvider interface {
    Snapshot() (*Config, error)
}
```

Each call to `Snapshot` may reload from the underlying source. `Get` caches
the first successful load; tests can call `MustReload` to drop the cache.

## Validation

1. **Schema** (`config.schema.json`): required fields, enum membership, type
   shapes, the `bearer/x-api-key/anthropic ⇒ secret_ref required` rule. YAML
   is roundtripped through JSON before validation so integer types match what
   the JSON Schema validator expects.
2. **Semantic**: every `models[*].provider` must exist in `providers`.
3. **Secret resolution**: every `auth.secret_ref` is resolved against the
   environment (`env://VAR_NAME`) and cached on the `Config`. Missing env vars
   surface as load errors. Retrieve resolved secrets with
   `cfg.ProviderSecret(name)`.

## Helpers

- `Provider.EffectiveBackendSchema()` — `BackendSchema` or fallback to `Kind`.
- `Provider.ResolvedPathPrefix()` — `PathPrefix` or `"/v1"`.
- `Provider.Host()` — hostname of the endpoint (drives SNI / `:authority`).
- `Config.LookupModel(id) (provider, backendModel string)` — exact match on
  the model map; backend name defaults to the map key when `ModelEntry.Name`
  is unset. Empty strings on miss.
- `Config.OpenAIV1Models() V1ModelList` — returns the model catalogue as an
  OpenAI-compatible `GET /v1/models` response body, sorted by model ID. Each
  entry carries `id`, `object: "model"`, `owned_by` (provider name), and
  the optional `metadata` blob from the model entry.

## Testdata

`testdata/` carries one minimal fixture, one full fixture covering every
provider kind + auth type, and four invalid fixtures pinning specific schema
or semantic failures. Tests set `TEST_*` env vars for `secret_ref` resolution.
