---
name: transit-k3d-envoy-gateway-e2e
description: Build and debug Transit integration e2e suites that run Envoy Gateway in k3d with Gateway API, EnvoyPatchPolicy, custom Envoy images, and dynamic modules.
---

# Transit k3d Envoy Gateway E2E

Use this skill when adding or debugging an integration under `integrations/`
that needs a real k3d cluster, Envoy Gateway, Gateway API resources, an
EnvoyPatchPolicy, or a custom Envoy image carrying a Transit `.so`.

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

Poll `EnvoyPatchPolicy` programmed status through
`status.ancestors[*].conditions`; generic `kubectl wait
envoypatchpolicy/... --for=condition=Programmed` can miss the nested condition.

## Images and modules

For Transit dynamic-module scenarios, keep the `.so` inside the custom Envoy
image. Use local image tags by default and reserve ttl.sh or registry tags for
explicit publish/demo flows. Import locally built images into k3d before
applying resources:

```text
k3d image import -c <cluster> --mode direct <envoy-image> <demo-or-control-image>
```

Build the image from the repo root when the Dockerfile needs `./dist`. Do not
copy generated `.so` or `.h` artifacts into the integration directory. For Go
control-plane or upstream demo services, build the Linux binary locally with
`CGO_ENABLED=0`, then wrap it with a copy-only minimal runtime Dockerfile. Avoid
multi-stage Docker builds for these services unless the suite specifically
needs a containerized build environment.

Set `ENVOY_DYNAMIC_MODULES_SEARCH_PATH=/etc/envoy/dynamic-modules` on the
generated Envoy pod when the module file lives there. The `dynamicModules.local`
path can make Envoy mount the file, but patched dynamic-module clusters may
still resolve `lib<name>.so` through the process search path.

For Cluster Extension config refresh in an Envoy Gateway/CDS path, do not rely
only on `ServerInitialized`. The cluster may be created after Envoy has already
started. Start refresh work from `Cluster.Init` and keep `ServerInitialized` as
a guarded second entry point.

## Assertions

Prefer black-box assertions through the Gateway listener. Use admin
`/config_dump` only to discover or debug Envoy-generated names such as backend
cluster names for EnvoyPatchPolicy.

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
3. Port-forward Envoy admin and inspect `/config_dump`.
4. Confirm the generated cluster name targeted by EnvoyPatchPolicy.
5. Check the custom Envoy pod logs for dynamic module load errors.
6. Keep the cluster with `KEEP_CLUSTER=1` only for interactive debugging.

## Make targets

All Go e2e targets must use `-count=1` so a cached test result cannot skip a
real cluster run.

Use environment gates for expensive suites:

```text
RUN_CLUSTER_ROUTER_EG_INSTALL=1 go test ./e2e/... -run TestEnvoyGatewayInstallOnly -count=1
RUN_CLUSTER_ROUTER_EG_E2E=1 go test ./e2e/... -run TestClusterRouterEnvoyGateway -count=1
```
