// Package main renders envoy.tmpl.yaml with default ports for manual inspection.
// Run: go run ./examples/tiered-single-envoy/cmd > /tmp/envoy.yaml
package main

import (
	"fmt"
	"os"
	"text/template"
)

// defaultPorts matches the port layout in docs/tiered-single-envoy.md.
var defaultPorts = map[string]int{
	"AdminPort":    9901,
	"L1Port":       10000,
	"L2APort":      10001,
	"L2BPort":      10002,
	"BackendAPort": 18081,
	"BackendBPort": 18082,
}

const envoyTmpl = `admin:
  address:
    socket_address: { address: 127.0.0.1, port_value: {{.AdminPort}} }

static_resources:
  listeners:
    - name: l1
      address:
        socket_address: { address: 127.0.0.1, port_value: {{.L1Port}} }
      filter_chains:
        - filters:
            - name: envoy.filters.network.http_connection_manager
              typed_config:
                "@type": type.googleapis.com/envoy.extensions.filters.network.http_connection_manager.v3.HttpConnectionManager
                stat_prefix: l1
                http_filters:
                  - name: cluster-shard-router-debug
                    typed_config:
                      "@type": type.googleapis.com/envoy.extensions.filters.http.dynamic_modules.v3.DynamicModuleFilter
                      dynamic_module_config:
                        name: cluster-shard-router
                      filter_name: cluster-shard-router-debug
                  - name: envoy.filters.http.router
                    typed_config:
                      "@type": type.googleapis.com/envoy.extensions.filters.http.router.v3.Router
                route_config:
                  name: l1
                  virtual_hosts:
                    - name: local
                      domains: ["*"]
                      routes:
                        - match: { prefix: "/" }
                          route: { cluster: l1 }

    - name: l2-a
      address:
        socket_address: { address: 127.0.0.1, port_value: {{.L2APort}} }
      filter_chains:
        - filters:
            - name: envoy.filters.network.http_connection_manager
              typed_config:
                "@type": type.googleapis.com/envoy.extensions.filters.network.http_connection_manager.v3.HttpConnectionManager
                stat_prefix: l2_a
                http_filters:
                  - name: cluster-router-debug
                    typed_config:
                      "@type": type.googleapis.com/envoy.extensions.filters.http.dynamic_modules.v3.DynamicModuleFilter
                      dynamic_module_config:
                        name: cluster-router
                      filter_name: cluster-router-l2a-debug
                      filter_config:
                        "@type": type.googleapis.com/google.protobuf.StringValue
                        value: '{}'
                  - name: envoy.filters.http.router
                    typed_config:
                      "@type": type.googleapis.com/envoy.extensions.filters.http.router.v3.Router
                route_config:
                  name: l2-a
                  virtual_hosts:
                    - name: local
                      domains: ["*"]
                      routes:
                        - match: { prefix: "/" }
                          route: { cluster: l2-a }

    - name: l2-b
      address:
        socket_address: { address: 127.0.0.1, port_value: {{.L2BPort}} }
      filter_chains:
        - filters:
            - name: envoy.filters.network.http_connection_manager
              typed_config:
                "@type": type.googleapis.com/envoy.extensions.filters.network.http_connection_manager.v3.HttpConnectionManager
                stat_prefix: l2_b
                http_filters:
                  - name: cluster-router-debug
                    typed_config:
                      "@type": type.googleapis.com/envoy.extensions.filters.http.dynamic_modules.v3.DynamicModuleFilter
                      dynamic_module_config:
                        name: cluster-router
                      filter_name: cluster-router-l2b-debug
                      filter_config:
                        "@type": type.googleapis.com/google.protobuf.StringValue
                        value: '{}'
                  - name: envoy.filters.http.router
                    typed_config:
                      "@type": type.googleapis.com/envoy.extensions.filters.http.router.v3.Router
                route_config:
                  name: l2-b
                  virtual_hosts:
                    - name: local
                      domains: ["*"]
                      routes:
                        - match: { prefix: "/" }
                          route: { cluster: l2-b }

  clusters:
    - name: l1
      connect_timeout: 5s
      lb_policy: CLUSTER_PROVIDED
      typed_extension_protocol_options:
        envoy.extensions.upstreams.http.v3.HttpProtocolOptions:
          "@type": type.googleapis.com/envoy.extensions.upstreams.http.v3.HttpProtocolOptions
          explicit_http_config:
            http_protocol_options: {}
          http_filters:
            - name: cluster-shard-router-upstream
              typed_config:
                "@type": type.googleapis.com/envoy.extensions.filters.http.dynamic_modules.v3.DynamicModuleFilter
                dynamic_module_config:
                  name: cluster-shard-router
                filter_name: cluster-shard-router-upstream
            - name: envoy.filters.http.upstream_codec
              typed_config:
                "@type": type.googleapis.com/envoy.extensions.filters.http.upstream_codec.v3.UpstreamCodec
      cluster_type:
        name: envoy.clusters.dynamic_modules
        typed_config:
          "@type": type.googleapis.com/envoy.extensions.clusters.dynamic_modules.v3.ClusterConfig
          dynamic_module_config:
            name: cluster-shard-router
          cluster_name: l1
          cluster_config:
            "@type": type.googleapis.com/google.protobuf.StringValue
            value: '{"initial":{"version":"local","default_shard":"a","shards":{"a":{"target":"127.0.0.1:{{.L2APort}}","prefixes":["a"],"shard":"a","status":"active"},"b":{"target":"127.0.0.1:{{.L2BPort}}","prefixes":["b"],"shard":"b","status":"active"}}}}'

    - name: l2-a
      connect_timeout: 5s
      lb_policy: CLUSTER_PROVIDED
      typed_extension_protocol_options:
        envoy.extensions.upstreams.http.v3.HttpProtocolOptions:
          "@type": type.googleapis.com/envoy.extensions.upstreams.http.v3.HttpProtocolOptions
          explicit_http_config:
            http_protocol_options: {}
          http_filters:
            - name: cluster-router-upstream
              typed_config:
                "@type": type.googleapis.com/envoy.extensions.filters.http.dynamic_modules.v3.DynamicModuleFilter
                dynamic_module_config:
                  name: cluster-router
                filter_name: cluster-router-l2a-upstream
            - name: envoy.filters.http.upstream_codec
              typed_config:
                "@type": type.googleapis.com/envoy.extensions.filters.http.upstream_codec.v3.UpstreamCodec
      cluster_type:
        name: envoy.clusters.dynamic_modules
        typed_config:
          "@type": type.googleapis.com/envoy.extensions.clusters.dynamic_modules.v3.ClusterConfig
          dynamic_module_config:
            name: cluster-router
          cluster_name: l2-a
          cluster_config:
            "@type": type.googleapis.com/google.protobuf.StringValue
            value: '{"initial":{"version":"local","models":{"gpt-fast":{"target":"127.0.0.1:{{.BackendAPort}}","provider":"openai","auth_header":"Bearer token-fast"},"gpt-slow":{"target":"127.0.0.1:{{.BackendAPort}}","provider":"openai","auth_header":"Bearer token-slow"}}}}'

    - name: l2-b
      connect_timeout: 5s
      lb_policy: CLUSTER_PROVIDED
      typed_extension_protocol_options:
        envoy.extensions.upstreams.http.v3.HttpProtocolOptions:
          "@type": type.googleapis.com/envoy.extensions.upstreams.http.v3.HttpProtocolOptions
          explicit_http_config:
            http_protocol_options: {}
          http_filters:
            - name: cluster-router-upstream
              typed_config:
                "@type": type.googleapis.com/envoy.extensions.filters.http.dynamic_modules.v3.DynamicModuleFilter
                dynamic_module_config:
                  name: cluster-router
                filter_name: cluster-router-l2b-upstream
            - name: envoy.filters.http.upstream_codec
              typed_config:
                "@type": type.googleapis.com/envoy.extensions.filters.http.upstream_codec.v3.UpstreamCodec
      cluster_type:
        name: envoy.clusters.dynamic_modules
        typed_config:
          "@type": type.googleapis.com/envoy.extensions.clusters.dynamic_modules.v3.ClusterConfig
          dynamic_module_config:
            name: cluster-router
          cluster_name: l2-b
          cluster_config:
            "@type": type.googleapis.com/google.protobuf.StringValue
            value: '{"initial":{"version":"local","models":{"claude-safe":{"target":"127.0.0.1:{{.BackendBPort}}","provider":"anthropic","auth_header":"Bearer token-claude-safe"},"claude-fast":{"target":"127.0.0.1:{{.BackendBPort}}","provider":"anthropic","auth_header":"Bearer token-claude-fast"}}}}'
`

