---
name: transit-envoy-dynamic-modules
description: Build, wire, debug, and validate Transit dynamic module filters and cluster extensions in both local examples and Envoy Gateway integrations. Covers registration, filter_name wiring, body callbacks, callout pitfalls, Cluster Extension lifecycle, EnvoyProxy dynamicModules, EnvoyPatchPolicy filter insertion, and common failure modes.
---

# Transit Envoy Dynamic Modules

Use this skill for any work involving Transit `.so` files: registration
failures, filter wiring, body callbacks, callout problems, Cluster Extension
lifecycle, or EPP-based filter insertion under Envoy Gateway.

Related skills:
- `envoy-abi-wrapper` — changing the low-level C ABI wrappers
- `transit-k3d-envoy-gateway-e2e` — k3d cluster setup and EPP debugging order
- `transit-example-creator` — example file layout and Makefile shape

---

## Mental Model

Four checkpoints. Debug in order:

1. **Shared library built** — the expected `lib*.so` exists and Envoy is
   loading it.
2. **Module loaded** — Envoy logs `Dynamic module ABI version ... matched`.
3. **Filter/cluster registered** — Go `init()` reached `up.Register*` or
   `up.RegisterCluster` with a name that matches what Envoy expects.
4. **Callbacks invoked** — request/body/upstream callbacks fire and affect the
   stream.

Do not debug callout, routing, or body-buffering issues until checkpoint 3 is
confirmed.

---

## Entrypoint Shape

Every `.so` command package:

```go
package main

import (
    _ "github.com/dio/transit/down/abi_impl"
    example "github.com/dio/transit/examples/<name>"
)

func init() {
    config, err := example.LoadConfigFromEnv()
    if err != nil {
        log.Printf("<name>: %v", err)
        return  // module loads but filter NOT registered
    }
    example.RegisterTransitFilter("<filter-name>", config)
}

func main() {}
```

Rules:
- `down/abi_impl` blank import must be in the `cmd/main.go` package built with
  `-buildmode=c-shared`, never in a library package.
- Registration must happen from `init()`. Envoy loads the `.so`; it never calls
  `main()`.
- If `init()` returns before `up.Register*`, the module loads silently but the
  filter is not available to Envoy.

For Cluster Extension:

```go
func init() {
    up.RegisterCluster("<cluster-name>", &myClusterFactory{})
    up.Register("<debug-filter-name>", debugHandler)
    up.Register("<upstream-filter-name>", upstreamHandler)
}
```

For LB Policy:

```go
func init() {
    up.RegisterLBPolicy("<policy-name>", &myLBFactory{})
}
```

---

## Envoy Config Wiring

Three distinct names — all must be consistent:

```yaml
# HTTP filter in a listener or EPP JSONPatch:
- name: my-filter-readable        # Envoy config name — cosmetic
  typed_config:
    "@type": type.googleapis.com/envoy.extensions.filters.http.dynamic_modules.v3.DynamicModuleFilter
    dynamic_module_config:
      name: my-module              # matches dynamicModules[].name in EnvoyProxy
    filter_name: my-filter-name    # must match string passed to up.Register*
    filter_config:                 # optional; passed as []byte to ConfigFunc
      "@type": type.googleapis.com/google.protobuf.StringValue
      value: '{"key":"value"}'
```

```yaml
# Upstream HTTP filter in a cluster (Cluster Extension):
- name: my-upstream-filter
  typed_config:
    "@type": type.googleapis.com/envoy.extensions.filters.http.dynamic_modules.v3.DynamicModuleFilter
    dynamic_module_config:
      name: my-module
    filter_name: my-upstream-filter-name   # matches up.Register("my-upstream-filter-name", ...)
```

`dynamic_module_config.name` resolves the loaded module. In EnvoyProxy it
matches `dynamicModules[].name`. In local Envoy config it matches the module
file stem (`libfoo.so` → `foo`) unless overridden.

---

## EnvoyProxy dynamicModules

Declare modules on the EnvoyProxy resource before referencing them in EPP:

```yaml
spec:
  dynamicModules:
  - name: my-module
    source:
      type: Local
      local:
        path: /etc/envoy/dynamic-modules/libmy-module.so
    doNotClose: true      # required: keeps the .so resident across config updates
    loadGlobally: false   # per-proxy scope; true only for root-process filters
  provider:
    kubernetes:
      envoyDeployment:
        container:
          env:
          - name: ENVOY_DYNAMIC_MODULES_SEARCH_PATH
            value: /etc/envoy/dynamic-modules
          - name: MY_MODULE_CONFIG
            value: '{"key":"value"}'
```

