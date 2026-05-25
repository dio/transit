# header-router

Demonstrates `SetUpstreamOverrideHost`. The filter reads an `x-route-to`
request header and routes the request to backend A (`x-route-to: a`) or
backend B (`x-route-to: b`). Requests without the header use Envoy's default
load balancer.

Backend addresses are configured via `HEADER_ROUTER_HOST_A` and
`HEADER_ROUTER_HOST_B` environment variables.

## Build and run

    make build
    HEADER_ROUTER_HOST_A=127.0.0.1:8080 \
    HEADER_ROUTER_HOST_B=127.0.0.1:8081 \
    make run
