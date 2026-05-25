---
name: transit-k3d-envoy-gateway-e2e
description: Build and debug Transit integration e2e suites that run Envoy Gateway in k3d with Gateway API, EnvoyPatchPolicy, custom Envoy images, and dynamic modules.
---

# Transit k3d Envoy Gateway E2E

Use this skill when adding or debugging an integration under `integrations/`
that needs a real k3d cluster, Envoy Gateway, Gateway API resources, an
EnvoyPatchPolicy, or a custom Envoy image carrying a Transit `.so`.

All tests and checks for these suites must run outside the Codex sandbox. This
includes `make -C integrations/<name> test`, gated k3d e2e targets, direct
`go test` commands for integration packages, `kubectl`, `helm`, `k3d`, Docker
image builds/imports, and any command that touches the local kube context or
container runtime.

## Shape of the suite

Prefer a Go `testify/suite` harness as the canonical e2e entry point. Avoid a
second shell implementation of the same install flow; route Make targets such as
`eg-install` through a small gated Go smoke test instead. Shell scripts are only
for manual checkpointing or debugging that the Go suite does not already own.

The base suite should:

- create a bounded `context.Context` in `SetupSuite`;
- resolve the repo root and integration directory once;
- create a fresh k3d cluster for each run;
- install Envoy Gateway;
- apply the Envoy Gateway config that enables EnvoyPatchPolicy;
- wait for the Envoy Gateway deployment, not arbitrary pods;
- verify the required CRDs and runtime config;
- delete the cluster in `TearDownSuite` unless `KEEP_CLUSTER=1`.

Put the reusable harness in a visible file such as `e2e/suite_test.go`, then
embed it from scenario suites:

```go
type clusterRouterSuite struct {
    envoyGatewaySuite

    envoyImage   string
    controlImage string
}
```

## k3d setup

Use the same k3d flags in bash and Go so manual debugging matches CI behavior:

```text
k3d cluster create <test-name>
  --agents 0
  --image rancher/k3s:<tag>
  --k3s-arg --disable=traefik@server:*
  --k3s-arg --kubelet-arg=allowed-unsafe-sysctls=net.ipv4.ip_unprivileged_port_start@server:*
```

Wait for `nodes/k3d-<test-name>-server-0` to become Ready before installing
Envoy Gateway. Use `--agents 1` only when the scenario needs a LoadBalancer
external IP or multi-node behavior. Transit integration suites that use
port-forwarding should stay single-node so image import is faster and failures
are easier to inspect.

## Envoy Gateway install

Never run bare `kubectl` or `helm` in these suites. Pin every Kubernetes command
to the expected k3d context:

```text
kubectl --context k3d-<test-name> ...
helm --kube-context k3d-<test-name> ...
```

The Go e2e helpers should reject contexts that do not start with `k3d-`. Manual
debug commands should do the same. If the k3d cluster has already been deleted,
stop debugging rather than falling through to the user's current kube context.

Install Envoy Gateway with Helm, then wait with deployment-level checks:

```text
kubectl --context k3d-<test-name> rollout status deployment/envoy-gateway -n envoy-gateway-system --timeout=120s
kubectl --context k3d-<test-name> wait deployment/envoy-gateway -n envoy-gateway-system --for=condition=Available --timeout=120s
```

Apply the integration's `envoy-gateway-config.yaml`, restart Envoy Gateway, and
wait again. The config should enable:

- `extensionApis.enableEnvoyPatchPolicy: true`
- `runtimeFlags.enabled: XDSNameSchemeV2`

Verify these CRDs before applying scenario resources:

- `gatewayclasses.gateway.networking.k8s.io`
- `gateways.gateway.networking.k8s.io`
- `httproutes.gateway.networking.k8s.io`
- `envoyproxies.gateway.envoyproxy.io`
- `envoypatchpolicies.gateway.envoyproxy.io`

For Envoy Gateway v1.8 with `XDSNameSchemeV2`, expect generated names like:

- backend cluster: `httproute/<namespace>/<httproute>/rule/0`
- listener: `tcp-80`
- route configuration: `http-80`

