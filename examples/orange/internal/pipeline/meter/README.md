# meter

Response observer that extracts LLM token usage and emits Envoy counters and dynamic metadata. Runs as an upstream HTTP filter after `adapt`, adding zero latency — chunks are forwarded to the downstream client as they arrive.

## Extraction strategies

The provider kind (`orange.provider`) and endpoint (`orange.endpoint`) are read from dynamic metadata written by the `match` filter, and combined with the response `Content-Type` to select one of six strategies:

| Provider kind | Endpoint | Content-Type | Strategy | File |
|---------------|----------|-------------|----------|------|
| `openai`, `azureopenai`, … | `chat_completions` / `messages` | `application/json` | `ExtractOpenAIChatCompletionsJSON` | `meter_openai_chat_completions.go` |
| `openai`, `azureopenai`, … | `chat_completions` / `messages` | `text/event-stream` | `ExtractOpenAIChatCompletionsSSE` | `meter_openai_chat_completions.go` |
| `openai` | `responses` | `application/json` | `ExtractOpenAIResponsesJSON` | `meter_openai_responses.go` |
| `openai` | `responses` | `text/event-stream` | `ExtractOpenAIResponsesSSE` | `meter_openai_responses.go` |
| `anthropic`, `awsanthropic`, `gcpanthropic` | any | `application/json` | `ExtractAnthropicMessagesJSON` | `meter_anthropic_messages.go` |
| `anthropic`, `awsanthropic`, `gcpanthropic` | any | `text/event-stream` | `ExtractAnthropicMessagesSSE` | `meter_anthropic_messages.go` |

Any other `Content-Type` (e.g. AWS EventStream binary) is skipped; meter emits zero for that stream. Additional provider files (e.g. `meter_awsbedrock.go`) can be added later following the same pattern.

## Why `awsanthropic`/`gcpanthropic` map to the Anthropic strategy

`adapt` translates the upstream response body (Anthropic → OpenAI) and queues the replacement, but Envoy delivers the **original** upstream body to every co-filter in the chain. The meter therefore always sees the raw provider wire format, regardless of what `adapt` rewrites for the downstream client.

## Token usage fields

`TokenUsage` carries all fields reported by either provider. Fields not applicable to the current provider are zero.

```go
type TokenUsage struct {
    // Common
    Input  uint32
    Output uint32

    // OpenAI — prompt_tokens_details
    CachedInput uint32 // cached (prompt caching hit)
    AudioInput  uint32 // audio input tokens

    // OpenAI — completion_tokens_details
    ReasoningOutput          uint32 // reasoning tokens (o1 / o3 models)
    AudioOutput              uint32 // audio output tokens
    AcceptedPredictionOutput uint32 // speculative decoding: accepted
    RejectedPredictionOutput uint32 // speculative decoding: rejected

    // Anthropic — cache breakdown
    CacheCreationInput uint32 // cache_creation_input_tokens
    CacheReadInput     uint32 // cache_read_input_tokens
    CacheEphemeral5m   uint32 // cache_creation.ephemeral_5m_input_tokens
    CacheEphemeral1h   uint32 // cache_creation.ephemeral_1h_input_tokens
}
```

## Envoy counters

Counters are incremented only when the value is non-zero.

| Counter name | Description |
|-------------|-------------|
| `orange_input_tokens` | Prompt / input tokens |
| `orange_output_tokens` | Completion / output tokens |
| `orange_cached_input_tokens` | OpenAI: cached prompt tokens |
| `orange_audio_input_tokens` | OpenAI: audio input tokens |
| `orange_reasoning_output_tokens` | OpenAI: reasoning tokens (o1/o3) |
| `orange_audio_output_tokens` | OpenAI: audio output tokens |
| `orange_accepted_prediction_output_tokens` | OpenAI: accepted speculative tokens |
| `orange_rejected_prediction_output_tokens` | OpenAI: rejected speculative tokens |
| `orange_cache_creation_input_tokens` | Anthropic: standard cache write |
| `orange_cache_read_input_tokens` | Anthropic: cache read (reduced rate) |
| `orange_cache_ephemeral_5m_input_tokens` | Anthropic: ephemeral 5-minute cache write |
| `orange_cache_ephemeral_1h_input_tokens` | Anthropic: ephemeral 1-hour cache write |

## Dynamic metadata

Written to the `orange_meter` namespace at end-of-stream. Always present:

| Key | Value |
|-----|-------|
| `input_tokens` | `u.Input` |
| `output_tokens` | `u.Output` |

Written only when non-zero:

| Key | Value |
|-----|-------|
| `cached_input_tokens` | `u.CachedInput` |
| `reasoning_output_tokens` | `u.ReasoningOutput` |
| `cache_creation_input_tokens` | `u.CacheCreationInput` |
| `cache_read_input_tokens` | `u.CacheReadInput` |

## Reporting

### Access log

Add an `access_log` stanza to the `HttpConnectionManager` in `envoy.tmpl.yaml`. Envoy writes one JSON record per completed stream, reading the `orange_meter` metadata via `%DYNAMIC_METADATA(namespace:key)%`:

