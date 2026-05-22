// Package clusterdfp demonstrates model-based upstream selection with the
// Cluster Extension. The module resolves configured model targets with Go DNS,
// adds the resolved hosts to Envoy, then picks a host per request.
package clusterdfp

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"sync"
	"time"

	"github.com/dio/transit/up"
)

const (
	modelHeader = "x-model"
)

func init() {
	up.RegisterCluster("go-dfp", &dfpFactory{})
}

type dfpConfig struct {
	TimeoutMillis int               `json:"timeout_millis,omitempty"`
	Models        map[string]string `json:"models,omitempty"`
}

type dfpFactory struct{}

func (f *dfpFactory) Create(config []byte) (up.ClusterConfigFactory, error) {
	cfg, err := parseConfig(config)
	if err != nil {
		return nil, err
	}
	return &dfpConfigFactory{
		timeout: cfg.timeout(),
		models:  cloneStringMap(cfg.Models),
	}, nil
}

func parseConfig(config []byte) (dfpConfig, error) {
	if len(config) == 0 {
		return dfpConfig{}, nil
	}
	var cfg dfpConfig
	if err := json.Unmarshal(config, &cfg); err != nil {
		return dfpConfig{}, fmt.Errorf("cluster-dfp: parse config: %w", err)
	}
	return cfg, nil
}

func (c dfpConfig) timeout() time.Duration {
	if c.TimeoutMillis <= 0 {
		return 250 * time.Millisecond
	}
	return time.Duration(c.TimeoutMillis) * time.Millisecond
}

type dfpConfigFactory struct {
	timeout time.Duration
	models  map[string]string
}

func (cf *dfpConfigFactory) NewCluster(h up.ClusterHandle) up.Cluster {
	return &dfpCluster{
		handle:       h,
		timeout:      cf.timeout,
		modelTargets: cloneStringMap(cf.models),
		modelAddrs:   make(map[string]string),
	}
}

func (cf *dfpConfigFactory) Close() {}

type dfpCluster struct {
	handle  up.ClusterHandle
	timeout time.Duration

	mu           sync.RWMutex
	modelTargets map[string]string
	modelAddrs   map[string]string
}

func (c *dfpCluster) Init(h up.ClusterHandle) {
	c.handle = h
	c.applyResolved(c.resolveModels(context.Background()))
	h.PreInitComplete()
}

func (c *dfpCluster) NewClusterLB() up.ClusterLB {
	return &dfpLB{cluster: c}
}

func (c *dfpCluster) ServerInitialized(_ up.ClusterHandle) {}
func (c *dfpCluster) DrainStarted(_ up.ClusterHandle)      {}
func (c *dfpCluster) Shutdown(_ up.ClusterHandle, done func()) {
	done()
}
func (c *dfpCluster) Close() {}

func (c *dfpCluster) addressForModel(model string) (string, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	addr, ok := c.modelAddrs[model]
	return addr, ok
}

func (c *dfpCluster) remember(model, addr string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.modelAddrs[model] = addr
}

func (c *dfpCluster) configuredModels() map[string]string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return cloneStringMap(c.modelTargets)
}

func (c *dfpCluster) resolveModels(ctx context.Context) map[string]string {
	resolved := make(map[string]string)
	for model, target := range c.configuredModels() {
		addr, err := resolveTarget(ctx, target, c.timeout)
		if err != nil {
			continue
		}
		resolved[model] = addr
	}
	return resolved
}

func (c *dfpCluster) applyResolved(resolved map[string]string) {
	for model, addr := range resolved {
		host := c.handle.FindHostByAddress(addr)
		if host == nil {
			ptrs := c.handle.AddHosts([]up.HostSpec{{Address: addr}})
			if len(ptrs) == 0 {
				continue
			}
			host = ptrs[0]
			c.handle.UpdateHostHealth(host, up.HostHealthy)
		}
		c.remember(model, addr)
	}
}

type dfpLB struct {
	up.EmptyClusterLB
	cluster *dfpCluster
}

func (lb *dfpLB) ChooseHost(h up.ClusterLBHandle, ctx up.ClusterLBContext) (up.HostPtr, *up.ClusterLBCompletion) {
	model, ok := ctx.GetHeader(modelHeader)
	if !ok || model == "" {
		return nil, nil
	}

	addr, ok := lb.cluster.addressForModel(model)
	if !ok {
		return nil, nil
	}
	return h.FindHostByAddress(addr), nil
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

func cloneStringMap(in map[string]string) map[string]string {
	if len(in) == 0 {
		return nil
	}
	out := make(map[string]string, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}
