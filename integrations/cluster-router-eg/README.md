# Cluster Router Envoy Gateway Integration

This integration runs the `examples/cluster-router` dynamic module behind Envoy
Gateway in a local k3d cluster. It is the Kubernetes demo for request-aware
upstream selection with a Transit Cluster Extension.

The point of the demo is simple: Gateway API exposes one logical backend, while
Go code chooses the real upstream host per request. New model routes can appear
at runtime without adding a new Gateway route, a new static Envoy cluster, or a
second Envoy route selection pass.

## Scenario

Requests carry an `x-model` header.

Bootstrap config:

- `gpt-fast` routes to upstream A with OpenAI auth.
- `claude-safe` routes to upstream B with Anthropic auth.

Updated config:

- `gpt-slow` also routes to upstream A, but uses different OpenAI auth.
- `kimi-fast` routes to upstream C, which was not part of the bootstrap config.

The e2e proves both the data-plane behavior and the operational surface:

- Envoy Gateway runs a custom Envoy image that contains `libcluster-router.so`.
- EnvoyPatchPolicy patches Envoy Gateway generated xDS.
- Envoy loads the Transit dynamic module from the custom image.
- The Cluster Extension chooses hosts from Go.
- The upstream HTTP filter injects `authorization`, `x-llm-provider`, and
  `x-cluster-router-version`.
- The demo control plane updates routing config at runtime.
- The CLI can add models, dump redacted config, and send Gateway requests.

## Architecture

```text
CLI or client
  |
  v
Gateway API Gateway and HTTPRoute
  |
  v
Envoy Gateway translates Gateway API to xDS
  |
  v
EnvoyPatchPolicy patches the generated listener and backend cluster
  |
  v
Envoy proxy with /etc/envoy/dynamic-modules/libcluster-router.so
  |
  v
Transit Cluster Extension chooses upstream A, B, or C
  |
  v
Transit upstream HTTP filter injects provider headers
```

## Components

The integration creates:

- Envoy Gateway, installed by Helm.
- `EnvoyProxy`, pointing Envoy Gateway at the custom Envoy image.
- `GatewayClass`, `Gateway`, and `HTTPRoute`.
- `EnvoyPatchPolicy`, patching the generated Envoy listener and cluster.
- Demo control-plane Deployment and Service.
- Upstream A, B, and C Deployments and Services.

The custom Envoy image contains:

- `/usr/local/bin/envoy`
- `/etc/envoy/dynamic-modules/libcluster-router.so`
- Debian bookworm slim as the glibc userspace

The demo image contains one static Go binary. It runs in two modes:

- `cluster-router-demo control`
- `cluster-router-demo upstream`

The same binary also provides host-side CLI commands:

- `cluster-router-demo routes`
- `cluster-router-demo dump`
- `cluster-router-demo models add`
- `cluster-router-demo request`

## Why EnvoyPatchPolicy

Envoy Gateway owns the generated xDS. The demo keeps Gateway API as the source
of truth, then uses EnvoyPatchPolicy to replace the generated backend cluster
with a Transit Cluster Extension cluster.

The important generated names for Envoy Gateway v1.8 with `XDSNameSchemeV2`
are:

- backend cluster: `httproute/default/cluster-router/rule/0`
- listener: `tcp-80`
- route configuration: `http-80`

The e2e discovers the generated backend cluster from Envoy admin
`/config_dump`, then renders `k8s/epp.tmpl.yaml` with that name. The listener
patch targets `tcp-80`.

EnvoyPatchPolicy status stores conditions under
`status.ancestors[*].conditions`, so the e2e does not use generic
`kubectl wait envoypatchpolicy --for=condition=Programmed`. It polls those
ancestor conditions directly.

## Dynamic Module Loading

The Envoy image carries the `.so` directly:

```text
/etc/envoy/dynamic-modules/libcluster-router.so
```

`EnvoyProxy` configures:

- the custom Envoy image
- `GODEBUG=cgocheck=0`
- `ENVOY_DYNAMIC_MODULES_SEARCH_PATH=/etc/envoy/dynamic-modules`
- `dynamicModules` with local path
  `/etc/envoy/dynamic-modules/libcluster-router.so`

