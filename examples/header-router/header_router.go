package headerrouter

import (
	"os"

	"github.com/dio/transit/up"
)

var (
	hostA = "127.0.0.1:8080"
	hostB = "127.0.0.1:8081"
)

func init() {
	if v := os.Getenv("HEADER_ROUTER_HOST_A"); v != "" {
		hostA = v
	}
	if v := os.Getenv("HEADER_ROUTER_HOST_B"); v != "" {
		hostB = v
	}
	up.Register("header-router", Handler)
}

// ResolveHost returns the backend address for the given x-route-to header value.
// Returns ("", false) if the value does not match "a" or "b".
func ResolveHost(header, ha, hb string) (string, bool) {
	switch header {
	case "a":
		return ha, true
	case "b":
		return hb, true
	default:
		return "", false
	}
}

// Handler routes requests to backend A or B based on the x-route-to header.
// Requests without a matching header are forwarded using Envoy's default LB.
func Handler(w *up.Writer, r *up.Request) {
	if addr, ok := ResolveHost(r.Header("x-route-to"), hostA, hostB); ok {
		w.SetUpstreamOverrideHost(addr, false)
	}
}
