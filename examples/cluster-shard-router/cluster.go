package clustershardrouter

import (
	"context"
	"encoding/json"
	"net/http"
	"sync"
	"time"

	"github.com/dio/transit/up"
)

type clusterFactory struct{}

func (f *clusterFactory) Create(config []byte) (up.ClusterConfigFactory, error) {
	cfg, err := parseRouterConfig(config)
	if err != nil {
		return nil, err
	}
	return &clusterConfigFactory{cfg: cfg}, nil
}

type clusterConfigFactory struct {
	cfg routerConfig
}

func (cf *clusterConfigFactory) NewCluster(h up.ClusterHandle) up.Cluster {
	return &routerCluster{
		handle:  h,
		cfg:     cf.cfg,
		store:   activeShards,
		timeout: cf.cfg.timeout(),
		refresh: cf.cfg.refresh(),
	}
}

func (cf *clusterConfigFactory) Close() {}

type routerCluster struct {
	handle  up.ClusterHandle
	group   *up.Group
	cfg     routerConfig
	store   *shardStore
	timeout time.Duration
	refresh time.Duration
	startMu sync.Mutex
}

func (c *routerCluster) Init(h up.ClusterHandle) {
	c.handle = h
	if len(c.cfg.Initial.Shards) > 0 {
		if snap, err := resolveConfigSnapshot(context.Background(), c.cfg.Initial, c.timeout); err == nil {
			c.applySnapshot(snap)
		}
	}
	h.PreInitComplete()
	c.startFetchLoop()
}

func (c *routerCluster) ServerInitialized(_ up.ClusterHandle) {
	c.startFetchLoop()
}

func (c *routerCluster) startFetchLoop() {
	if c.cfg.ConfigURL == "" {
		return
	}
	c.startMu.Lock()
	defer c.startMu.Unlock()
	if c.group != nil {
		return
	}
	c.group = up.NewGroup()
	c.group.AddGoroutine(c.fetchLoop)
	c.group.Start()
}

func (c *routerCluster) NewClusterLB() up.ClusterLB {
	return &routerLB{store: c.store}
}

func (c *routerCluster) DrainStarted(_ up.ClusterHandle) {}

func (c *routerCluster) Shutdown(_ up.ClusterHandle, done func()) {
	c.stop()
	done()
}

func (c *routerCluster) Close() {
	c.stop()
}

func (c *routerCluster) stop() {
	if c.group != nil {
		c.group.Stop()
		c.group = nil
	}
}

func (c *routerCluster) fetchLoop(ctx context.Context) {
	ticker := time.NewTicker(c.refresh)
	defer ticker.Stop()
	c.fetchOnce(ctx)
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			c.fetchOnce(ctx)
		}
	}
}

func (c *routerCluster) fetchOnce(ctx context.Context) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.cfg.ConfigURL, nil)
	if err != nil {
		return
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return
	}
	var cfg configSnapshot
	if err := json.NewDecoder(resp.Body).Decode(&cfg); err != nil {
		return
	}
	snap, err := resolveConfigSnapshot(ctx, cfg, c.timeout)
	if err != nil {
		return
	}
	c.handle.Schedule(func() {
		c.applySnapshot(snap)
	})
}

func (c *routerCluster) applySnapshot(snap shardSnapshot) {
	for _, route := range snap.Shards {
		host := c.handle.FindHostByAddress(route.Address)
		if host == nil {
			ptrs := c.handle.AddHosts([]up.HostSpec{{Address: route.Address}})
			if len(ptrs) == 0 {
				return
			}
			host = ptrs[0]
			c.handle.UpdateHostHealth(host, up.HostHealthy)
		}
		c.store.RememberHost(route.Address, host)
	}
	c.store.Publish(snap)
}

type routerLB struct {
	up.EmptyClusterLB
	store *shardStore
}

func (lb *routerLB) ChooseHost(h up.ClusterLBHandle, ctx up.ClusterLBContext) (up.HostPtr, *up.ClusterLBCompletion) {
	decision, ok := lb.store.Decide(func(name string) string {
		value, _ := ctx.GetHeader(name)
		return value
	})
	if !ok {
		return nil, nil
	}
	return h.FindHostByAddress(decision.Route.Address), nil
}