The search path matters. Without it, Envoy may accept the local module
declaration but later reject patched dynamic-module cluster config because it
tries to resolve `libcluster-router.so` from the process working directory.

## Refresh Loop Finding

The standalone `cluster-router` example can start its config fetch loop from
`ServerInitialized`. In the Envoy Gateway path, the patched cluster arrives via
CDS after Envoy is already running, so that callback is not a reliable place to
start live config refresh.

The working behavior starts the refresh loop from `Cluster.Init` and keeps
`ServerInitialized` as a second safe entry point. A guard prevents starting the
loop twice.

This is the key implementation detail that makes updated models work:

- control plane publishes `version: updated`
- Transit fetch loop consumes `/routes.json`
- scheduled host mutation adds any new hosts
- `gpt-slow` and `kimi-fast` route without Gateway API changes

## k3d Shape

The suite uses a single-node k3d cluster:

```sh
k3d cluster create transit-cluster-router-eg \
  --agents 0 \
  --image "rancher/k3s:${K3S_TAG}" \
  --k3s-arg "--disable=traefik@server:*" \
  --k3s-arg "--kubelet-arg=allowed-unsafe-sysctls=net.ipv4.ip_unprivileged_port_start@server:*"
```

Single-node works because the e2e uses port-forwarding for the Gateway and
Envoy admin endpoints. The generated Envoy service is configured as `ClusterIP`,
so the test does not depend on k3s ServiceLB or an external LoadBalancer IP.

Images are local by default:

- `transit-cluster-router-eg:<timestamp>`
- `transit-cluster-router-eg-control:<timestamp>`

They are imported into k3d with:

```sh
k3d image import -c transit-cluster-router-eg --mode direct ...
```

Use `make publish` only when a remote cluster needs to pull from ttl.sh.

## Context Safety

The e2e must never touch the user's current Kubernetes context by accident.

All Go helper calls inject:

```sh
kubectl --context k3d-transit-cluster-router-eg ...
helm --kube-context k3d-transit-cluster-router-eg ...
```

The helpers refuse non-k3d contexts. If the k3d cluster has already been
deleted, stop debugging instead of running bare `kubectl`.

## Build And Test

Run the fast integration unit tests:

```sh
make -C integrations/cluster-router-eg test
```

Run the full e2e:

```sh
make -C integrations/cluster-router-eg e2e
```

That target builds:

- `dist/libcluster-router.linux-<arch>.so`
- custom Envoy image
- Linux static demo binary
- demo control-plane/upstream image

Then it creates k3d, installs Envoy Gateway, applies Gateway resources, patches
Envoy xDS, and runs the routing assertions.

The underlying Go test is gated by `RUN_CLUSTER_ROUTER_EG_E2E=1`. That variable
is intentional friction: the test creates and deletes a real k3d cluster,
imports Docker images, installs Envoy Gateway, and applies Kubernetes
resources. A broad command such as `go test ./...` should compile the e2e
package but skip this local-cluster workflow.

You normally do not set `RUN_CLUSTER_ROUTER_EG_E2E=1` by hand. The `e2e` Make
target sets it for you:

```sh
RUN_CLUSTER_ROUTER_EG_E2E=1 \
IMAGE=... \
CONTROL_PLANE_IMAGE=... \
GOWORK=off go test ./e2e/... -v -timeout=15m -count=1
```

Reuse images after a successful build:

```sh
SKIP_IMAGE_BUILD=1 \
IMAGE=transit-cluster-router-eg:<tag> \
CONTROL_PLANE_IMAGE=transit-cluster-router-eg-control:<tag> \
make -C integrations/cluster-router-eg e2e
```

Keep the cluster after a run:

```sh
KEEP_CLUSTER=1 make -C integrations/cluster-router-eg e2e
```

Reuse a kept cluster instead of resetting it at test start:

```sh
KEEP_CLUSTER=1 RESET_CLUSTER=0 \
SKIP_IMAGE_BUILD=1 \
IMAGE=transit-cluster-router-eg:<tag> \
CONTROL_PLANE_IMAGE=transit-cluster-router-eg-control:<tag> \
make -C integrations/cluster-router-eg e2e
```