If an integration discovers generated backend cluster names from Envoy
`/config_dump` and feeds those names into a dynamic-module config, allow the
slash-containing `httproute/.../rule/...` value through. Do not reuse validation
intended for logical cluster aliases that reject `/`, or the Envoy pod can load
an invalid module config even though discovery succeeded.

Poll `EnvoyPatchPolicy` programmed status through
`status.ancestors[*].conditions`; generic `kubectl wait
envoypatchpolicy/... --for=condition=Programmed` can miss the nested condition.

## Gateway Namespace mode

When an integration enables Gateway Namespace mode through the Envoy Gateway
runtime config, make the namespace and RBAC assumptions explicit in manifests.

The controller namespace and dataplane namespace should both be watched. If the
watch list only includes the dataplane namespace, Envoy Gateway can fail to read
its own TLS secret and Gateways may be Accepted but not Programmed:

```yaml
provider:
  kubernetes:
    deploy:
      type: GatewayNamespace
    watch:
      type: Namespaces
      namespaces:
      - transit-system
      - transit-dataplane
```

The Envoy Gateway service account also needs dataplane namespace infra-manager
permissions when Helm did not create namespace-specific RBAC. Mirror the chart's
infra-manager Role shape for serviceaccounts, services, configmaps,
deployments, daemonsets, HPAs, PDBs, and clustertrustbundles in the dataplane
namespace, then bind it to `system-namespace/envoy-gateway`.

Data-plane Envoy pods authenticate to xDS with service-account JWTs. If Envoy
pods exist but stay unready and Envoy Gateway logs contain
`tokenreviews.authentication.k8s.io is forbidden`, add cluster-scope
`tokenreviews` `create` permission for the Envoy Gateway service account.

Prefer stable EnvoyProxy-generated Service names and pod labels instead of
hand-written Services that duplicate generated selectors:

```yaml
provider:
  kubernetes:
    envoyService:
      name: l1
      type: ClusterIP
      labels:
        transit.dio/proxy: l1
    envoyDeployment:
      pod:
        labels:
          transit.dio/proxy: l1
```

Use the stable service name for port-forwarding and the stable pod label for
readiness checks. For shared L2 deployments with logical shard Services, label
the alias Services with shard identity and select the shared L2 pod label. Only
use per-shard pod labels such as `transit.dio/proxy: l2-a` when the integration
creates separate physical L2 EnvoyProxy deployments.

After mutating a generated Envoy deployment, prefer deployment-level rollout
checks over broad pod-label waits. Stable labels such as `transit.dio/proxy=l1`
can match old ReplicaSet pods during or after a restart; `kubectl wait pods -l
...` can then fail even when the new deployment replica is ready. Use
`kubectl rollout status deployment/<name>` plus `kubectl wait deployment/<name>
--for=condition=Available` for the rollout, and use pod-label waits only for
initial readiness or labels that cannot include stale pods.

Do not patch generated Envoy Gateway deployments as the durable configuration
source. Envoy Gateway owns those deployments and can reconcile manual
`kubectl set env deployment/<generated>` changes away. When dynamic-module env
or image config must change, update and apply the owning `EnvoyProxy` resource,
then wait for the generated deployment rollout.

## Images and modules

For Transit dynamic-module scenarios, keep the `.so` inside the custom Envoy
image. Use local image tags by default and reserve ttl.sh or registry tags for
explicit publish/demo flows. Import locally built images into k3d before
applying resources:

```text
k3d image import -c <cluster> --mode direct <envoy-image> <demo-or-control-image>
```

For CI, ttl.sh can be simpler than local import. Push short-lived image tags,
pass those tags as `IMAGE` and `CONTROL_PLANE_IMAGE`, and set
`K3D_SKIP_IMAGE_IMPORT=1` so k3d pulls from the registry instead of importing
from the runner Docker daemon. Keep local demos on direct import because it is
faster and works offline after the base images are present.