`ENVOY_DYNAMIC_MODULES_SEARCH_PATH` is required. Without it Envoy may accept
the `local` path declaration but fail to resolve `libmy-module.so` when a
dynamic-module cluster or EPP patch references it by stem.

`doNotClose: true` is required for any module used in dynamic cluster config.
Closing the `.so` between xDS updates causes crashes.

---

## EPP Filter Insertion (Envoy Gateway)

Insert a filter at the front of an existing listener chain:

```yaml
jsonPatches:
- type: "type.googleapis.com/envoy.config.listener.v3.Listener"
  name: tcp-80
  operation:
    op: add
    path: "/default_filter_chain/filters/0/typed_config/http_filters/0"
    value:
      name: my-filter-readable
      typed_config:
        "@type": type.googleapis.com/envoy.extensions.filters.http.dynamic_modules.v3.DynamicModuleFilter
        dynamic_module_config:
          name: my-module
        filter_name: my-filter-name
```

Multiple `add` patches at the same path insert in reverse order (each new
patch pushes earlier ones down). Apply the second filter first if order matters:

```yaml
# Applied first → ends up at index 1 after the second patch
- op: add  path: "/default_filter_chain/filters/0/typed_config/http_filters/0"
  value: {name: second-filter ...}
# Applied second → ends up at index 0 (front)
- op: add  path: "/default_filter_chain/filters/0/typed_config/http_filters/0"
  value: {name: first-filter ...}
```

---

## Cluster Extension EPP Patch

Replace a generated EG backend cluster with a dynamic cluster extension:

```yaml
- type: "type.googleapis.com/envoy.config.cluster.v3.Cluster"
  name: "httproute/<namespace>/<httproute>/rule/0"
  operation:
    op: replace
    path: ""
    value:
      name: "httproute/<namespace>/<httproute>/rule/0"
      connect_timeout: 5s
      lb_policy: CLUSTER_PROVIDED
      typed_extension_protocol_options:
        envoy.extensions.upstreams.http.v3.HttpProtocolOptions:
          "@type": type.googleapis.com/envoy.extensions.upstreams.http.v3.HttpProtocolOptions
          explicit_http_config:
            http_protocol_options: {}
          http_filters:
          - name: my-upstream-filter
            typed_config:
              "@type": type.googleapis.com/envoy.extensions.filters.http.dynamic_modules.v3.DynamicModuleFilter
              dynamic_module_config:
                name: my-module
              filter_name: my-upstream-filter-name
          - name: envoy.filters.http.upstream_codec
            typed_config:
              "@type": type.googleapis.com/envoy.extensions.filters.http.upstream_codec.v3.UpstreamCodec
      cluster_type:
        name: envoy.clusters.dynamic_modules
        typed_config:
          "@type": type.googleapis.com/envoy.extensions.clusters.dynamic_modules.v3.ClusterConfig
          dynamic_module_config:
            name: my-module
          cluster_name: my-module
          cluster_config:
            "@type": type.googleapis.com/google.protobuf.StringValue
            value: '{"route_header":"x-my-header","initial":{...}}'
```

The `cluster_name` field under `ClusterConfig` is the logical name used to
look up the factory registered with `up.RegisterCluster`. It does not have to
match the Envoy cluster name.

---

## Critical: Do Not Patch the Live Catalog Backend Cluster

When using a Cluster Extension as a routing layer (e.g. cluster-router), only
patch a cluster that the extension is supposed to fully own. Never patch the
live backend cluster that a demo service depends on.

The failure mode (`bytes_received: 0`, `no_healthy_upstream 503`):

```
catalog-router-demo receives /mcp/s/kiwi
  → sets x-mcp-server: kiwi on its outgoing HTTP call
  → outgoing call hits the patched cluster
  → ChooseHost reads x-mcp-server from incoming request headers
  → incoming request has NO x-mcp-server (set only on outgoing call)
  → ChooseHost returns nil → no_healthy_upstream → 503
```

Fix: add a dedicated init HTTPRoute (`l2-a-cluster-router-init`) pointing at
the same backend. Patch THAT cluster with the extension. Real traffic flows
through the original catalog cluster unchanged. The extension initializes its
route store from the `initial` config and serves the debug dump endpoint.

---

## Cluster Extension Lifecycle

