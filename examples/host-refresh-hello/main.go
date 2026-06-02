// Package main demonstrates how to wire HostSet and HostRefreshLoop together.
// It uses an inline fakeClusterHandle so no Envoy binary is needed.
package main

import (
	"context"
	"fmt"
	"sync"
	"time"
	"unsafe"

	"github.com/dio/transit/up"
)

// fakeClusterHandle is a local, print-only ClusterHandle.
// It lets us observe the threading contract without a running Envoy.
type fakeClusterHandle struct {
	mu      sync.Mutex
	nextID  uint64
	backing []*uint64 // keeps allocations alive so HostPtrs remain valid
}

func (h *fakeClusterHandle) AddHosts(specs []up.HostSpec) []up.HostPtr {
	h.mu.Lock()
	defer h.mu.Unlock()
	ptrs := make([]up.HostPtr, len(specs))
	for i, s := range specs {
		h.nextID++
		v := new(uint64)
		*v = h.nextID
		h.backing = append(h.backing, v)
		ptrs[i] = up.HostPtr(unsafe.Pointer(v))
		fmt.Printf("[cluster] AddHosts: %s (id=%d)\n", s.Address, h.nextID)
	}
	return ptrs
}

func (h *fakeClusterHandle) RemoveHosts(hosts []up.HostPtr) {
	fmt.Printf("[cluster] RemoveHosts: %d host(s)\n", len(hosts))
}

func (h *fakeClusterHandle) UpdateHostHealth(_ up.HostPtr, health up.HostHealth) {
	fmt.Printf("[cluster] UpdateHostHealth: health=%d\n", health)
}

func (h *fakeClusterHandle) FindHostByAddress(_ string) up.HostPtr { return nil }
func (h *fakeClusterHandle) PreInitComplete()                      { fmt.Println("[cluster] PreInitComplete") }

// Schedule runs fn on the "main thread" — in this example the calling goroutine.
func (h *fakeClusterHandle) Schedule(fn func()) { fn() }

func main() {
	handle := &fakeClusterHandle{}

	// Build the HostSet and HostRefreshLoop.
	hosts := up.NewHostSet[string](handle)

	snapFn := func(_ context.Context) (up.HostSnapshot[string], error) {
		return up.HostSnapshot[string]{
			"a": {Address: "127.0.0.1:8001"},
			"b": {Address: "127.0.0.1:8002"},
		}, nil
	}

	obs := func(e up.HostRefreshEvent) {
		fmt.Printf("[observer] added=%d removed=%d unchanged=%d duration=%s err=%v\n",
			e.Added, e.Removed, e.Unchanged, e.Duration.Round(time.Millisecond), e.Err)
	}

	loop := up.NewHostRefreshLoop[string](handle, hosts, snapFn, up.HostRefreshOptions{
		Interval: 10 * time.Second,
		Timeout:  time.Second,
		Observe:  obs,
	})

	// RefreshOnce warms the host set synchronously (equivalent to Init-time Apply).
	fmt.Println("--- initial refresh ---")
	if err := loop.RefreshOnce(context.Background()); err != nil {
		fmt.Printf("RefreshOnce error: %v\n", err)
		return
	}

	// Simulate what ChooseHost would do: look up a provider by key.
	for _, key := range []string{"a", "b", "missing"} {
		ptr, ok := hosts.Get(key)
		if ok {
			fmt.Printf("[chooseHost] key=%q → host ptr non-nil=%v\n", key, ptr != nil)
		} else {
			fmt.Printf("[chooseHost] key=%q → not found\n", key)
		}
	}

	// Start the background ticker then stop it quickly.
	fmt.Println("--- starting loop ---")
	loop.Start()
	time.Sleep(50 * time.Millisecond)
	loop.Stop()
	fmt.Println("--- loop stopped ---")
}