Build the image from the repo root when the Dockerfile needs `./dist`. Do not
copy generated `.so` or `.h` artifacts into the integration directory. For Go
control-plane or upstream demo services, build the Linux binary locally with
`CGO_ENABLED=0`, then wrap it with a copy-only minimal runtime Dockerfile. Avoid
multi-stage Docker builds for these services unless the suite specifically
needs a containerized build environment.

Each runnable integration should follow the same packaging contract as
`integrations/tiered-router-eg` and `integrations/cluster-router-eg` unless
there is a clear reason not to:

- `Dockerfile.envoy` copies all Linux `.so` files needed by the scenario into
  `/etc/envoy/dynamic-modules`.
- `Dockerfile.demo` or an equivalent image contains the fake upstream/control
  binaries used by the suite.
- The integration `Makefile` exposes `build-modules`, `image`, a demo/control
  binary target, a demo/control image target, `publish`, `eg-install`, `e2e`,
  and `clean`.
- `make -C integrations/<name> e2e` builds the local images by default, imports
  them into k3d, and passes `IMAGE` plus the demo/control image env vars to the
  Go suite.
- `SKIP_IMAGE_BUILD=1 IMAGE=... <DEMO_OR_CONTROL_IMAGE>=... make -C
  integrations/<name> e2e` reuses prebuilt images.
- `publish` retags images to short-lived ttl.sh names and pushes them; CI can
  then set `K3D_SKIP_IMAGE_IMPORT=1` so the cluster pulls images instead of
  importing from the Docker daemon.

If a new integration only documents required `IMAGE` values but has no local
build path, treat it as a skeleton rather than a working example.

Set `ENVOY_DYNAMIC_MODULES_SEARCH_PATH=/etc/envoy/dynamic-modules` on the
generated Envoy pod when the module file lives there. The `dynamicModules.local`
path can make Envoy mount the file, but patched dynamic-module clusters may
still resolve `lib<name>.so` through the process search path.

For Cluster Extension config refresh in an Envoy Gateway/CDS path, do not rely
only on `ServerInitialized`. The cluster may be created after Envoy has already
started. Start refresh work from `Cluster.Init` and keep `ServerInitialized` as
a guarded second entry point.

## Backend clusters and EPP

EG v1.8 does **not** create a CDS cluster for an HTTPRoute backend when there
are no ready endpoints. This makes EPP `replace` patches fail with
`ResourceNotFound`. The fix is to give EG a ready endpoint so it generates the
cluster, which the EPP can then replace.

**Correct pattern**: use a `registry.k8s.io/pause:3.9` placeholder Deployment.
The pause container starts in < 1 s, has no readiness probe (ready immediately),
and is selected by the backend Service. EG generates the cluster from the pause
pod's endpoint; the EPP replaces the entire cluster with whatever the integration
needs (e.g. a STATIC loopback). No real traffic ever reaches the placeholder.

```yaml
apiVersion: apps/v1
kind: Deployment
metadata:
  name: ws-proxy-backend
spec:
  replicas: 1
  selector:
    matchLabels:
      app: ws-proxy-backend
  template:
    metadata:
      labels:
        app: ws-proxy-backend
    spec:
      containers:
      - name: pause
        image: registry.k8s.io/pause:3.9
        ports:
        - containerPort: 8080
```

Always `waitDeployment` on the placeholder before calling
`waitEnvoyPatchPolicyProgrammed`. If the EPP is applied before the pause pod is
ready, EG has not yet generated the cluster and the patch will fail with
`ResourceNotFound`, then never recover without a re-apply.

**Static EndpointSlices do not work**: manually created EndpointSlices with
`conditions.ready: true` are not sufficient — EG v1.8 still reports "no ready
endpoints" and generates a `direct_response: 503` route instead of a cluster.
A real running pod is required.

**Loopback addresses are rejected**: Kubernetes rejects `127.0.0.0/8` and
`::1/128` in EndpointSlice addresses. RFC 5737 test addresses (`192.0.2.x`)
are valid syntactically, but still will not satisfy EG's endpoint readiness
check. Use a real pod.

## WebSocket dials in Go test code