The default e2e still resets the cluster before starting. `KEEP_CLUSTER=1` only
affects teardown.

## Local k3d Demo

This is the shortest path for a live demo on a laptop. It lets the e2e suite do
the hard setup work, then keeps the cluster alive so the CLI can drive the
scenario.

1. Build the images, create k3d, install Envoy Gateway, and run the full
   scenario once.

   ```sh
   KEEP_CLUSTER=1 make -C integrations/cluster-router-eg e2e
   ```

   `KEEP_CLUSTER=1` is for demo ergonomics. The e2e normally deletes the k3d
   cluster during teardown, but the live demo needs the cluster to stay around
   for port-forwarding, CLI commands, logs, and config inspection. It still
   resets the cluster at the start of the run. Use `RESET_CLUSTER=0` only when
   you intentionally want to reuse an already-kept cluster.

   This builds the Transit `.so`, builds the custom Envoy image, builds the
   demo control-plane image, imports both images into k3d, installs Envoy
   Gateway, applies Gateway API resources, applies EnvoyPatchPolicy, and proves
   the routing behavior.

2. Build the host-side demo CLI.

   ```sh
   make -C integrations/cluster-router-eg cli
   ```

3. Confirm the k3d cluster is still the one you expect.

   ```sh
   k3d cluster list
   kubectl --context k3d-transit-cluster-router-eg get pods -A
   ```

   A healthy demo cluster should look roughly like this. Pod suffixes and ages
   will differ.

   ```text
   NAMESPACE              NAME                                             READY   STATUS    RESTARTS   AGE
   default                cluster-router-control-...                       1/1     Running   0          2m
   default                upstream-a-...                                   1/1     Running   0          2m
   default                upstream-b-...                                   1/1     Running   0          2m
   default                upstream-c-...                                   1/1     Running   0          2m
   envoy-gateway-system   envoy-gateway-...                                1/1     Running   0          3m
   envoy-gateway-system   envoy-default-cluster-router-...                 2/2     Running   0          2m
   kube-system            coredns-...                                      1/1     Running   0          3m
   kube-system            local-path-provisioner-...                       1/1     Running   0          3m
   kube-system            metrics-server-...                               1/1     Running   0          3m
   ```

   Keep using `--context k3d-transit-cluster-router-eg` for every manual
   Kubernetes command.

4. Forward the demo control plane.

   ```sh
   kubectl --context k3d-transit-cluster-router-eg -n default \
     port-forward service/cluster-router-control 19080:8080
   ```

5. Find the generated Envoy Gateway service.

   ```sh
   kubectl --context k3d-transit-cluster-router-eg -n envoy-gateway-system \
     get svc -l gateway.envoyproxy.io/owning-gateway-namespace=default,gateway.envoyproxy.io/owning-gateway-name=cluster-router
   ```

   In the current setup this is usually
   `service/envoy-default-cluster-router-8323ed73`, but Envoy Gateway can
   generate a different suffix.

6. Forward the generated Envoy Gateway service in a second terminal.

   ```sh
   kubectl --context k3d-transit-cluster-router-eg -n envoy-gateway-system \
     port-forward service/envoy-default-cluster-router-8323ed73 19081:80
   ```

   Replace the service name if step 5 returned a different one.

7. Inspect the bootstrap routes.

   ```sh
   ./integrations/cluster-router-eg/dist/cluster-router-demo routes \
     --control-url http://127.0.0.1:19080 | jq
   ```

   After the full e2e has run once, the control plane may already contain the
   updated demo routes. In that case, expect output like this:

   ```json
   {
     "version": "updated",
     "models": {
       "claude-safe": {
         "target": "upstream-b.default.svc.cluster.local:8080",
         "provider": "anthropic",
         "auth_header": "Bearer anthropic-token"
       },
       "gpt-fast": {
         "target": "upstream-a.default.svc.cluster.local:8080",
         "provider": "openai",
         "auth_header": "Bearer openai-token"
       },
       "gpt-slow": {
         "target": "upstream-a.default.svc.cluster.local:8080",
         "provider": "openai",
         "auth_header": "Bearer slow-token"
       },
       "kimi-fast": {
         "target": "upstream-c.default.svc.cluster.local:8080",
         "provider": "moonshot",
         "auth_header": "Bearer moonshot-token"
       }
     }
   }
   ```

