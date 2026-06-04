# Orange `GET /v1/models` Local Response

## Goal

Orange should answer `GET /v1/models` locally from the current config snapshot.
The request should not enter provider routing, should not allocate the
`orange-match` stream promise used by body-routed requests, and should not
contact an upstream provider.

## Endpoint

`orange-match` registers:

```go
router.Handle(http.MethodGet, pathV1Models, listModels)
```

`POST /v1/models` is intentionally not registered. It continues to return the
standard `404` OpenAI-shaped error envelope with code `orange.not_found`.

## Response Shape

The response body is the OpenAI-compatible list produced by
`config.Get().OpenAIV1Models()`:

```json
{
  "object": "list",
  "data": [
    {
      "id": "gpt-4o-mini",
      "object": "model",
      "owned_by": "openai",
      "metadata": {
        "description": "GPT-4o mini via OpenAI.",
        "context_length": 128000,
        "max_tokens": 16384,
        "tags": ["chat", "responses", "fast", "vision"]
      }
    }
  ]
}
```

Model IDs are the client-facing keys from `models.<id>`. `owned_by` is the
orange provider name from the model entry, not the provider kind. When
`models.<id>.metadata` is present, it is emitted verbatim as `metadata`.
Ordering stays alphabetical by model ID because `OpenAIV1Models` owns catalogue
construction and sorting.

## Send Helper

`examples/orange/internal/send.JSON` is the generic JSON local-response helper:

```go
func JSON(w *up.Writer, status int, payload any, headers ...[2]string) error
```

It prepends `content-type: application/json`, appends caller headers, marshals
the payload, and sends via `Writer.SendLocalResponse`. If marshaling fails, it
returns the error without sending a partial response. Callers then send the
existing OpenAI-shaped error envelope.

The `/v1/models` payload is config-derived and marshal-safe in normal operation:
schema-validated YAML metadata is limited to JSON-compatible map, slice, string,
number, boolean, and null values.

## Routing Invariants

`GET /v1/models` must not:

- create a `StreamPromise`
- write the stream-object bag or `up.stream_object_id` filter state
- write `orange.model`, `orange.upstream`, or `orange.provider` filter state
- write orange routing dynamic metadata
- rewrite `:authority`
- initiate upstream host selection or provider traffic

It is a headers-phase local response.

## Auth Limitation

The first implementation is unauthenticated at the orange layer. This is a known
limitation because orange does not currently have downstream client auth.

Future auth gate: once orange has downstream auth, `GET /v1/models` should run
after downstream client authentication and before provider routing. The endpoint
should remain local-response-only after that gate succeeds.