When dialing a WebSocket through a port-forward (URL host is `127.0.0.1:<port>`)
but the virtual host in Envoy is a hostname (e.g. `ws-proxy.example.com`), use
`DialOptions.Host` — not `HTTPHeader: http.Header{"Host": {...}}`. Go's
`net/http` ignores `Header["Host"]`; the correct override path is `req.Host`,
which the `coder/websocket` library maps from `DialOptions.Host`.

```go
// Wrong — Host header silently ignored by net/http
conn, _, err := websocket.Dial(ctx, wsURL, &websocket.DialOptions{
    HTTPHeader: http.Header{"Host": {gatewayHost}},
})

// Correct
conn, _, err := websocket.Dial(ctx, wsURL, &websocket.DialOptions{
    Host: gatewayHost,
})
```

The same applies to `http.NewRequest`: set `req.Host = hostname` (not
`req.Header.Set("Host", ...)`).

## Assertions

Prefer black-box assertions through the Gateway listener. Use admin
`/config_dump` only to discover or debug Envoy-generated names such as backend
cluster names for EnvoyPatchPolicy.

Keep skipped parity/future assertions from short-circuiting active topology
checks. In `testify/suite`, `s.T().Skip` skips the current test method, so do
not call a pending stub before implemented assertions in the same test. Put
pending cases in separate `t.Run` subtests, move them after active assertions,
or keep them as TODO comments until they are runnable.

For request-aware routing demos, assert both the externally visible upstream
selection and the active configuration surface. For example:

- request for `gpt-slow` reaches upstream A with the updated version header;
- request for `kimi-fast` reaches upstream C with the updated version header;
- `/dump` shows the active model names but does not leak bearer tokens.

Use bounded retry loops for eventually consistent config propagation. Avoid
plain sleeps.

Prefer a small CLI for demo actions instead of documenting raw curl commands.
The CLI should talk to the control plane for model updates and dumps, and to the
Gateway for request checks. Keep cluster lifecycle in Makefiles/manifests unless
there is an explicit deploy-command requirement.

## Debugging order

When the suite fails after Envoy Gateway is installed:

1. Check Gateway and HTTPRoute status.
2. Check EnvoyProxy and EnvoyPatchPolicy status.
3. If EPP reports `Programmed=False:ResourceNotFound` for a cluster type, EG
   has not generated that cluster. Check `kubectl get httproute -o yaml` for
   `BackendsAvailable=False:EndpointsNotFound`. If so, the backend has no ready
   endpoints — add a `pause` placeholder Deployment (see Backend clusters section).
4. Port-forward Envoy admin and inspect `/config_dump`.
5. Confirm the generated cluster name targeted by EnvoyPatchPolicy.
6. Check the custom Envoy pod logs for dynamic module load errors.
   EnvoyPatchPolicy can report `Programmed=True` even when Envoy later rejects
   a dynamic-module cluster update. Look for messages such as
   `Failed to create in-module cluster configuration` or module-specific parse
   errors.
7. If Envoy pods are created but not Ready, inspect Envoy Gateway logs for xDS
   authentication errors such as missing `tokenreviews` permission.
8. Keep the cluster with `KEEP_CLUSTER=1` only for interactive debugging. Run
   `make -C integrations/<name> eg-install KEEP_CLUSTER=1` to get a stable
   base cluster without the image import, then apply resources manually.

## WebSocket tiered proxy debugging

This section covers failure patterns specific to integrations that use
`ws-proxy-eg` or `tiered-ws-proxy-eg` shapes (embedded ws-proxy server,
two-listener L2, `WSPROXY_EGRESS_URL`).

### accept-before-dial: Gate 1 passes but Gate 2 fails on first Read

`WSProxy.ServeHTTP` calls `websocket.Accept` (sends `101`) **before** dialing
the egress URL. A Gate that only checks the WS dial will pass even when the
egress dial fails.

```
Gate 1: conn, _, err := websocket.Dial(...)  ← returns nil (101 received)
Gate 2: _, _, err = conn.Read(...)           ← returns StatusInternalError "upstream unavailable"
```

