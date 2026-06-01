package up

import (
	"context"
	"sync"
)

// ClusterGroup manages background goroutines scoped to a cluster extension's
// lifecycle. Goroutines are registered with [ClusterGroup.Go], launched at
// [ClusterGroup.Start] (called from [Cluster.ServerInitialized]), and stopped
// when [ClusterGroup.Stop] is called (called from [Cluster.Shutdown]).
//
// This mirrors the [Register] + [WithGroup] pattern from the HTTP filter side:
// goroutines are declared alongside setup logic and lifecycle plumbing is
// handled for you.
//
//	type myCluster struct {
//	    bg up.ClusterGroup
//	}
//
//	func (c *myCluster) ServerInitialized(_ up.ClusterHandle) {
//	    c.bg.Go(func(ctx context.Context) {
//	        up.RunRetry(ctx, "routes-watch", func(ctx context.Context) error {
//	            return c.watchRoutes(ctx)
//	        })
//	    })
//	    c.bg.Start()
//	}
//
//	func (c *myCluster) Shutdown(_ up.ClusterHandle, done func()) {
//	    c.bg.Stop()
//	    done()
//	}
type ClusterGroup struct {
	mu      sync.Mutex
	g       *Group
	pending []func(ctx context.Context)
}

// Go registers a context-aware background goroutine. Must be called before
// [ClusterGroup.Start]. Panics if called after Start.
func (cg *ClusterGroup) Go(fn func(ctx context.Context)) {
	cg.mu.Lock()
	defer cg.mu.Unlock()
	if cg.g != nil {
		panic("up: ClusterGroup.Go called after Start")
	}
	cg.pending = append(cg.pending, fn)
}

// Start launches all registered goroutines. Call exactly once from
// [Cluster.ServerInitialized]. No-op when no goroutines have been registered.
func (cg *ClusterGroup) Start() {
	cg.mu.Lock()
	defer cg.mu.Unlock()
	if cg.g != nil || len(cg.pending) == 0 {
		return
	}
	cg.g = NewGroup()
	for _, fn := range cg.pending {
		cg.g.AddGoroutine(fn)
	}
	cg.pending = nil
	cg.g.Start()
}

// Stop cancels all goroutines and waits for them to finish. Safe to call
// multiple times or before Start. Call from [Cluster.Shutdown] before invoking
// the done callback.
func (cg *ClusterGroup) Stop() {
	cg.mu.Lock()
	g := cg.g
	cg.mu.Unlock()
	if g != nil {
		g.Stop()
	}
}

// BaseCluster provides no-op implementations of all [Cluster] lifecycle methods
// except [Cluster.NewClusterLB], which must always be implemented by the
// embedder because [ClusterLB.ChooseHost] has no sensible default.
//
// Embed BaseCluster to only override the methods your cluster actually needs:
//
//	type myCluster struct {
//	    up.BaseCluster
//	    bg up.ClusterGroup
//	    // ...
//	}
//
//	func (c *myCluster) NewClusterLB() up.ClusterLB { return &myLB{} }
//
//	func (c *myCluster) Init(h up.ClusterHandle) {
//	    // add initial hosts, then:
//	    h.PreInitComplete()
//	}
//
//	func (c *myCluster) ServerInitialized(_ up.ClusterHandle) {
//	    c.bg.Go(func(ctx context.Context) { c.watch(ctx) })
//	    c.bg.Start()
//	}
//
//	func (c *myCluster) Shutdown(_ up.ClusterHandle, done func()) {
//	    c.bg.Stop()
//	    done()
//	}
//
// If neither Init nor ServerInitialized is overridden, BaseCluster.Init calls
// PreInitComplete (the cluster starts with no hosts) and BaseCluster.Shutdown
// calls done() immediately.
type BaseCluster struct{}

// Init calls PreInitComplete with no hosts. Override to add initial hosts
// before signalling Envoy that init is complete.
func (BaseCluster) Init(h ClusterHandle) { h.PreInitComplete() }

// ServerInitialized is a no-op. Override to start background goroutines via
// [ClusterGroup.Start] or to run synchronous cold-start work before Envoy
// workers begin accepting traffic.
func (BaseCluster) ServerInitialized(_ ClusterHandle) {}

// DrainStarted is a no-op.
func (BaseCluster) DrainStarted(_ ClusterHandle) {}

// Shutdown calls done immediately with no cleanup. Override to stop
// [ClusterGroup] goroutines (or other background work) before calling done.
func (BaseCluster) Shutdown(_ ClusterHandle, done func()) { done() }

// Close is a no-op.
func (BaseCluster) Close() {}