```yaml
access_log:
  - name: envoy.access_loggers.file
    typed_config:
      "@type": type.googleapis.com/envoy.extensions.access_loggers.file.v3.FileAccessLog
      path: /dev/stdout
      log_format:
        json_format:
          start_time:    "%START_TIME%"
          method:        "%REQ(:METHOD)%"
          path:          "%REQ(:PATH)%"
          response_code: "%RESPONSE_CODE%"
          duration_ms:   "%DURATION%"
          request_id:    "%REQ(x-request-id)%"
          # routing — written by the match filter
          model:         "%DYNAMIC_METADATA(orange:model)%"
          upstream:      "%DYNAMIC_METADATA(orange:upstream)%"
          provider:      "%DYNAMIC_METADATA(orange:provider)%"
          # token usage — written by the meter filter
          input_tokens:                "%DYNAMIC_METADATA(orange_meter:input_tokens)%"
          output_tokens:               "%DYNAMIC_METADATA(orange_meter:output_tokens)%"
          cached_input_tokens:         "%DYNAMIC_METADATA(orange_meter:cached_input_tokens)%"
          reasoning_output_tokens:     "%DYNAMIC_METADATA(orange_meter:reasoning_output_tokens)%"
          cache_creation_input_tokens: "%DYNAMIC_METADATA(orange_meter:cache_creation_input_tokens)%"
          cache_read_input_tokens:     "%DYNAMIC_METADATA(orange_meter:cache_read_input_tokens)%"
```

This is already wired in `envoy.tmpl.yaml` and `e2e/testdata/envoy.tmpl.yaml`. Keys whose `SetMetadata` was never called (zero / not applicable) render as `null` in the JSON output.

Example output line:

```json
{
  "start_time": "2026-06-03T12:00:00.000Z",
  "method": "POST", "path": "/v1/chat/completions",
  "response_code": 200, "duration_ms": 843,
  "request_id": "abc-123",
  "model": "gpt-4o-mini", "upstream": "openai", "provider": "openai",
  "input_tokens": 42, "output_tokens": 326,
  "reasoning_output_tokens": 256,
  "cached_input_tokens": null,
  "cache_creation_input_tokens": null, "cache_read_input_tokens": null
}
```

### Envoy stats

The `IncrementCounter` calls accumulate process-lifetime totals queryable at any time via the Envoy admin endpoint:

```
curl http://127.0.0.1:9901/stats?filter=orange_
```

```
orange_input_tokens: 12043
orange_output_tokens: 48291
orange_reasoning_output_tokens: 9830
orange_cache_read_input_tokens: 3200
orange_cache_creation_input_tokens: 0
...
```

The access log and stats serve different purposes: the access log is per-request and carries routing context (`model`, `upstream`); the stats endpoint gives aggregated totals with no per-request breakdown.

## Streaming internals

For `text/event-stream` responses, the meter uses a `buffer.HeadTail` ring (8 KB head / 64 KB tail). This bounds memory use regardless of stream length while guaranteeing:

- **OpenAI SSE**: usage chunk always appears near the tail (last non-`[DONE]` event).
- **Anthropic SSE**: input tokens are in the head (`message_start` event); output tokens are in the tail (`message_delta` event).

Non-streaming (`application/json`) accumulates the full body across chunks before parsing.

## Tests

| Test | What it covers |
|------|---------------|
| `TestExtractOpenAIChatCompletionsJSON_Chat` | `prompt_tokens` / `completion_tokens` |
| `TestExtractOpenAIChatCompletionsJSON_WithDetails` | Full `prompt_tokens_details` + `completion_tokens_details` |
| `TestExtractOpenAIChatCompletionsJSON_ChunkedBody` | Body split across two chunks |
| `TestExtractOpenAIChatCompletionsJSON_NoUsage` / `_Empty` / `_Invalid` | Error paths |
| `TestExtractOpenAIChatCompletionsSSE_Chat` | Usage in last SSE chunk before `[DONE]` |
| `TestExtractOpenAIChatCompletionsSSE_WithDetails` | Detail fields in SSE usage chunk |
| `TestExtractOpenAIChatCompletionsSSE_LargeStream_HeadTailOnly` | >64 KB stream; usage present only in tail |
| `TestExtractOpenAIChatCompletionsSSE_Empty` | Empty stream |
| `TestExtractOpenAIResponsesJSON_Basic` | `input_tokens` / `output_tokens` |
| `TestExtractOpenAIResponsesJSON_WithDetails` | `input_tokens_details.cached_tokens` + `output_tokens_details.reasoning_tokens` |
| `TestExtractOpenAIResponsesSSE_NestedUsage` | `response.completed` event with nested `response.usage` |
| `TestExtractOpenAIResponsesSSE_LargeStream` | >64 KB delta stream; `response.completed` in tail |
| `TestExtractAnthropicMessagesJSON_Basic` | `input_tokens` / `output_tokens` |
| `TestExtractAnthropicMessagesJSON_WithCacheTokens` | All cache fields including ephemeral |
| `TestExtractAnthropicMessagesJSON_NoUsage` / `_Empty` / `_Invalid` | Error paths |
| `TestExtractAnthropicMessagesSSE_Basic` | `message_start` (input) + `message_delta` (output) |
| `TestExtractAnthropicMessagesSSE_WithCacheTokens` | Cache fields in `message_start` |
| `TestExtractAnthropicMessagesSSE_LargeStream_HeadTailOnly` | >200 KB stream; head/tail split |
| `TestExtractAnthropicMessagesSSE_Empty` | Empty stream |
