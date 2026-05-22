# request-ui

This example records requests and responses that pass through Envoy, then serves
a small UI for inspecting them.

It combines an HTTP filter with a dynamic module access logger. The filter
captures request and response data while the stream is active. The access logger
runs after the stream ends and fills in finalized fields such as duration,
response flags, byte counts, trace IDs, and upstream timing.

Records can be stored in Postgres or in an in-memory ring buffer.

## What it shows

- `up.RegisterWithResponse`
- `up.RegisterAccessLogger`
- correlation through `x-request-id`
- finalized stream data from `AccessLoggerHandle`
- a module-owned HTTP UI and SSE updates
- optional Postgres storage

## Run without Envoy

Use the simulator when you only want to look at the UI:

```sh
make -C examples/request-ui simulate
```

Open:

```text
http://localhost:6062/
```

The simulator uses generated records and does not require Envoy or Postgres.

## Run with Envoy

From the example directory:

```sh
make -C examples/request-ui run
```

The proxy listens on `localhost:10000`. The UI listens on `localhost:6062`.

The reference config points at an upstream host named `upstream` on port `8080`.
Replace that cluster in `envoy.yaml` or provide a matching service.

## Storage

By default the sink uses Postgres mode. For local memory mode:

```sh
REQUI_MODE=memory make -C examples/request-ui run
```

Useful environment variables:

- `REQUI_MODE` selects `postgres` or `memory`.
- `REQUI_DSN` sets the Postgres DSN.
- `REQUI_ADDR` sets the UI listen address.

## Test

Unit tests:

```sh
make -C examples/request-ui test
```

End to end test:

```sh
make -C examples/request-ui e2e
```

## Files

- `filter.go` records request and response state.
- `accesslogger.go` enriches records after the stream completes.
- `sink/` stores records and serves the UI.
- `cmd/main.go` starts the sink, links the ABI layer, and registers both module
  components.
- `envoy.yaml` is a reference bootstrap for local Envoy runs.
