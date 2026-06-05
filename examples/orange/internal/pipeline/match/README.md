# match

Downstream HTTP filter that routes each request to its upstream by reading the `model` field from the request body. It runs in two phases per stream: a headers phase that tags the stream and validates the caller, and a body phase that resolves the routing decision.

## Request flow

```
headers phase                        body phase
─────────────────────────────────    ─────────────────────────────────────────────────
Create StreamPromise[Decision]       Parse "model" from JSON body
Store in stream-object bag           Resolve ModelEntry from config (or per-key blob)
(pick's ChooseHost waits on it)      Walk RoutingNode (target / chain / split)
Inject Envoy retry headers           Rewrite :authority to provider host
Validate API key (key-mode)          Write filter state + dynamic metadata
                                     Resolve promise → pick completes host selection
```

## Endpoints handled

| Path | Method | Endpoint constant |
|------|--------|-------------------|
| `/v1/chat/completions` | POST | `EndpointChatCompletions` |
| `/v1/messages` | POST | `EndpointMessages` |
| `/v1/messages/count_tokens` | POST | `EndpointCountTokens` |
| `/v1/responses` | POST | `EndpointResponses` |
| `/v1/embeddings` | POST | `EndpointEmbeddings` |
| `/v1/images/generations` | POST | `EndpointImages` |
| `/v1/models` | GET | — (list; no upstream) |
| `/mcp/**` | GET / POST / DELETE | passthrough to `orange-mcp` sidecar |

## Routing nodes

`RoutingNode` is a tagged union; match walks it recursively:

| Node type | Behaviour |
|-----------|-----------|
| `target` | Direct provider reference. Returns the single upstream. |
| `chain` | Ordered fallback list. The first child is the primary; remaining children become `Fallbacks` on the `Decision`. Chains of chains are rejected. `adapt` advances through the list on each Envoy retry. |
| `split` | Weighted random sampling over child nodes. `sampleSplit` picks one child; match recurses into it. The selected child may itself be a `target` or `chain`. |

## Decision and filter state

`Decision` is published once per stream. On success its fields are written to both filter state (readable by `pick`) and dynamic metadata (readable by `adapt`, `meter`, `reqlog`):

| Filter state key | Dynamic metadata key | Value |
|------------------|----------------------|-------|
| `orange.provider_backend` | `orange:provider_backend` | config-level backend name (e.g. `"gemini"`) |
| `orange.provider_kind` | `orange:provider_kind` | API wire-format (e.g. `"openai"`) |
| `orange.model` | `orange:model` | client-facing model ID |
| `orange.endpoint` | `orange:endpoint` | endpoint discriminator |
| `orange.provider_binding` | `orange:provider_binding` | named binding within the provider |
| `orange.backend_model` | `orange:backend_model` | resolved backend model name |
| `orange.fallbacks` | — | JSON-encoded `[]Target` for retry advancement |

`ProviderBackend` ≠ `ProviderKind`: a Gemini backend speaking the OpenAI compatibility wire-format has `ProviderBackend="gemini"`, `ProviderKind="openai"`.

## Retry header injection

When the config contains chain routing, match injects Envoy request headers at the headers phase so `RetryStateImpl` picks them up before the first attempt:

- `x-envoy-max-retries` — deepest chain depth minus one.
- `x-envoy-retry-on` — union of all chains' retry conditions.
- `x-envoy-upstream-rq-per-try-timeout-ms` — maximum per-try timeout across all chains.

## Key-mode

When `keys[]` is present in config, match validates the `Authorization: Bearer <token>` header at the headers phase. Unknown or missing keys receive a `401` immediately; the `Decision` is resolved with `Err: ErrUnknownKey`. Accepted keys have their workspace/user/key-id written to dynamic metadata (`attribution.*`) and their per-key `ModelEntry` map takes precedence over the global `models[]` map.

## Error codes on `Decision.Err`

| Code | Trigger |
|------|---------|
| `orange.model_required` | `model` field absent from request body |
| `orange.model_not_found` | model present but has no upstream configured |
| `orange.not_found` | request path not handled by any route |
| `orange.unknown_key` | key-mode active and key not recognized |
| `orange.stream_terminated` | stream ended before body phase resolved the promise (disconnect, timeout, foreign local reply) |

`onStreamComplete` publishes `ErrStreamTerminated` as a no-op Resolve so `pick` can always complete cleanly regardless of how Envoy terminated the stream.