func main() {
	tmpl, err := template.New("envoy").Parse(envoyTmpl)
	if err != nil {
		fmt.Fprintf(os.Stderr, "parse template: %v\n", err)
		os.Exit(1)
	}
	fmt.Fprintf(os.Stderr, "# tiered-single-envoy — default ports\n")
	fmt.Fprintf(os.Stderr, "#   L1 listener:  127.0.0.1:%d\n", defaultPorts["L1Port"])
	fmt.Fprintf(os.Stderr, "#   L2-a listener: 127.0.0.1:%d\n", defaultPorts["L2APort"])
	fmt.Fprintf(os.Stderr, "#   L2-b listener: 127.0.0.1:%d\n", defaultPorts["L2BPort"])
	fmt.Fprintf(os.Stderr, "#   Backend A:    127.0.0.1:%d\n", defaultPorts["BackendAPort"])
	fmt.Fprintf(os.Stderr, "#   Backend B:    127.0.0.1:%d\n", defaultPorts["BackendBPort"])
	fmt.Fprintf(os.Stderr, "#   Admin:        127.0.0.1:%d\n", defaultPorts["AdminPort"])
	if err := tmpl.Execute(os.Stdout, defaultPorts); err != nil {
		fmt.Fprintf(os.Stderr, "render template: %v\n", err)
		os.Exit(1)
	}
}
