# hello

This is the smallest transit example. It registers one HTTP filter named
`hello`, logs the request method and path, then lets Envoy continue to the route.

The Envoy config returns a direct response, so no upstream service is needed.

## What it shows

- `up.Register`
- a plain `func(w *up.Writer, r *up.Request)` handler
- logging through Envoy
- a minimal dynamic module entrypoint in `cmd/main.go`

## Run

From the repository root:

```sh
make -C examples/hello run
```

Then send a request:

```sh
curl localhost:10000/
```

## Test

Unit tests:

```sh
make -C examples/hello test
```

End to end test:

```sh
make -C examples/hello e2e
```

## Files

- `hello.go` contains the handler.
- `cmd/main.go` links the ABI layer and registers the handler.
- `envoy.yaml` is a reference bootstrap for local Envoy runs.
- `e2e/` builds the shared library, starts Envoy, and verifies requests through
  the real module.
