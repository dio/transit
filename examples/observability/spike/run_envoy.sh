#!/bin/bash
# Run Envoy with observability filter and OTLP configuration for investigation.
# This script sets up Envoy with fixed ports to make debugging easier.

set -e

cd "$(dirname "$0")/.."

echo "Building observability filter..."
make build

PROXY_PORT=9090
ADMIN_PORT=9901
BACKEND_PORT=9092
OTEL_PORT=9093

# Create config from template
cat > /tmp/envoy_obs_spike.yaml << CFGEOF
tracing:
  http:
    name: envoy.tracers.opentelemetry
    typed_config:
      "@type": type.googleapis.com/envoy.config.trace.v3.OpenTelemetryConfig
      grpc_service:
        envoy_grpc:
          cluster_name: otel-collector
      service_name: "observability-spike"

stats_flush_interval: 1s
stats_sinks:
  - name: envoy.stat_sinks.open_telemetry
    typed_config:
      "@type": type.googleapis.com/envoy.extensions.stat_sinks.open_telemetry.v3.SinkConfig
      grpc_service:
        envoy_grpc:
          cluster_name: otel-collector
      report_counters_as_deltas: false

static_resources:
  listeners:
    - name: observability
      address:
        socket_address: { address: 127.0.0.1, port_value: $PROXY_PORT }
      filter_chains:
        - filters:
            - name: envoy.filters.network.http_connection_manager
              typed_config:
                "@type": type.googleapis.com/envoy.extensions.filters.network.http_connection_manager.v3.HttpConnectionManager
                stat_prefix: observability
                generate_request_id: true
                tracing:
                  client_sampling: { value: 100 }
                  random_sampling: { value: 100 }
                  overall_sampling: { value: 100 }
                access_log:
                  - name: envoy.access_loggers.open_telemetry
                    typed_config:
                      "@type": type.googleapis.com/envoy.extensions.access_loggers.open_telemetry.v3.OpenTelemetryAccessLogConfig
                      grpc_service:
                        envoy_grpc:
                          cluster_name: otel-collector
                      log_name: observability
                      attributes:
                        values:
                          - key: status_code
                            value:
                              string_value: "%DYNAMIC_METADATA(observability:status_code)%"
                          - key: model
                            value:
                              string_value: "%DYNAMIC_METADATA(observability:model)%"
                http_filters:
                  - name: observability
                    typed_config:
                      "@type": type.googleapis.com/envoy.extensions.filters.http.dynamic_modules.v3.DynamicModuleFilter
                      dynamic_module_config:
                        name: observability
                      filter_name: observability
                      filter_config:
                        "@type": type.googleapis.com/google.protobuf.StringValue
                        value: '{}'
                  - name: envoy.filters.http.router
                    typed_config:
                      "@type": type.googleapis.com/envoy.extensions.filters.http.router.v3.Router
                route_config:
                  name: observability
                  virtual_hosts:
                    - name: local
                      domains: ["*"]
                      routes:
                        - match: { prefix: "/" }
                          route: { cluster: backend }

  clusters:
    - name: backend
      connect_timeout: 5s
      type: STATIC
      load_assignment:
        cluster_name: backend
        endpoints:
          - lb_endpoints:
              - endpoint:
                  address:
                    socket_address: { address: 127.0.0.1, port_value: $BACKEND_PORT }

    - name: otel-collector
      connect_timeout: 5s
      type: STATIC
      typed_extension_protocol_options:
        envoy.extensions.upstreams.http.v3.HttpProtocolOptions:
          "@type": type.googleapis.com/envoy.extensions.upstreams.http.v3.HttpProtocolOptions
          explicit_http_config:
            http2_protocol_options: {}
      load_assignment:
        cluster_name: otel-collector
        endpoints:
          - lb_endpoints:
              - endpoint:
                  address:
                    socket_address: { address: 127.0.0.1, port_value: $OTEL_PORT }

admin:
  address:
    socket_address: { address: 127.0.0.1, port_value: $ADMIN_PORT }
CFGEOF

echo "Config written to /tmp/envoy_obs_spike.yaml"
echo ""
echo "Starting backend server on port $BACKEND_PORT..."
python3 -m http.server $BACKEND_PORT > /tmp/backend_spike.log 2>&1 &
BACKEND_PID=$!
sleep 1

echo ""
echo "Starting Envoy..."
echo "  Proxy:      http://127.0.0.1:$PROXY_PORT"
echo "  Admin:      http://127.0.0.1:$ADMIN_PORT"
echo "  OTEL:       127.0.0.1:$OTEL_PORT"
echo "  Backend:    127.0.0.1:$BACKEND_PORT"
echo ""
echo "Useful endpoints:"
echo "  /stats:        curl http://127.0.0.1:$ADMIN_PORT/stats"
echo "  /stats/prometheus: curl http://127.0.0.1:$ADMIN_PORT/stats/prometheus"
echo "  /config_dump:  curl http://127.0.0.1:$ADMIN_PORT/config_dump"
echo ""
echo "Make requests:"
echo "  curl -i http://127.0.0.1:$PROXY_PORT/"
echo "  curl -i -H 'x-model: claude-opus' http://127.0.0.1:$PROXY_PORT/"
echo ""

trap "kill $BACKEND_PID 2>/dev/null || true" EXIT

exec env GODEBUG=cgocheck=0 /Users/dio/src/dio/transit2/.bin/envoy -c /tmp/envoy_obs_spike.yaml -l info
