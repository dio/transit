# Cluster Extension Example

This example shows a dynamic module that owns an Envoy cluster. The module reads
a small host list from config, adds those hosts during cluster initialization,
marks them healthy, and returns a `ClusterLB` that selects the first healthy
host.

It demonstrates:

- `up.RegisterCluster`
- `ClusterFactory.Create`
- `Cluster.Init`
- `ClusterHandle.AddHosts`
- `ClusterHandle.PreInitComplete`
- `ClusterLB.ChooseHost`

## Build

From the repository root:

```sh
make -C examples/cluster build
```

The shared library is written to `examples/cluster/libcluster.so`.

## Run

Start any local HTTP upstream on port `8080`, then run Envoy with the example
config:

```sh
make -C examples/cluster run
```

Requests to `http://127.0.0.1:10000/` route through the dynamic module cluster
to `127.0.0.1:8080`.

## Test

Unit tests cover config parsing and pure Go host selection:

```sh
make -C examples/cluster test
```

The e2e test builds `libcluster.so`, starts a local upstream, starts Envoy, and
checks that traffic reaches the upstream through the Cluster Extension:

```sh
make -C examples/cluster e2e
```
