# Model-Based Cluster Extension Example

This example shows how to use transit's Cluster Extension API to choose
different upstream hosts from Go.

It is intentionally small. The cluster config defines a model-to-target map,
the module resolves those targets with Go DNS when the cluster initializes, and
each request selects a host by sending an `x-model` header.

This is DFP-like in the sense that the module owns host discovery and host
selection. It is not Envoy's built-in Dynamic Forward Proxy, and it does not try
to implement a DNS cache, eviction, or request-time host creation yet.

## Scenario

The Envoy cluster is configured with two model targets:

```json
{
  "timeout_millis": 500,
  "models": {
    "tiny": "localhost:8080",
    "large": "localhost:9090"
  }
}
```

When Envoy loads the cluster, the Go module:

1. Parses the `models` map.
2. Resolves each target with `net.DefaultResolver`.
3. Adds the resolved addresses to Envoy with `ClusterHandle.AddHosts`.
4. Marks those hosts healthy.
5. Stores the model-to-address mapping for request-time selection.

When a request arrives, the client chooses the model:

```sh
curl -H 'x-model: tiny' http://127.0.0.1:10000/
curl -H 'x-model: large' http://127.0.0.1:10000/
```

`ClusterLB.ChooseHost` reads `x-model`, looks up the resolved address for that
model, then returns Envoy's host pointer with `ClusterLBHandle.FindHostByAddress`.

The e2e test starts two local upstream servers and verifies that `tiny` and
`large` route to different hosts.

## Why Cluster Extension

LB Policy is the right API when Envoy already owns the host set and the module
only chooses among existing hosts.

Cluster Extension is the right API when the module owns the host set. This
example uses Cluster Extension because the Go code resolves model targets and
adds the hosts to Envoy.

## Request Body Selection

This first version uses `x-model` because ClusterLB callbacks can read request
headers directly. They cannot read the request body directly.

A later version can add a small HTTP filter before the router. That filter can
parse a JSON body such as:

```json
{"model":"tiny"}
```

and write the selected model into filter state or an internal request header.
The Cluster Extension can then read that value during `ChooseHost`. Keeping
that body-parsing layer separate makes the basic cluster behavior easier to
test and reason about.

## Files

- `cluster_dfp.go` contains the Cluster Extension implementation.
- `cmd/main.go` links the Envoy ABI implementation into the shared library.
- `envoy.yaml` is a runnable local Envoy config.
- `cluster_dfp_test.go` covers config parsing and Go DNS target resolution.
- `e2e/e2e_test.go` builds the shared library, starts Envoy, and verifies
  model-based routing.
- `e2e/testdata/envoy.tmpl.yaml` is embedded by the e2e test.

## Envoy Config

The Envoy cluster uses the dynamic module Cluster Extension and
`lb_policy: CLUSTER_PROVIDED`:

```yaml
clusters:
  - name: dfp-upstream
    connect_timeout: 5s
    lb_policy: CLUSTER_PROVIDED
    cluster_type:
      name: envoy.clusters.dynamic_modules
      typed_config:
        "@type": type.googleapis.com/envoy.extensions.clusters.dynamic_modules.v3.ClusterConfig
        dynamic_module_config:
          name: cluster-dfp
        cluster_name: go-dfp
        cluster_config:
          "@type": type.googleapis.com/google.protobuf.StringValue
          value: '{"timeout_millis":500,"models":{"tiny":"localhost:8080","large":"localhost:9090"}}'
```

`cluster_name: go-dfp` must match the registration in `cluster_dfp.go`:

```go
up.RegisterCluster("go-dfp", &dfpFactory{})
```

The shared library entrypoint in `cmd/main.go` blank-imports
`github.com/dio/transit/down/abi_impl`. Keep that import in the command package,
not in reusable library code, so ordinary Go tests do not link unresolved Envoy
callback symbols on Linux.

## Build

From the repository root:

```sh
make -C examples/cluster-dfp build
```

The shared library is written to `examples/cluster-dfp/libcluster-dfp.so`.

## Run

Start two local HTTP upstreams:

```sh
python3 -m http.server 8080
python3 -m http.server 9090
```

Then start Envoy:

```sh
make -C examples/cluster-dfp run
```

Send requests for different models:

```sh
curl -H 'x-model: tiny' http://127.0.0.1:10000/
curl -H 'x-model: large' http://127.0.0.1:10000/
```

Each request uses the same Envoy route and cluster. The Go Cluster Extension
chooses the concrete upstream host.

## Test

Unit tests:

```sh
make -C examples/cluster-dfp test
```

End-to-end test:

```sh
make -C examples/cluster-dfp e2e
```

The e2e test starts two upstream servers, renders the embedded
`envoy.tmpl.yaml`, starts Envoy, sends requests with `x-model: tiny` and
`x-model: large`, and checks that each request reaches the expected upstream.

## Limits

This example deliberately keeps the moving parts small:

- Model selection is passed in a request header.
- DNS happens during cluster initialization.
- There is no DNS refresh loop.
- There is no host eviction.
- Unknown models return no host, so Envoy fails the request.

Those limits are useful for a first example because they keep the focus on the
Cluster Extension lifecycle: parse config, add hosts, call `PreInitComplete`,
create a `ClusterLB`, and choose a host per request.