8. Prove bootstrap routing through Envoy Gateway.

   ```sh
   ./integrations/cluster-router-eg/dist/cluster-router-demo request gpt-fast \
     --gateway-url http://127.0.0.1:19081 \
     --host cluster-router.example.com

   ./integrations/cluster-router-eg/dist/cluster-router-demo request claude-safe \
     --gateway-url http://127.0.0.1:19081 \
     --host cluster-router.example.com
   ```

   `gpt-fast` should reach upstream A. `claude-safe` should reach upstream B.

9. Add more routes through the control plane.

   If you started from `KEEP_CLUSTER=1 make -C integrations/cluster-router-eg
   e2e`, `gpt-slow` and `kimi-fast` are already present because the e2e added
   them. Add different names during the live demo so the audience can see new
   model entries appear without changing Gateway API or restarting Envoy.

   ```sh
   ./integrations/cluster-router-eg/dist/cluster-router-demo models add gemini-pro \
     --control-url http://127.0.0.1:19080 \
     --target upstream-b.default.svc.cluster.local:8080 \
     --provider google \
     --auth-header "Bearer google-token"

   ./integrations/cluster-router-eg/dist/cluster-router-demo models add qwen-coder \
     --control-url http://127.0.0.1:19080 \
     --target upstream-c.default.svc.cluster.local:8080 \
     --provider qwen \
     --auth-header "Bearer qwen-token"
   ```

10. Prove request-aware routing with the updated config.

    ```sh
    ./integrations/cluster-router-eg/dist/cluster-router-demo request gpt-slow \
      --gateway-url http://127.0.0.1:19081 \
      --host cluster-router.example.com

    ./integrations/cluster-router-eg/dist/cluster-router-demo request kimi-fast \
      --gateway-url http://127.0.0.1:19081 \
      --host cluster-router.example.com

    ./integrations/cluster-router-eg/dist/cluster-router-demo request gemini-pro \
      --gateway-url http://127.0.0.1:19081 \
      --host cluster-router.example.com

    ./integrations/cluster-router-eg/dist/cluster-router-demo request qwen-coder \
      --gateway-url http://127.0.0.1:19081 \
      --host cluster-router.example.com
    ```

    `gpt-slow` should reach upstream A with version `updated`.
    `kimi-fast` should reach upstream C with version `updated`. `gemini-pro`
    should reach upstream B and `qwen-coder` should reach upstream C. No Gateway
    API route or Envoy static cluster is added for any of these model entries.

11. Dump the active config without leaking bearer tokens.

    ```sh
    ./integrations/cluster-router-eg/dist/cluster-router-demo dump \
      --control-url http://127.0.0.1:19080
    ```

12. Reuse the cluster for another run without rebuilding images.

    ```sh
    KEEP_CLUSTER=1 RESET_CLUSTER=0 SKIP_IMAGE_BUILD=1 \
      make -C integrations/cluster-router-eg e2e
    ```

13. Delete the demo cluster when done.

    ```sh
    k3d cluster delete transit-cluster-router-eg
    ```

## CLI Demo

Build the host-side CLI:

```sh
make -C integrations/cluster-router-eg cli
```

Port-forward the demo control plane and generated Gateway service:

```sh
kubectl --context k3d-transit-cluster-router-eg -n default \
  port-forward service/cluster-router-control 19080:8080

kubectl --context k3d-transit-cluster-router-eg -n envoy-gateway-system \
  port-forward service/envoy-default-cluster-router-8323ed73 19081:80
```

The generated Envoy service name can change. Discover it with:

```sh
kubectl --context k3d-transit-cluster-router-eg -n envoy-gateway-system \
  get svc -l gateway.envoyproxy.io/owning-gateway-namespace=default,gateway.envoyproxy.io/owning-gateway-name=cluster-router
```

Read current config:

```sh
./integrations/cluster-router-eg/dist/cluster-router-demo routes \
  --control-url http://127.0.0.1:19080
```

Add updated model routes:

