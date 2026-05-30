// Package clustergroup demonstrates how to use [up.BaseCluster], [up.ClusterGroup],
// and [up.RunRetry] together to build a cluster extension that discovers upstream
// hosts from an external source at runtime.
//
// The cluster polls a discovery HTTP endpoint that returns a JSON list of host
// addresses. On startup it fetches the list synchronously in ServerInitialized
// (before Envoy workers accept traffic), then refreshes it in the background
// via a ClusterGroup goroutine so that updates are picked up without restarts.
//
// Compare with the static [cluster] example: all lifecycle boilerplate
// (Init, DrainStarted, Close, Shutdown) is eliminated by embedding BaseCluster.
package clustergroup

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/dio/transit/up"
)

func init() {
	up.RegisterCluster("discovery-cluster", &discoveryFactory{})
}

// ---------------------------------------------------------------------------
// Config
// ---------------------------------------------------------------------------

type clusterConfig struct {
	DiscoveryURL string `json:"discovery_url"`
	RefreshMS    int    `json:"refresh_ms"` // poll interval in milliseconds; default 5000
}

func (c clusterConfig) refreshInterval() time.Duration {
	if c.RefreshMS <= 0 {
		return 5 * time.Second
	}
	return time.Duration(c.RefreshMS) * time.Millisecond
}

// ---------------------------------------------------------------------------
// Factory chain
// ---------------------------------------------------------------------------

type discoveryFactory struct{}

func (f *discoveryFactory) Create(config []byte) (up.ClusterConfigFactory, error) {
	var cfg clusterConfig
	if err := json.Unmarshal(config, &cfg); err != nil {
		return nil, fmt.Errorf("cluster-group: parse config: %w", err)
	}
	if cfg.DiscoveryURL == "" {
		return nil, fmt.Errorf("cluster-group: discovery_url is required")
	}
	return &discoveryConfigFactory{cfg: cfg}, nil
}

type discoveryConfigFactory struct{ cfg clusterConfig }

func (cf *discoveryConfigFactory) NewCluster(h up.ClusterHandle) up.Cluster {
	return &discoveryCluster{
		handle: h,
		cfg:    cf.cfg,
		known:  make(map[string]up.HostPtr),
	}
}

func (cf *discoveryConfigFactory) Close() {}

// ---------------------------------------------------------------------------
// Cluster — embeds BaseCluster for lifecycle boilerplate
// ---------------------------------------------------------------------------

type discoveryCluster struct {
	up.BaseCluster // Init (→ PreInitComplete), DrainStarted, Close, Shutdown no-ops

	bg     up.ClusterGroup
	handle up.ClusterHandle
	cfg    clusterConfig

	mu    sync.Mutex
	known map[string]up.HostPtr // addr → Envoy host pointer (main-thread only)
}

// Init overrides BaseCluster.Init to avoid the default PreInitComplete call
// so we can add any initial hosts if needed in the future. Right now we start
// with an empty host set and rely on ServerInitialized for the first fetch.
func (c *discoveryCluster) Init(h up.ClusterHandle) {
	h.PreInitComplete()
}

func (c *discoveryCluster) NewClusterLB() up.ClusterLB {
	return &roundRobinLB{}
}

// ServerInitialized performs a synchronous cold-start fetch before Envoy workers
// begin accepting traffic, then starts a background ClusterGroup goroutine that
// refreshes the host list at the configured interval. RunRetry ensures that a
// transient fetch error does not permanently stop discovery.
func (c *discoveryCluster) ServerInitialized(_ up.ClusterHandle) {
	// Cold-start: we are on the Envoy main thread, so AddHosts/UpdateHostHealth
	// are safe to call directly without Schedule.
	if hosts, err := fetchDiscovery(c.cfg.DiscoveryURL); err != nil {
		log.Printf("cluster-group: cold-start fetch failed: %v", err)
	} else {
		c.applyHostsDirect(hosts)
	}

	c.bg.Go(func(ctx context.Context) {
		up.RunRetry(ctx, "discovery-poll", func(ctx context.Context) error {
			ticker := time.NewTicker(c.cfg.refreshInterval())
			defer ticker.Stop()
			for {
				select {
				case <-ctx.Done():
					return ctx.Err()
				case <-ticker.C:
					hosts, err := fetchDiscovery(c.cfg.DiscoveryURL)
					if err != nil {
						return err // RunRetry logs and retries
					}
					c.scheduleApplyHosts(hosts)
				}
			}
		})
	})
	c.bg.Start()
}

// Shutdown stops background goroutines before the cluster is torn down.
// Overrides BaseCluster.Shutdown so we can drain the ClusterGroup first.
func (c *discoveryCluster) Shutdown(_ up.ClusterHandle, done func()) {
	c.bg.Stop()
	done()
}

// ---------------------------------------------------------------------------
// Host management
// ---------------------------------------------------------------------------

// applyHostsDirect adds or removes hosts on the Envoy main thread directly.
// Only call from Init or ServerInitialized (already on main thread).
func (c *discoveryCluster) applyHostsDirect(addrs []string) {
	want := make(map[string]struct{}, len(addrs))
	for _, a := range addrs {
		want[a] = struct{}{}
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	for addr := range want {
		if _, ok := c.known[addr]; !ok {
			ptrs := c.handle.AddHosts([]up.HostSpec{{Address: addr}})
			c.handle.UpdateHostHealth(ptrs[0], up.HostHealthy)
			c.known[addr] = ptrs[0]
		}
	}
	for addr, ptr := range c.known {
		if _, ok := want[addr]; !ok {
			c.handle.RemoveHosts([]up.HostPtr{ptr})
			delete(c.known, addr)
		}
	}
}

// scheduleApplyHosts dispatches a host update onto the Envoy main thread via
// ClusterHandle.Schedule. Use this from background goroutines.
func (c *discoveryCluster) scheduleApplyHosts(addrs []string) {
	c.handle.Schedule(func() { c.applyHostsDirect(addrs) })
}

// ---------------------------------------------------------------------------
// Discovery HTTP client
// ---------------------------------------------------------------------------

func fetchDiscovery(url string) ([]string, error) {
	resp, err := http.Get(url) //nolint:noctx
	if err != nil {
		return nil, fmt.Errorf("GET %s: %w", url, err)
	}
	defer resp.Body.Close() //nolint:errcheck
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", url, err)
	}
	var result struct {
		Hosts []string `json:"hosts"`
	}
	if err := json.Unmarshal(body, &result); err != nil {
		return nil, fmt.Errorf("parse %s: %w", url, err)
	}
	return result.Hosts, nil
}

// ---------------------------------------------------------------------------
// ClusterLB — round-robin over healthy hosts
// ---------------------------------------------------------------------------

type roundRobinLB struct {
	up.EmptyClusterLB
	counter atomic.Uint64
}

func (lb *roundRobinLB) ChooseHost(h up.ClusterLBHandle, _ up.ClusterLBContext) (up.HostPtr, *up.ClusterLBCompletion) {
	n := h.HealthyHostCount(0)
	if n == 0 {
		return nil, nil
	}
	idx := int(lb.counter.Add(1)-1) % n
	return h.HealthyHost(0, idx), nil
}