If Gate 2 fails immediately on the **first** `Read` before any frame was
written, the egress URL is wrong — not the L1→L2 inbound path. Common causes:

| Symptom at Gate 2 | Root cause |
|-------------------|-----------|
| `StatusInternalError / upstream unavailable` | Egress dial failed: wrong URL, 426 from Envoy (missing `upgrade_configs` on egress listener), or egress cluster has no endpoints |
| `StatusInternalError / upstream unavailable` immediately after 101 | `WSPROXY_EGRESS_URL` includes path → double-path URL → Envoy has no route |
| 426 on Gate 1 | `upgrade_configs` missing on L1 inbound listener or L2 inbound listener |
| 503 on Gate 1 | EDS cluster has no endpoints; EPP applied before pause placeholder was ready |

### WSPROXY_EGRESS_URL must be a base URL

`ws_proxy.go` constructs the upstream URL as `egressURL + r.URL.Path`. Set:

```yaml
# Correct
- name: WSPROXY_EGRESS_URL
  value: "ws://127.0.0.1:10002"

# Wrong — causes double-path ws://127.0.0.1:10002/v1/responses/v1/responses
- name: WSPROXY_EGRESS_URL
  value: "ws://127.0.0.1:10002/v1/responses"
```

### SKIP_IMAGE_BUILD timestamp mismatch

`IMAGE_TAG := $(shell date +%s)` in the integration Makefile is evaluated at
`make` start time. `SKIP_IMAGE_BUILD=1` skips the build but does **not** freeze
the tag — a fresh timestamp is generated. The resulting image names do not exist
in Docker; `k3d image import` fails.

Always pass explicit image names when reusing a previous build:

```sh
make -C integrations/tiered-ws-proxy-eg e2e SKIP_IMAGE_BUILD=1 \
  L1_IMAGE=transit-tiered-ws-proxy-l1:<tag> \
  L2_IMAGE=transit-tiered-ws-proxy-l2:<tag> \
  MOCK_IMAGE=transit-tiered-ws-proxy-mock:<tag>
```

### Two-listener Gateway for WS egress

After `101 Switching Protocols` Envoy becomes a transparent TCP tunnel — EPP
patches and upstream filters cannot touch WS frames. To keep Envoy ownership
of TLS and credentials on the upstream hop, add a second HTTP listener to
the L2 Gateway:

```yaml
listeners:
- name: inbound   # receives WS from L1
  protocol: HTTP
  port: 80
- name: egress    # receives WS from embedded server
  protocol: HTTP
  port: 10002
```

EG generates `tcp-80` and `tcp-10002` from the same pod. EPP patches both.
The embedded server dials `ws://127.0.0.1:10002` (base URL only). This is the
only pattern that restores Envoy ownership of the upstream WS connection without
patching Envoy core.

### config_dump verification for L2 patches

Poll `/config_dump` on the L2 Envoy admin port after EPP is applied. A
fully-patched L2 must show all of:

- `DynamicModuleFilter` with `filter_name: ws-proxy` in `tcp-80` filter chain
- `127.0.0.1:10001` in a STATIC cluster (inbound → embedded server)
- `10002` bound as a listener port (egress listener present)
- `upgrade_type: websocket` in both `tcp-80` and `tcp-10002`

The `waitL2ConfigApplied` helper in `tiered-ws-proxy-eg/e2e/e2e_test.go` checks
these conditions (60s deadline, 500ms poll). If it times out, one of the six L2
EPP patches did not land — inspect the patch paths against the running Envoy
version's xDS schema.

## Make targets

All Go e2e targets must use `-count=1` so a cached test result cannot skip a
real cluster run.

Use environment gates for expensive suites:

```text
RUN_CLUSTER_ROUTER_EG_INSTALL=1 go test ./e2e/... -run TestEnvoyGatewayInstallOnly -count=1
RUN_CLUSTER_ROUTER_EG_E2E=1 go test ./e2e/... -run TestClusterRouterEnvoyGateway -count=1
```
