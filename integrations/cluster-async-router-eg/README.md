# Cluster Async Router Envoy Gateway Integration

Status: **scaffold only**. P1 of a 4-phase build — see
[`docs/cluster-async-router-eg.md`](../../docs/cluster-async-router-eg.md) for
the design.

This integration will run the `examples/cluster-async-router` dynamic module
behind Envoy Gateway in a local k3d cluster, and additionally exercise the
per-host TLS path the SDK's `HostSpec.Metadata` change unlocks.

It is the body-driven, TLS-aware sibling of
[`cluster-router-eg`](../cluster-router-eg/):

- `cluster-router-eg` routes on the `x-model` request header (headers phase).
- `cluster-async-router-eg` routes on the request body (async ChooseHost via
  the `Pending` token pattern).
- `cluster-async-router-eg` also demonstrates per-host TLS using
  `HostSpec.Metadata` + `transport_socket_matches` injected via
  `EnvoyPatchPolicy`.

## Phases

| Phase | Status | Lands |
|---|---|---|
| P1 — design + scaffold | in progress | this README, Makefile, Dockerfiles, design doc |
| P2 — body-driven routing on k3d (plaintext) | not started | k8s manifests, demo binary, e2e |
| P3 — per-host TLS via EnvoyPatchPolicy | not started | TLS upstreams, cert provisioning, transport_socket_matches injection |
| P4 — EPP integration (Gateway API Inference Extension) | deferred | speculative — see design doc |

## Layout (target)

Mirrors `cluster-router-eg`:

```
integrations/cluster-async-router-eg/
  README.md                 ← you are here
  Makefile
  Dockerfile.envoy          # custom Envoy + libcluster-async-router.so
  Dockerfile.demo           # demo binary (upstream, upstream-tls, CLI)
  k8s/                      # P2 / P3 manifests (gateway, httproute, envoyproxy, EPP, demo)
  cmd/demo/                 # demo binary entrypoint
  internal/demo/            # control plane / upstream / client / TLS helpers
  e2e/                      # k3d e2e suite, gated by RUN_CLUSTER_ASYNC_ROUTER_EG_E2E=1
```

## Why not just extend cluster-router-eg

Body-driven routing and per-host TLS are both about timing and metadata
flowing through layers that `cluster-router-eg` doesn't exercise. Bolting
them onto that integration would obscure both. A sibling keeps each
integration single-purpose and reviewable.

See the design doc for the full rationale and the EPP open question.
