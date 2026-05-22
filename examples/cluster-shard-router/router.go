// Package clustershardrouter demonstrates request-aware shard selection with a
// Cluster Extension plus upstream request shaping from Go.
package clustershardrouter

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/dio/transit/up"
)

const (
	tagHeader        = "x-transit-tag"
	tagSourceHeader  = "x-transit-tag-source"
	byokKeyIDHeader  = "x-byok-key-id"
	userKeyHeader    = "x-user-key"
	tenantHeader     = "x-tenant"
	shardHeader      = "x-transit-l1-shard"
	targetHeader     = "x-transit-l1-target"
	versionHeader    = "x-cluster-shard-router-version"
	debugPath        = "/__cluster-shard-router/config"
	defaultShardName = "default"

	defaultRefresh = time.Second
	defaultTimeout = 500 * time.Millisecond
)

var activeShards = newShardStore()

func init() {
	up.RegisterCluster("cluster-shard-router", &clusterFactory{})
	up.Register("cluster-shard-router-upstream", upstreamHeaderFilter)
	up.Register("cluster-shard-router-debug", debugHandler)
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
	Version      string                 `json:"version"`
	DefaultShard string                 `json:"default_shard,omitempty"`
	Shards       map[string]shardConfig `json:"shards,omitempty"`
}

type shardConfig struct {
	Target   string   `json:"target"`
	Prefixes []string `json:"prefixes,omitempty"`
	Shard    string   `json:"shard,omitempty"`
	Status   string   `json:"status,omitempty"`
}

type shardSnapshot struct {
	Version      string
	DefaultShard string
	Shards       map[string]shardRoute
}

type shardRoute struct {
	Name     string
	Shard    string
	Target   string
	Address  string
	Prefixes []string
	Status   string
}

type shardDecision struct {
	Tag    string
	Source string
	Route  shardRoute
}

type shardStore struct {
	current atomic.Value // shardSnapshot

	hostsMu sync.Mutex
	hosts   map[string]up.HostPtr
}

func newShardStore() *shardStore {
	s := &shardStore{hosts: make(map[string]up.HostPtr)}
	s.current.Store(shardSnapshot{Shards: make(map[string]shardRoute)})
	return s
}

func (s *shardStore) Current() shardSnapshot {
	if v := s.current.Load(); v != nil {
		if snap, ok := v.(shardSnapshot); ok {
			return snap
		}
	}
	return shardSnapshot{Shards: make(map[string]shardRoute)}
}

func (s *shardStore) Publish(snapshot shardSnapshot) {
	s.current.Store(cloneShardSnapshot(snapshot))
}

func (s *shardStore) RememberHost(addr string, host up.HostPtr) {
	s.hostsMu.Lock()
	defer s.hostsMu.Unlock()
	s.hosts[addr] = host
}

func (s *shardStore) Host(addr string) (up.HostPtr, bool) {
	s.hostsMu.Lock()
	defer s.hostsMu.Unlock()
	host, ok := s.hosts[addr]
	return host, ok
}

func (s *shardStore) Decide(getHeader func(string) string) (shardDecision, bool) {
	snap := s.Current()
	tag, source := deriveTag(getHeader)
	if tag == "" {
		tag = snap.DefaultShard
		source = "default"
	}
	route, ok := chooseShard(snap, tag)
	if !ok {
		return shardDecision{}, false
	}
	return shardDecision{Tag: tag, Source: source, Route: route}, true
}

func parseRouterConfig(config []byte) (routerConfig, error) {
	if len(config) == 0 {
		return routerConfig{}, nil
	}
	var cfg routerConfig
	if err := json.Unmarshal(config, &cfg); err != nil {
		return routerConfig{}, fmt.Errorf("cluster-shard-router: parse config: %w", err)
	}
	return cfg, nil
}