```sh
./integrations/cluster-router-eg/dist/cluster-router-demo models add gpt-slow \
  --control-url http://127.0.0.1:19080 \
  --target upstream-a.default.svc.cluster.local:8080 \
  --provider openai \
  --auth-header "Bearer slow-token"

./integrations/cluster-router-eg/dist/cluster-router-demo models add kimi-fast \
  --control-url http://127.0.0.1:19080 \
  --target upstream-c.default.svc.cluster.local:8080 \
  --provider moonshot \
  --auth-header "Bearer moonshot-token"
```

Send Gateway requests:

```sh
./integrations/cluster-router-eg/dist/cluster-router-demo request gpt-slow \
  --gateway-url http://127.0.0.1:19081 \
  --host cluster-router.example.com

./integrations/cluster-router-eg/dist/cluster-router-demo request kimi-fast \
  --gateway-url http://127.0.0.1:19081 \
  --host cluster-router.example.com
```

Dump redacted active control-plane config:

```sh
./integrations/cluster-router-eg/dist/cluster-router-demo dump \
  --control-url http://127.0.0.1:19080
```

The dump reports which models have auth configured, but does not print raw
bearer tokens.

## E2E Assertions

Bootstrap assertions:

- `x-model: gpt-fast` reaches upstream A.
- `x-model: claude-safe` reaches upstream B.
- upstream A receives `authorization: Bearer openai-token`.
- upstream B receives `authorization: Bearer anthropic-token`.
- both upstreams receive `x-cluster-router-version: bootstrap`.

Refresh assertions:

- `POST /models` adds `gpt-slow`.
- `POST /models` adds `kimi-fast`.
- `x-model: gpt-slow` eventually reaches upstream A.
- upstream A receives `authorization: Bearer slow-token`.
- upstream A receives `x-cluster-router-version: updated`.
- `x-model: kimi-fast` eventually reaches upstream C.
- upstream C receives `authorization: Bearer moonshot-token`.
- upstream C receives `x-cluster-router-version: updated`.

Debug assertions:

- dump contains `gpt-slow` and `kimi-fast`.
- dump does not contain raw bearer tokens.

## Debugging

Use only pinned k3d context commands:

```sh
kubectl --context k3d-transit-cluster-router-eg ...
```

Useful checks:

```sh
kubectl --context k3d-transit-cluster-router-eg get pods -A
kubectl --context k3d-transit-cluster-router-eg get gateway,httproute,envoyproxy,envoypatchpolicy -A
kubectl --context k3d-transit-cluster-router-eg get envoypatchpolicy cluster-router -o yaml
```

Port-forward Envoy admin and inspect `/config_dump`:

```sh
kubectl --context k3d-transit-cluster-router-eg -n envoy-gateway-system \
  port-forward deploy/envoy-default-cluster-router-8323ed73 19000:19000
```

Then:

```sh
curl -fsS http://127.0.0.1:19000/config_dump
```

Verify the `.so` is mapped into Envoy:

```sh
kubectl --context k3d-transit-cluster-router-eg -n envoy-gateway-system \
  exec deploy/envoy-default-cluster-router-8323ed73 -c envoy -- \
  sh -c 'grep libcluster-router /proc/1/maps || true'
```

If bootstrap routes work but updated models return `503 no healthy upstream`,
check the module debug endpoint:

```sh
./integrations/cluster-router-eg/dist/cluster-router-demo request gpt-fast \
  --gateway-url http://127.0.0.1:19081 \
  --host cluster-router.example.com \
  --path /__cluster-router/config
```

If that still shows `version: bootstrap` after `/routes.json` shows
`version: updated`, the refresh loop is not running or scheduled host mutation
is not being applied.

## Make Targets

```sh
make -C integrations/cluster-router-eg test
make -C integrations/cluster-router-eg cli
make -C integrations/cluster-router-eg image
make -C integrations/cluster-router-eg control-plane-image
make -C integrations/cluster-router-eg publish
make -C integrations/cluster-router-eg eg-install
make -C integrations/cluster-router-eg e2e
make -C integrations/cluster-router-eg clean
```

`eg-install` is an install smoke test. It only creates k3d, installs Envoy
Gateway, enables EnvoyPatchPolicy, and verifies CRDs/config. It does not build
images or run the cluster-router scenario.
