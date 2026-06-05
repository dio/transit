# Implementation prompt: `orange-responsesws` loopback as a ClusterExtension

Hand this file to a fresh agent. The full design rationale is in
`docs/orange-sidecar-loopback-cluster-extension.md` — read that for the
"why". This file is the "how" for the second of three PRs.

PR #1 (`orange-mcp`) is merged. Its output is the canonical reference for
everything structural. Read the MCP PR diff first; this PR is the same shape
with four responsesws-specific wrinkles called out below.

---

## Task

Fold the `orange-responsesws` sidecar's lifecycle and its loopback endpoint
into a single dynamic-modules cluster. After this change:

- The no-op `orange-responsesws` HTTP filter is gone.
- The static `orange-responsesws-loopback` cluster is replaced by a
  dynamic-modules cluster that owns the sidecar.
- The sidecar bind port lives in Go only — the YAML no longer duplicates it.
- `/v1/responses` WebSocket upgrade behavior is unchanged end-to-end.
- `orange-responsesws-meter` is **not** touched — it is a real filter and
  stays exactly where it is.

Out of scope: `orange-mcp` (done), extracting a shared `up/sidecarcluster`
package (PR #3).

## Read these first

1. `docs/orange-sidecar-loopback-cluster-extension.md` — the design, especially
   "Conversion 2: `orange-responsesws`" and the "Generalization" section.
2. `examples/orange/internal/pipeline/mcp/loopback/loopback.go` — the MCP
   cluster you are mirroring. Shape is identical: `factory` → `cfgFactory` →
   `cluster`, `Init` binds synchronously, `ServerInitialized` starts
   `ClusterGroup`, `Shutdown` calls `sc.Stop()`.
3. `examples/orange/internal/pipeline/mcp/sidecar.go` — the Listen/Serve/Stop
   split you are replicating in `responsesws.go`.
4. `examples/orange/internal/pipeline/responsesws/responsesws.go` — the code
   you are moving. Focus on `init()`, `responsesWSSidecar`, `execute`, `stop`,
   `newResponsesWSSidecar`, `listenForSidecar`, and `resolveListenAddr`.
5. `examples/orange/envoy.tmpl.yaml` — the YAML to edit. After the MCP PR the
   `http_filters` list reads: `orange-match`, `orange-responsesws`, `router`.
   The `upgrade_configs.filters` list reads: `orange-responsesws-meter`,
   `router` — leave `upgrade_configs` untouched.

## Differences from the MCP PR (responsesws-specific wrinkles)

| Concern | MCP | Responsesws |
|---|---|---|
| Filter to delete | `orange-mcp` (no-op) | `orange-responsesws` (no-op) |
| Filter to keep | `orange-mcp-egress-match` | `orange-responsesws-meter` (real filter — do **not** touch) |
| Loopback transport | HTTP/1.1 + SSE | HTTP/1.1 + WebSocket upgrade |
| UDS egress | not applicable | `ORANGE_RESPONSESWS_EGRESS_URL=ws+unix://...` is supported; `dialOptionsForEgress` already handles it |
| Sidecar constructor error return | always nil | `newResponsesWSSidecar` returns `error`; it is always nil in practice — keep the error return in the exported constructor for forward compatibility |
| `execute` name arg | dropped (became a `fmt.Fprintf` prefix) | same: drop the name arg; move the `log.Info("…sidecar listening…")` call into `Serve()` |
| UDS socket cleanup | `Stop()` removes file | same — currently **missing** from `responsesws.stop()`; add it in `Stop()` |
| Default listen addr | changed to `127.0.0.1:0` | change `127.0.0.1:10002` → `127.0.0.1:0` |
| `clusterConfig` extra fields | `listen_addr` only | `listen_addr` only (same as MCP; `egress_url` and `first_frame_timeout` remain env-only for now) |

`orange-responsesws-meter` lives in `upgrade_configs.filters`, **not** in
`http_filters`. Deleting the `orange-responsesws` entry from `http_filters`
will not affect it. See `docs/orange-sidecar-loopback-cluster-extension.md`
"What stays put: `orange-responsesws-meter`" for the full explanation of why.

## Steps

### Step 1 — Split sidecar lifecycle in `responsesws.go`

Perform the same Listen/Serve/Stop split that was done on `mcp/sidecar.go`:

- Rename `responsesWSSidecar` → `Sidecar` (export the type).
- Rename `responsesWSSidecarOptions` → `sidecarOptions` (keep unexported; only
  the loopback sub-package calls the exported constructor).
- Split `execute(name string) error` into:
  - `Listen() error` — `listenForSidecar(s.opts.listenAddr)`, populate
    `s.ln / s.resolved / s.srv`, close `s.ready` + `s.started`. On error,
    close `s.started` and return. Track `unixSocketPath` when
    `ln.Addr().Network() == "unix"`.
  - `Serve() error` — log `"orange-responsesws sidecar listening"`, check
    `s.opts.egressURL == ""` warn, then `s.srv.Serve(s.ln)`.
- `stop()` → `Stop()` — add `os.Remove(unixSocketPath)` after `ln.Close()`,
  matching MCP.
- Update `newResponsesWSSidecar` to return `(*Sidecar, error)` (signature
  unchanged, just the type name).
- Add exported constructor in `responsesws.go`:

```go
// NewSidecar constructs the responsesws handler and sidecar. listenAddr
// overrides ORANGE_RESPONSESWS_LISTEN_ADDR and the compiled-in default;
// pass "" to use the env var / default. Supports TCP ("127.0.0.1:0") and
// Unix sockets ("unix:///tmp/orange-responsesws.sock"). The sidecar is not
// yet bound; call Listen then Serve to start it.
func NewSidecar(listenAddr string) (*Sidecar, error) {
    if listenAddr == "" {
        listenAddr = resolveListenAddr()
    }
    log := observability.Logger("orange/responsesws")
    handler := &responseswsHandler{
        egressURL:         resolveEgressURL(),
        firstFrameTimeout: resolveFirstFrameTimeout(),
        log:               log,
        onSummary:         publishSummary,
    }
    return newSidecar(handler, sidecarOptions{
        listenAddr:      listenAddr,
        shutdownTimeout: 5 * time.Second,
        egressURL:       handler.egressURL,
        log:             log,
    })
}
```

Change `defaultListenAddr` from `"127.0.0.1:10002"` to `"127.0.0.1:0"`.

### Step 2 — Clean up `init()` in `responsesws.go`

```go
func init() {
    up.Register(MeterFilterName, requestHandler, up.WithResponse(responseHandler))
}
```

- Delete the `FilterName` constant.
- Delete `up.Register(FilterName, func(*up.Writer, *up.Request) {}, up.WithGroup(g))`.
- Keep `MeterFilterName` and `up.Register(MeterFilterName, ...)` exactly as-is.
- Update `responsesws_test.go`: `TestInit_registersFilterName` tests both
  `FilterName` and `MeterFilterName`; remove the `FilterName` assertion.

### Step 3 — New package `responsesws/loopback`

Create `examples/orange/internal/pipeline/responsesws/loopback/loopback.go`.
Mirror `mcp/loopback/loopback.go` exactly, substituting:

| MCP | Responsesws |
|---|---|
| `"orange-mcp-loopback"` | `"orange-responsesws-loopback"` |
| `observability.Logger("orange/mcp/loopback")` | `observability.Logger("orange/responsesws/loopback")` |
| `mcp.NewSidecar(c.cfg.ListenAddr)` | `responsesws.NewSidecar(c.cfg.ListenAddr)` |
| `*mcp.Sidecar` | `*responsesws.Sidecar` |

`clusterConfig` struct is identical (`listen_addr` only). The `Init` /
`ServerInitialized` / `Shutdown` / `NewClusterLB` logic is verbatim from MCP.

Add `loopback_test.go` mirroring `mcp/loopback/loopback_test.go`. The three
tests are identical in structure:

- `TestInitBindSuccess` — `"127.0.0.1:0"`, verify `AddHosts`, `UpdateHostHealth`,
  `PreInitComplete`, `ChooseHost`, shutdown.
- `TestInitBindSuccessUDS` — `"unix://" + t.TempDir() + "/rws.sock"`, verify
  `AddHosts` called with the socket path.
- `TestInitBindFail` — occupy the port, verify no `AddHosts`, `PreInitComplete`
  still called.

### Step 4 — Register in `cmd/main.go`

```go
_ "github.com/dio/transit/examples/orange/internal/pipeline/responsesws/loopback"
```

Add it after the `responsesws` import.

### Step 5 — YAML

Edit `examples/orange/envoy.tmpl.yaml`:

- Delete the `orange-responsesws` HTTP filter entry from `http_filters`
  (the one with the comment "no-op for HTTP; its WithGroup starts the Responses
  WS sidecar"). The `upgrade_configs.filters` block (which contains
  `orange-responsesws-meter`) is **not** in `http_filters` — leave it alone.
- Replace the `orange-responsesws-loopback` static cluster (type: STATIC,
  load_assignment with port 10002) with the dynamic-modules version from
  `docs/orange-sidecar-loopback-cluster-extension.md` "Conversion 2 → Rendered
  config → clusters". Add the same `cluster_config` comment block as
  `orange-mcp-loopback` (identical `listen_addr` options including the K8s
  `emptyDir` note, but update the cluster name and example socket path to
  `orange-responsesws`).
- Regenerate `envoy.yaml` via `envsubst` (see Makefile `envoy.yaml` target).

Leave alone: `upgrade_configs` block (including `orange-responsesws-meter`),
`orange-responsesws-egress` listener, `orange-responsesws-default` cluster,
`orange-responsesws-egress-match` filter, all access logs.

### Step 6 — Build and e2e

1. `make` at repo root or `make build` in `examples/orange/` to rebuild
   `liborange.so`.
2. `make test` to run the unit test suite.
3. Run the orange e2e suite: `cd examples/orange/e2e && make` (check Makefile
   for the exact target).
4. Sanity-check `make run` and run the `codex-demo` script to confirm WebSocket
   upgrade end-to-end:
   ```
   cd examples/orange && ./codex-demo --ws exec 'reply with exactly: responsesws ok'
   ```
5. Confirm the access log emits token fields (`orange:model`, `orange_meter:*`)
   for a WebSocket session — this validates the meter-bridge path is unbroken.

Likely failure modes:
- `orange-responsesws-meter` fails to initialise: you accidentally deleted
  something from `upgrade_configs`. Check the YAML diff carefully.
- Meter bridge emits no token fields: `MeterFilterName` registration was
  accidentally removed from `init()`.
- Port conflict: `Listen()` blocks because `s.opts.listenAddr` is still hardcoded
  to `10002` somewhere. Verify `defaultListenAddr` was updated.

## Acceptance

- `make` builds clean.
- Orange e2e suite passes.
- `make run` + `./codex-demo --ws exec 'ping'` works end-to-end; access log
  shows `orange:model` and `orange_meter:*` fields.
- `grep -rn "orange-responsesws" examples/orange/envoy.tmpl.yaml` returns only
  the route match, the egress listener, the egress cluster, the
  `upgrade_configs.filters` entries, and the new dynamic-modules
  `orange-responsesws-loopback` block — *not* an `http_filters` entry.
- `grep -rn "FilterName" examples/orange/internal/pipeline/responsesws/`
  returns only `MeterFilterName` references.

## PR description checklist

Include in the PR body:

- Confirmation that the open questions from the MCP PR apply unchanged (fixed
  LB, PreInitComplete on failure, Listen/Serve split).
- Note on the UDS socket cleanup addition to `Stop()` (bug fix vs. MCP parity).
- Confirmation that `orange-responsesws-meter` in `upgrade_configs.filters`
  is untouched and the meter bridge still emits token fields (test it).
- Confirmation that `ORANGE_RESPONSESWS_LISTEN_ADDR` still overrides the
  address (test once: `ORANGE_RESPONSESWS_LISTEN_ADDR=127.0.0.1:19002 make run`).
- Note that `orange-mcp` is intentionally not touched (done in PR #1).

## What you must not do

- Do not touch `upgrade_configs` or `orange-responsesws-meter`. The meter-bridge
  filter stays on the upgrade path. See the design doc for why.
- Do not add `egress_url` or `first_frame_timeout` to `clusterConfig`. Those
  are env-only for now; wiring them through YAML is a separate decision.
- Do not extract a shared `sidecarcluster` package. That is PR #3.
- Do not change `/v1/responses` route semantics, WebSocket upgrade config, or
  any user-visible behavior.
- Do not switch the sidecar to an ephemeral UDS path (:0 analogue for sockets).
  The ephemeral TCP default (127.0.0.1:0) is sufficient for this PR.
- Do not skip the UDS socket cleanup in `Stop()` — it is a correctness fix
  (leaked socket files on restart) that belongs in this PR.
