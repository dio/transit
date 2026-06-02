package main

import (
	"os"

	_ "github.com/dio/transit/down/abi_impl"
	wsproxy "github.com/dio/transit/examples/ws-proxy"
)

func init() {
	wsproxy.Register()

	// Direct-dial mode instance, used by e2e to test break-glass path.
	directListenAddr := os.Getenv("WSPROXY_DIRECT_LISTEN_ADDR")
	directUpstreamURL := os.Getenv("WSPROXY_DIRECT_UPSTREAM_URL")
	if directListenAddr != "" && directUpstreamURL != "" {
		cfg := wsproxy.Config{
			ListenAddress: directListenAddr,
			UpstreamURL:   directUpstreamURL,
		}
		wsproxy.RegisterDirect("ws-proxy-direct", cfg)
	}
}

func main() {}
