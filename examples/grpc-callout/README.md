# grpc-callout

Demonstrates `up.GRPCCallout` by calling Envoy's
`envoy.service.ratelimit.v3.RateLimitService/ShouldRateLimit` service from a
request body handler.

The filter uses the downstream request body as a rate-limit descriptor value:

- RLS `OK`: forward the original request to the configured upstream
- RLS `OVER_LIMIT`: return `429 rate limit exceeded`
- RLS callout/init/decode failure: fail open and forward the request

The Envoy cluster named `rls-service` must use HTTP/2 because gRPC runs over
HTTP/2.

## Build and test

```sh
make test
make e2e
```

## Run

```sh
make run
```

The checked-in `envoy.yaml` expects:

- an HTTP upstream on `127.0.0.1:10001`
- an h2c Envoy RLS v3 gRPC service on `127.0.0.1:10002`

Then send a request:

```sh
curl -i -X POST http://127.0.0.1:10000/ -d allow
```
