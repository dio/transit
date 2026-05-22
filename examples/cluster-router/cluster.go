package clusterrouter

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
		store:   routeStoreForScope(cf.cfg.scope()),
		timeout: cf.cfg.timeout(),
		refresh: cf.cfg.refresh(),
	}
}

func (cf *clusterConfigFactory) Close() {}

type routerCluster struct {
	handle  up.ClusterHandle
	group   *up.Group
	cfg     routerConfig
	store   *routeStore
	timeout time.Duration
	refresh time.Duration
	startMu sync.Mutex
}

func (c *routerCluster) Init(h up.ClusterHandle) {
	c.handle = h
	if len(c.cfg.Initial.Models) > 0 {
		// Bootstrap hosts synchronously so Envoy can mark the cluster ready with
		// a usable route table. Later refreshes use the background group below.
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
	// Config fetching is module-scoped work, not request-scoped work. up.Group
	// gives it a clear lifetime tied to the cluster instance.
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
	// Host mutation belongs on Envoy's cluster main thread. The goroutine can
	// fetch and resolve, but AddHosts and snapshot publication happen here.
	c.handle.Schedule(func() {
		c.applySnapshot(snap)
	})
}

func (c *routerCluster) applySnapshot(snap routeSnapshot) {
	// Add every host first, then publish the snapshot. That ordering prevents
	// ChooseHost from seeing a route whose address is not yet known to Envoy.
	for _, route := range snap.Models {
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
	store *routeStore
}

func (lb *routerLB) ChooseHost(h up.ClusterLBHandle, ctx up.ClusterLBContext) (up.HostPtr, *up.ClusterLBCompletion) {
	// This is the whole point of the example: no route rewrite, no cluster per
	// model. The request says the model, and Go returns the matching host.
	model, ok := ctx.GetHeader(modelHeader)
	if !ok || model == "" {
		return nil, nil
	}
	route, ok := lb.store.LookupModel(model)
	if !ok {
		return nil, nil
	}
	return h.FindHostByAddress(route.Address), nil
}
