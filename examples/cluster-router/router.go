// Package clusterrouter demonstrates request-aware host selection with a
// Cluster Extension plus upstream request shaping from Go.
package clusterrouter

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"github.com/dio/transit/up"
)

const (
	modelHeader  = "x-model"
	tenantHeader = "x-tenant"
	debugPath    = "/__cluster-router/config"

	defaultRefresh = time.Second
	defaultTimeout = 500 * time.Millisecond
)

// One shared route table keeps the example honest. Host selection and upstream
// header injection are separate Envoy callbacks, but in a router they have to
// agree on the same active config:
//
//   - the Cluster Extension reads it while choosing a host
//   - the upstream HTTP filter reads it while injecting provider headers
//   - the debug HTTP filter reads it while dumping active config
var activeRoutes = newRouteStore()

func init() {
	up.RegisterCluster("cluster-router", &clusterFactory{})
	up.Register("cluster-router-upstream", upstreamHeaderFilter)
	up.Register("cluster-router-debug", debugHandler)
}

type routerConfig struct {
	ConfigURL     string         `json:"config_url,omitempty"`
	RefreshMillis int            `json:"refresh_millis,omitempty"`
	TimeoutMillis int            `json:"timeout_millis,omitempty"`
	Initial       configSnapshot `json:"initial,omitempty"`
}

func (c routerConfig) refresh() time.Duration {
	if c.RefreshMillis <= 0 {
		return defaultRefresh
	}
	return time.Duration(c.RefreshMillis) * time.Millisecond
}

func (c routerConfig) timeout() time.Duration {
	if c.TimeoutMillis <= 0 {
		return defaultTimeout
	}
	return time.Duration(c.TimeoutMillis) * time.Millisecond
}

type configSnapshot struct {
	Version string                       `json:"version"`
	Models  map[string]modelConfig       `json:"models,omitempty"`
	Auth    map[string]authConfig        `json:"auth,omitempty"`
	BYOK    map[string]map[string]string `json:"byok,omitempty"`
}

type modelConfig struct {
	Target     string `json:"target"`
	Provider   string `json:"provider"`
	AuthHeader string `json:"auth_header,omitempty"`
	AuthRef    string `json:"auth_ref,omitempty"`
}

type authConfig struct {
	Type         string `json:"type"`
	Header       string `json:"header,omitempty"`
	TenantHeader string `json:"tenant_header,omitempty"`
}

type routeSnapshot struct {
	Version string
	Models  map[string]modelRoute
	Auth    map[string]authPolicy
	BYOK    map[string]map[string]string
}

type modelRoute struct {
	Target     string
	Address    string
	Provider   string
	AuthHeader string
	AuthRef    string
}

type authPolicy struct {
	Type         string
	Header       string
	TenantHeader string
}

type routeStore struct {
	// Publish whole snapshots, not individual fields. A request should either
	// see the old config or the new config, never a half-updated mix.
	current atomic.Value // routeSnapshot

	// Host pointers are Envoy-owned handles. Keep them as bookkeeping only; the
	// route snapshot above is the source of truth for request-time decisions.
	hostsMu sync.Mutex
	hosts   map[string]up.HostPtr
}

func newRouteStore() *routeStore {
	s := &routeStore{hosts: make(map[string]up.HostPtr)}
	s.current.Store(routeSnapshot{
		Models: make(map[string]modelRoute),
		Auth:   make(map[string]authPolicy),
		BYOK:   make(map[string]map[string]string),
	})
	return s
}

func (s *routeStore) Current() routeSnapshot {
	if v := s.current.Load(); v != nil {
		if snap, ok := v.(routeSnapshot); ok {
			return snap
		}
	}
	return routeSnapshot{Models: make(map[string]modelRoute)}
}

func (s *routeStore) LookupModel(model string) (modelRoute, bool) {
	route, ok := s.Current().Models[model]
	return route, ok
}

