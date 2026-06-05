package loopback

import (
	"log/slog"
	"net"
	"sync"
	"testing"
	"time"
	"unsafe"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/dio/transit/up"
)

// recordingHandle records calls made by cluster.Init.
type recordingHandle struct {
	mu               sync.Mutex
	addedSpecs       []up.HostSpec
	addedPtrs        []up.HostPtr
	healthUpdates    []healthUpdate
	preInitCompleted bool
	bump             uint64
}

type healthUpdate struct {
	ptr    up.HostPtr
	health up.HostHealth
}

func (h *recordingHandle) AddHosts(specs []up.HostSpec) []up.HostPtr {
	ptrs := make([]up.HostPtr, len(specs))
	for i := range specs {
		h.bump++
		v := new(uint64)
		*v = h.bump
		ptrs[i] = up.HostPtr(unsafe.Pointer(v))
	}
	cp := make([]up.HostSpec, len(specs))
	copy(cp, specs)
	pcp := make([]up.HostPtr, len(ptrs))
	copy(pcp, ptrs)
	h.mu.Lock()
	h.addedSpecs = append(h.addedSpecs, cp...)
	h.addedPtrs = append(h.addedPtrs, pcp...)
	h.mu.Unlock()
	return ptrs
}

func (h *recordingHandle) RemoveHosts(_ []up.HostPtr)            {}
func (h *recordingHandle) FindHostByAddress(_ string) up.HostPtr { return nil }
func (h *recordingHandle) Schedule(fn func())                    { fn() }
func (h *recordingHandle) UpdateHostHealth(ptr up.HostPtr, health up.HostHealth) {
	h.mu.Lock()
	h.healthUpdates = append(h.healthUpdates, healthUpdate{ptr: ptr, health: health})
	h.mu.Unlock()
}
func (h *recordingHandle) PreInitComplete() {
	h.mu.Lock()
	h.preInitCompleted = true
	h.mu.Unlock()
}

func (h *recordingHandle) addedCount() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.addedSpecs)
}

func newCluster(listenAddr string) *cluster {
	return &cluster{
		logger: slog.Default(),
		cfg:    clusterConfig{ListenAddr: listenAddr},
	}
}

func TestInitBindSuccess(t *testing.T) {
	h := &recordingHandle{}
	c := newCluster("127.0.0.1:0")
	c.Init(h)

	assert.True(t, h.preInitCompleted, "PreInitComplete must be called")
	require.Equal(t, 1, h.addedCount(), "AddHosts must be called once on success")

	require.Len(t, h.healthUpdates, 1, "UpdateHostHealth must be called once")
	assert.Equal(t, up.HostHealthy, h.healthUpdates[0].health)

	assert.NotNil(t, c.host, "cluster.host must be set")
	assert.NotNil(t, c.sc, "cluster.sc must be set")

	// ChooseHost returns the registered host.
	gotHost, completion := c.NewClusterLB().ChooseHost(nil, nil)
	assert.Equal(t, c.host, gotHost)
	assert.Nil(t, completion)

	// Shutdown stops the sidecar and calls done.
	doneCalled := make(chan struct{})
	c.ServerInitialized(nil)
	c.Shutdown(nil, func() { close(doneCalled) })
	select {
	case <-doneCalled:
	case <-time.After(3 * time.Second):
		t.Fatal("Shutdown did not call done in time")
	}
}

func TestInitBindSuccessUDS(t *testing.T) {
	sockPath := t.TempDir() + "/rws.sock"
	h := &recordingHandle{}
	c := newCluster("unix://" + sockPath)
	c.Init(h)

	assert.True(t, h.preInitCompleted)
	require.Equal(t, 1, h.addedCount(), "AddHosts must be called for UDS")
	assert.Equal(t, sockPath, h.addedSpecs[0].Address)

	doneCalled := make(chan struct{})
	c.ServerInitialized(nil)
	c.Shutdown(nil, func() { close(doneCalled) })
	select {
	case <-doneCalled:
	case <-time.After(3 * time.Second):
		t.Fatal("Shutdown did not call done in time")
	}
}

func TestInitBindFail(t *testing.T) {
	// Occupy the port so the sidecar Listen fails.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	defer ln.Close()

	h := &recordingHandle{}
	c := newCluster(ln.Addr().String())
	c.Init(h)

	assert.True(t, h.preInitCompleted, "PreInitComplete must be called even on bind failure")
	assert.Equal(t, 0, h.addedCount(), "AddHosts must not be called on bind failure")
	assert.Nil(t, c.sc, "cluster.sc must remain nil on bind failure")

	// ChooseHost returns nil (no host registered).
	gotHost, completion := c.NewClusterLB().ChooseHost(nil, nil)
	assert.Nil(t, gotHost)
	assert.Nil(t, completion)

	doneCalled := make(chan struct{})
	c.ServerInitialized(nil)
	c.Shutdown(nil, func() { close(doneCalled) })
	select {
	case <-doneCalled:
	case <-time.After(3 * time.Second):
		t.Fatal("Shutdown did not call done in time")
	}
}
