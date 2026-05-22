# sse-tap

This example observes streaming server sent event responses and extracts token
usage from LLM streaming formats.

It does not buffer the whole response. Instead it keeps a small head and tail
window, forwards chunks as they arrive, then parses the saved windows when the
stream ends.

## What it shows

- `up.RegisterWithConfig`
- response body observation without replacing the body
- Envoy counters defined at config time
- dynamic metadata written at stream completion
- per-request state stored in `ResponseChunk.Context`

## Supported stream formats

The parser handles the token usage shapes used by Anthropic and OpenAI streaming
responses:

- Anthropic `message_start` for input tokens
- Anthropic `message_delta` for output tokens
- OpenAI usage chunks for prompt and completion tokens

The module emits:

- `sse_tap_input_tokens`
- `sse_tap_output_tokens`
- dynamic metadata under the `sse_tap` namespace

## Run

The reference `envoy.yaml` routes to an upstream service on `127.0.0.1:8080`.
Start a streaming upstream there, then run Envoy from the repository root:

```sh
make build EXAMPLE=sse-tap EXAMPLE_CMD=./examples/sse-tap/cmd
ENVOY_DYNAMIC_MODULES_SEARCH_PATH=$PWD/dist \
GODEBUG=cgocheck=0 \
.bin/envoy -c examples/sse-tap/envoy.yaml
```

Then send traffic through Envoy:

```sh
curl -N localhost:10000/
```

## Test

Unit tests:

```sh
cd examples && GOWORK=off go test ./sse-tap/...
```

End to end test:

```sh
make e2e-sse-tap
```

## Files

- `sse_tap.go` contains the filter, parser, metrics, and metadata writes.
- `cmd/main.go` links the ABI layer and imports the example package.
- `envoy.yaml` is a reference bootstrap for local Envoy runs.
- `e2e/` verifies streaming behavior through a real Envoy process.