func resolveConfigSnapshot(parent context.Context, cfg configSnapshot, timeout time.Duration) (shardSnapshot, error) {
	out := shardSnapshot{
		Version:      cfg.Version,
		DefaultShard: normalizeToken(cfg.DefaultShard),
		Shards:       make(map[string]shardRoute, len(cfg.Shards)),
	}
	if out.DefaultShard == "" {
		out.DefaultShard = defaultShardName
	}
	for name, shard := range cfg.Shards {
		routeName := normalizeToken(name)
		if routeName == "" {
			return shardSnapshot{}, fmt.Errorf("cluster-shard-router: shard name is required")
		}
		if shard.Target == "" {
			return shardSnapshot{}, fmt.Errorf("cluster-shard-router: shard %q target is required", name)
		}
		addr, err := resolveTarget(parent, shard.Target, timeout)
		if err != nil {
			return shardSnapshot{}, err
		}
		route := shardRoute{
			Name:     routeName,
			Shard:    normalizeToken(shard.Shard),
			Target:   shard.Target,
			Address:  addr,
			Prefixes: normalizePrefixes(shard.Prefixes),
			Status:   normalizeToken(shard.Status),
		}
		if route.Shard == "" {
			route.Shard = routeName
		}
		if len(route.Prefixes) == 0 && routeName != defaultShardName {
			route.Prefixes = []string{routeName}
		}
		out.Shards[routeName] = route
	}
	if _, ok := out.Shards[out.DefaultShard]; !ok && len(out.Shards) > 0 {
		return shardSnapshot{}, fmt.Errorf("cluster-shard-router: default shard %q is not configured", out.DefaultShard)
	}
	return out, nil
}

func chooseShard(snap shardSnapshot, tag string) (shardRoute, bool) {
	tag = normalizeToken(tag)
	var (
		best    shardRoute
		bestLen int
		found   bool
	)
	for _, route := range snap.Shards {
		if route.Status != "" && route.Status != "active" {
			continue
		}
		for _, prefix := range route.Prefixes {
			if strings.HasPrefix(tag, prefix) && len(prefix) > bestLen {
				best = route
				bestLen = len(prefix)
				found = true
			}
		}
	}
	if found {
		return best, true
	}
	route, ok := snap.Shards[snap.DefaultShard]
	return route, ok
}

func deriveTag(getHeader func(string) string) (string, string) {
	if tag := normalizeToken(getHeader(tagHeader)); tag != "" {
		return tag, "tag"
	}
	if keyID := strings.TrimSpace(getHeader(byokKeyIDHeader)); keyID != "" {
		return hashTag(keyID), "byok-key-id"
	}
	if userKey := strings.TrimSpace(getHeader(userKeyHeader)); userKey != "" {
		return hashTag(userKey), "user-key"
	}
	if tenant := normalizeToken(getHeader(tenantHeader)); tenant != "" {
		return tenant, "tenant"
	}
	return "", ""
}

func hashTag(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])
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
		if ip4 := addr.IP.To4(); ip4 != nil {
			return net.JoinHostPort(ip4.String(), port), nil
		}
	}
	if len(addrs) > 0 {
		return net.JoinHostPort(addrs[0].IP.String(), port), nil
	}
	return "", fmt.Errorf("resolve %q: no addresses", host)
}

func cloneShardSnapshot(in shardSnapshot) shardSnapshot {
	out := shardSnapshot{
		Version:      in.Version,
		DefaultShard: in.DefaultShard,
		Shards:       make(map[string]shardRoute, len(in.Shards)),
	}
	for k, v := range in.Shards {
		v.Prefixes = append([]string(nil), v.Prefixes...)
		out.Shards[k] = v
	}
	return out
}

func normalizePrefixes(in []string) []string {
	out := make([]string, 0, len(in))
	seen := make(map[string]struct{}, len(in))
	for _, prefix := range in {
		prefix = normalizeToken(prefix)
		if prefix == "" {
			continue
		}
		if _, ok := seen[prefix]; ok {
			continue
		}
		seen[prefix] = struct{}{}
		out = append(out, prefix)
	}
	return out
}

func normalizeToken(v string) string {
	return strings.ToLower(strings.TrimSpace(v))
}