func (s *routeStore) Publish(snapshot routeSnapshot) {
	// Clone before publishing. Maps are mutable in Go, and request callbacks
	// will read these maps without locks after the atomic swap.
	s.current.Store(cloneRouteSnapshot(snapshot))
}

func (s *routeStore) RememberHost(addr string, host up.HostPtr) {
	s.hostsMu.Lock()
	defer s.hostsMu.Unlock()
	s.hosts[addr] = host
}

func (s *routeStore) Host(addr string) (up.HostPtr, bool) {
	s.hostsMu.Lock()
	defer s.hostsMu.Unlock()
	host, ok := s.hosts[addr]
	return host, ok
}

func parseRouterConfig(config []byte) (routerConfig, error) {
	if len(config) == 0 {
		return routerConfig{}, nil
	}
	var cfg routerConfig
	if err := json.Unmarshal(config, &cfg); err != nil {
		return routerConfig{}, fmt.Errorf("cluster-router: parse config: %w", err)
	}
	return cfg, nil
}

func resolveConfigSnapshot(parent context.Context, cfg configSnapshot, timeout time.Duration) (routeSnapshot, error) {
	// Resolve everything before the caller publishes. If one target is bad, keep
	// serving the previous config instead of publishing a broken partial one.
	out := routeSnapshot{
		Version: cfg.Version,
		Models:  make(map[string]modelRoute, len(cfg.Models)),
		Auth:    make(map[string]authPolicy, len(cfg.Auth)),
		BYOK:    cloneBYOK(cfg.BYOK),
	}
	for name, auth := range cfg.Auth {
		out.Auth[name] = authPolicy(auth)
	}
	for name, model := range cfg.Models {
		if model.Target == "" {
			return routeSnapshot{}, fmt.Errorf("cluster-router: model %q target is required", name)
		}
		addr, err := resolveTarget(parent, model.Target, timeout)
		if err != nil {
			return routeSnapshot{}, err
		}
		out.Models[name] = modelRoute{
			Target:     model.Target,
			Address:    addr,
			Provider:   model.Provider,
			AuthHeader: model.AuthHeader,
			AuthRef:    model.AuthRef,
		}
	}
	return out, nil
}

func resolveTarget(parent context.Context, target string, timeout time.Duration) (string, error) {
	host, port, err := net.SplitHostPort(target)
	if err != nil {
		return "", fmt.Errorf("invalid target %q: %w", target, err)
	}

	ctx, cancel := context.WithTimeout(parent, timeout)
	defer cancel()

	addrs, err := net.DefaultResolver.LookupIPAddr(ctx, host)
	if err != nil {
		return "", fmt.Errorf("resolve %q: %w", host, err)
	}
	for _, addr := range addrs {
		// Prefer IPv4 so local examples read as 127.0.0.1:port instead of
		// mixing IPv4 and IPv6 loopback addresses.
		if ip4 := addr.IP.To4(); ip4 != nil {
			return net.JoinHostPort(ip4.String(), port), nil
		}
	}
	if len(addrs) > 0 {
		return net.JoinHostPort(addrs[0].IP.String(), port), nil
	}
	return "", fmt.Errorf("resolve %q: no addresses", host)
}

func cloneRouteSnapshot(in routeSnapshot) routeSnapshot {
	out := routeSnapshot{
		Version: in.Version,
		Models:  make(map[string]modelRoute, len(in.Models)),
		Auth:    make(map[string]authPolicy, len(in.Auth)),
		BYOK:    cloneBYOK(in.BYOK),
	}
	for k, v := range in.Models {
		out.Models[k] = v
	}
	for k, v := range in.Auth {
		out.Auth[k] = v
	}
	return out
}

func cloneBYOK(in map[string]map[string]string) map[string]map[string]string {
	if len(in) == 0 {
		return nil
	}
	out := make(map[string]map[string]string, len(in))
	for tenant, providers := range in {
		out[tenant] = make(map[string]string, len(providers))
		for provider, header := range providers {
			out[tenant][provider] = header
		}
	}
	return out
}