```go
type myCluster struct {
    handle  up.ClusterHandle
    cfg     myConfig
}

func (c *myCluster) Init(h up.ClusterHandle) {
    c.handle = h
    // Bootstrap hosts synchronously so the cluster is ready before xDS
    // marks it available. Do NOT rely only on ServerInitialized: in the
    // Envoy Gateway path the patched cluster arrives via CDS after Envoy
    // is already running, so ServerInitialized may never fire for it.
    if len(c.cfg.Models) > 0 {
        snap := resolveSnapshot(c.cfg.Initial)
        c.applySnapshot(snap)
    }
    h.PreInitComplete()
    c.startFetchLoop()  // if live refresh is needed
}

func (c *myCluster) ServerInitialized(h up.ClusterHandle) {
    c.startFetchLoop()  // guard against starting twice
}
```

`PreInitComplete` must be called from `Init` or Envoy will hold the cluster
in a pending state indefinitely.

---

## Body Callback and Callout Pitfalls

**Fallback cluster must be reachable:**
When the header callback returns `Continue` and the body callback initiates
`HTTPCallout` + `SendLocalResponse`, Envoy's router immediately tries to
connect to the fallback cluster. If that connection fails before the callout
callback fires, `streamDone = true` and the callout callback is silently
skipped — the client sees the upstream error, not the filter response.

Use a real reachable cluster as the fallback (e.g. the same cluster the callout
targets, or a loopback listener). Avoid `direct_response` or port-1 blackholes
as the catch-all route for filters that call `SendLocalResponse`.

**`RegisterWithMutableBody` vs `RegisterWithBody`:**
Use `RegisterWithMutableBody` when the body handler calls `SetRequestBody`,
`HTTPCallout`, or `HTTPCalloutAllSettled`. Use `RegisterWithBody` for read-only
body inspection.

**`HTTPCalloutAllSettled` for fan-out:**
When the example fans out to multiple upstream requests before returning one
synthesized response, use `Writer.HTTPCalloutAllSettled`. Do not use `w.Go` +
`w.Do` with `SendLocalResponse` — Envoy only honors local responses from filter
callbacks, not from goroutines.

---

## Body Callback Not Firing

Confirm the filter was registered with `RegisterWithBody` or
`RegisterWithMutableBody`. Confirm the request has a body (or that `endOfStream`
synthetic body callbacks are expected for the HTTP method under test).

---

## Fast Failure Triage

1. Rebuilt `.so` after edits? Do not use `TRANSIT_SKIP_BUILD=1` while
   debugging registration.
2. `Dynamic module ABI version ... matched` in Envoy stderr? If absent: check
   `ENVOY_DYNAMIC_MODULES_SEARCH_PATH`, module name, file location, build.
3. Temporary log before `RegisterTransitFilter`. Not appearing → `init()` failed
   before registration. Check env vars and config validation.
4. Temporary request-header marker:
   ```go
   w.SetRequestHeader("x-debug-filter-seen", "1")
   ```
   If the upstream does not see it, callbacks are not invoked. If it appears
   but body/local-response fails, move to body/callout debugging.
5. For Cluster Extension: add a log in `ChooseHost`. If it is never called, the
   cluster extension cluster is not being used for requests. Check EPP
   `Programmed=True` and the cluster name in `/config_dump`.
6. Remove all temporary markers before finishing.

---

## Common Symptoms

`Dynamic module ABI version matched`, request routes normally:
- Module loaded, filter not registered, or `filter_name` mismatch.
- Check `cmd/main.go` `init()`, env config, `filter_name`.

`no_healthy_upstream` + `bytes_received: 0` on a CLUSTER_PROVIDED cluster:
- `ChooseHost` returned nil. Check that the routing header is set on the
  **incoming** request at the time `ChooseHost` runs, not just on an outgoing
  call from a backend service.

`upstream connect error ... remote connection failure` with no body:
- Filter did not intercept before routing, or fallback cluster is unreachable.
- Prove the header callback ran first.

`Header callback marker appears, body callback does not`:
- Wrong `Register*` variant — needs `RegisterWithBody` or `RegisterWithMutableBody`.

`EnvoyPatchPolicy Programmed=True`, but Envoy pod logs "Failed to create
in-module cluster configuration":
- Module loaded but cluster config is invalid JSON or the registered factory
  name in `cluster_name` does not match `up.RegisterCluster`.

---

## Validation

```sh
make -C examples/<name> test
make -C examples/<name> e2e
git diff --check -- examples/<name>
```

For integrations:

```sh
make -C integrations/<name> test
KEEP_CLUSTER=1 make -C integrations/<name> e2e
```
