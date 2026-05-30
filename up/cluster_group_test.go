package up

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestClusterGroup_GoAndStart(t *testing.T) {
	var cg ClusterGroup
	started := make(chan struct{})
	cg.Go(func(ctx context.Context) {
		close(started)
		<-ctx.Done()
	})
	cg.Start()
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("goroutine did not start after Start()")
	}
	cg.Stop()
}

func TestClusterGroup_StopCancelsGoroutines(t *testing.T) {
	var cg ClusterGroup
	stopped := make(chan struct{})
	cg.Go(func(ctx context.Context) {
		<-ctx.Done()
		close(stopped)
	})
	cg.Start()
	cg.Stop()
	select {
	case <-stopped:
	case <-time.After(2 * time.Second):
		t.Fatal("goroutine did not stop after Stop()")
	}
}

func TestClusterGroup_StartNoOp_WhenEmpty(t *testing.T) {
	var cg ClusterGroup
	// Must not panic or block.
	cg.Start()
	cg.Stop()
}

func TestClusterGroup_StopBeforeStart_IsNoOp(t *testing.T) {
	var cg ClusterGroup
	// Must not panic.
	cg.Stop()
}

func TestClusterGroup_StartIdempotent(t *testing.T) {
	var cg ClusterGroup
	var count atomic.Int32
	cg.Go(func(ctx context.Context) {
		count.Add(1)
		<-ctx.Done()
	})
	cg.Start()
	cg.Start() // second call must be a no-op
	time.Sleep(50 * time.Millisecond)
	require.Equal(t, int32(1), count.Load(), "goroutine should start exactly once")
	cg.Stop()
}

func TestClusterGroup_MultipleGoroutines(t *testing.T) {
	var cg ClusterGroup
	const n = 5
	var started atomic.Int32
	for range n {
		cg.Go(func(ctx context.Context) {
			started.Add(1)
			<-ctx.Done()
		})
	}
	cg.Start()
	require.Eventually(t, func() bool {
		return started.Load() == n
	}, 2*time.Second, 10*time.Millisecond)
	cg.Stop()
}

func TestClusterGroup_GoAfterStart_Panics(t *testing.T) {
	var cg ClusterGroup
	cg.Go(func(_ context.Context) {})
	cg.Start()
	defer cg.Stop()
	require.Panics(t, func() {
		cg.Go(func(_ context.Context) {})
	})
}

// ---------------------------------------------------------------------------
// BaseCluster
// ---------------------------------------------------------------------------

// minimalCluster embeds BaseCluster and provides the required NewClusterLB.
type minimalCluster struct {
	BaseCluster
}

func (c *minimalCluster) NewClusterLB() ClusterLB { return &fakeLB{} }

// fakeLB satisfies ClusterLB with no-op implementations.
type fakeLB struct{ EmptyClusterLB }

func (f *fakeLB) ChooseHost(_ ClusterLBHandle, _ ClusterLBContext) (HostPtr, *ClusterLBCompletion) {
	return nil, nil
}

func TestBaseCluster_InitCallsPreInitComplete(t *testing.T) {
	var c minimalCluster
	var called bool
	fakeHandle := &fakeClusterHandle{preInitCompleteFn: func() { called = true }}
	c.Init(fakeHandle)
	require.True(t, called)
}

func TestBaseCluster_ShutdownCallsDone(t *testing.T) {
	var c minimalCluster
	var called bool
	c.Shutdown(nil, func() { called = true })
	require.True(t, called)
}

func TestBaseCluster_LifecycleNoOps(t *testing.T) {
	var c minimalCluster
	// None of these should panic.
	c.ServerInitialized(nil)
	c.DrainStarted(nil)
	c.Close()
}

// fakeClusterHandle is a minimal ClusterHandle for testing.
type fakeClusterHandle struct {
	preInitCompleteFn func()
}

func (f *fakeClusterHandle) AddHosts(_ []HostSpec) []HostPtr          { return nil }
func (f *fakeClusterHandle) RemoveHosts(_ []HostPtr)                  {}
func (f *fakeClusterHandle) UpdateHostHealth(_ HostPtr, _ HostHealth) {}
func (f *fakeClusterHandle) FindHostByAddress(_ string) HostPtr       { return nil }
func (f *fakeClusterHandle) PreInitComplete()                         { f.preInitCompleteFn() }
func (f *fakeClusterHandle) Schedule(fn func())                       { fn() }
