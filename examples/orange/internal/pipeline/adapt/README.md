# adapt

Upstream HTTP filter that translates each request to its target provider's wire format and back. It runs as four Envoy extproc phases driven by a per-stream `translator.Translator`.

## Phase pipeline

| Phase | What happens |
|-------|-------------|
| **Request headers** | Reads `orange:provider_backend` and `orange:backend_model` from dynamic metadata. Advances the fallback chain on retries. Creates a `Translator` for the provider's schema and endpoint. Injects static auth credentials (`Bearer`, `APIKey`, `Anthropic`). Forces `accept-encoding: identity` when the client negotiated an unsupported encoding. |
| **Request body** | Calls `Translator.RequestBody` to translate the full buffered body to the backend wire format. Sets `:path` (some providers embed the model name in the URL). Re-appends the original query string to the translated path. Runs AWS SigV4 signing after the final body is known. Forces `accept-encoding: identity` for streaming requests. |
| **Response headers** | Calls `Translator.ResponseHeaders` to rewrite `content-type` when the response framing changes (e.g. converting backend EventStream to SSE for the downstream client). |
| **Response body** | Calls `Translator.ResponseBody` per chunk. Translators buffer partial frames internally and emit translated SSE. Non-streaming compressed bodies are decoded before translation and re-encoded after. |

## Fallback chain advancement

On each Envoy retry (`orange.adapt.attempt` filter state), adapt reads the `orange.fallbacks` JSON written by `match`, selects the target at index `attempt-1` (clamped to the last entry), and updates `orange:provider_backend` and `orange:backend_model` dynamic metadata so `meter` and `reqlog` reflect the provider that actually handled the request.

## Auth modes

| Handler | When selected | Injection point |
|---------|---------------|----------------|
| `noAuth` | Provider has no credentials configured | — |
| `BearerAuth` | Provider kind uses `Authorization: Bearer` | Phase 1 |
| `APIKeyAuth` | Provider uses `x-api-key` header | Phase 1 |
| `AnthropicAuth` | Anthropic direct; sets `x-api-key` + `anthropic-version` | Phase 1 |
| `AWSAuth` | AWS Bedrock; SigV4 over the final body | Phase 2 |
| `GCPAuth` | GCP Vertex; OAuth2 bearer token | Phase 1 |

## Passthrough mode

When the client sends `x-orange-api-key` in the request, adapt enters passthrough mode:

- The `x-orange-api-key` routing header is stripped (never forwarded upstream).
- The client's own `authorization`, `x-api-key`, and `anthropic-version` headers are left untouched and reach the upstream as-is.
- Orange's own credentials are not injected.
- `orange:passthrough = "true"` is written to dynamic metadata so `reqlog` can record it.

In normal mode, `authorization`, `x-api-key`, and `anthropic-version` are stripped from the client request and replaced with orange's own credentials.

## Config contract

adapt reads the following from dynamic metadata (written by `match`):

| Namespace | Key | Used for |
|-----------|-----|---------|
| `orange` | `provider_backend` | Select `ProviderConfig` from config snapshot |
| `orange` | `backend_model` | Pass to translator as `ProviderConfig.BackendModel` |

On retries these keys are updated in-place so downstream filters always see the provider that handled the current attempt.
