# Tiered Single-Envoy Example

This example verifies the L1 → L2 routing contract inside a single Envoy
process with three static listeners. It is a local scratchpad for iterating on
`cluster-shard-router` × `cluster-router` module interaction without a
Kubernetes cluster.

## What it shows

- A single Envoy process acting as both L1 (shard router) and L2 (model router)
- `cluster-shard-router` at L1 selecting a shard (L2 listener) based on `x-transit-tag`
- `cluster-router` at L2 selecting a provider upstream based on `x-model`
- Both modules using upstream HTTP filters and `CLUSTER_PROVIDED` load balancing
- Backend auth injection (`Authorization: Bearer ...`) by the cluster-router upstream filter

## Architecture

```
client
  │  x-transit-tag, x-model
  ▼
L1 listener :10000  (cluster-shard-router)
  │  shard a → L2-a :10001
  │  shard b → L2-b :10002
  ▼
L2-x listener        (cluster-router)
  │  model → provider upstream
  ▼
backend (httptest.Server in e2e)
```

L2 addresses are embedded directly in the L1 shard config; no service
discovery or external control plane is required.

## Run

Build the shared libraries and start Envoy with default ports
(L1 `:10000`, L2-a `:10001`, L2-b `:10002`, admin `:9901`):

```sh
make -C examples/tiered-single-envoy run
```

Send a request to shard a, model `gpt-fast`:

```sh
curl -s -X POST http://127.0.0.1:10000/v1/chat/completions \
  -H "x-transit-tag: a" -H "x-model: gpt-fast"
```

## Test

Run the Envoy e2e (builds both `.so` files, starts Envoy, runs assertions):

```sh
make -C examples/tiered-single-envoy e2e
```

Vet the example source:

```sh
make -C examples/tiered-single-envoy test
```

## Files

| Path | Role |
|---|---|
| `envoy.yaml` | Static Envoy config with default ports for `make run` |
| `cmd/main.go` | Template renderer — prints `envoy.yaml` with custom ports to stdout |
| `e2e/e2e_test.go` | Integration test: starts Envoy + fake backends, asserts shard and model routing |
| `e2e/testdata/envoy.tmpl.yaml` | Envoy config template used by the e2e test (ports injected at runtime) |
