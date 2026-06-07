# config

Loads and validates the orange proxy configuration.

The file shape (`orange.yaml`) is described by `config.schema.json`, which is
embedded at build time and applied at load. Structural errors come from the
schema; semantic errors (cross-references, secret resolution) are checked
after the typed unmarshal.

## Shape

```yaml
llm:
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
2. **Semantic**: every `llm.models[*].provider` must exist in `llm.providers`.
3. **Secret resolution**: every `auth.secret_ref` is resolved according to its
   scheme and cached on the `Config`. Missing secrets surface as load errors.
   Retrieve resolved secrets with `cfg.ProviderSecret(name)`.

## Secret Schemes

The `auth.secret_ref` field supports multiple schemes for flexible secret management.

### Built-in Schemes

- `env://VAR_NAME` — Read from environment variable
- `file:///path/to/secret` — Read from file on disk (whitespace trimmed)
- `literal://value` — Plaintext literal (dev/test only; never use in production)

### Orange Scheme

The `orange://` scheme lazily resolves secrets using the `SecretResolverService`,
allowing config to reference secrets managed by the orange secret store:

```
orange://workspace_id/realm/secret_id
```

#### Usage Example

```yaml
llm:
  providers:
    anthropic:
      kind: anthropic
      endpoint: https://api.anthropic.com
      auth:
        type: anthropic
        secret_ref: orange://my-workspace/api-keys/anthropic-key
```

#### For Config Service Operators

To enable `orange://` scheme support when publishing config:

```go
httpClient := &http.Client{}
resolver := config.NewDefaultResolverWithOrange(httpClient, "http://localhost:8080", 1*time.Hour)

// Use with LoadWithResolver to resolve orange:// references
cfg, err := config.LoadWithResolver(ctx, yamlBytes, resolver)
```

#### How It Works

- **Lazy resolution**: Secrets are fetched on-demand by calling `SecretResolverService.Resolve`
- **Caching**: Results are cached with configurable TTL (default 1 hour)
- **Rotation**: Secrets can be rotated server-side; clients invalidate on updates
- **Performance**: Concurrent requests for the same secret are coalesced (single-flight)
- **Errors**: Failed resolutions are retried on the next access (not cached)

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
