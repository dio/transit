# lb-policy

This example implements a custom Envoy LB Policy named `first-host`.

Envoy still owns the cluster and host set. The module only chooses a host from
the healthy host list. The policy always returns priority `0`, index `0` when a
healthy host exists, and returns no host when the list is empty.

## What it shows

- `up.RegisterLBPolicy`
- the LB Policy extension point
- reading healthy host counts through `up.LBHandle`
- returning a priority and index to Envoy

## Run

The reference `envoy.yaml` expects an upstream service on `127.0.0.1:8080`.
Start any HTTP server there, then run Envoy from the repository root:

```sh
make build EXAMPLE=lb-policy EXAMPLE_CMD=./examples/lb-policy/cmd
ENVOY_DYNAMIC_MODULES_SEARCH_PATH=$PWD/dist \
GODEBUG=cgocheck=0 \
.bin/envoy -c examples/lb-policy/envoy.yaml
```

Then send a request through Envoy:

```sh
curl localhost:10000/
```

## Test

Unit tests:

```sh
cd examples && GOWORK=off go test ./lb-policy/...
```

End to end test:

```sh
make e2e-lb-policy
```

The e2e suite starts its own upstream server, so it does not need the manual
`127.0.0.1:8080` service used by the reference config.

## Files

- `lb_policy.go` contains the factory and policy implementation.
- `cmd/main.go` links the ABI layer and imports the example package.
- `envoy.yaml` shows the dynamic module LB policy config.
- `e2e/` verifies the policy against a real Envoy process.
