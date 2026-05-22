package cluster

import (
	"testing"
	"unsafe"

	"github.com/stretchr/testify/require"

	"github.com/dio/transit/up"
)

func TestParseHosts(t *testing.T) {
	hosts, err := parseHosts([]byte(`{"hosts":[{"address":"127.0.0.1:8080","weight":7}]}`))
	require.NoError(t, err)
	require.Equal(t, []up.HostSpec{{Address: "127.0.0.1:8080", Weight: 7}}, hosts)
}

func TestParseHostsRequiresHost(t *testing.T) {
	_, err := parseHosts([]byte(`{"hosts":[]}`))
	require.Error(t, err)
}

func TestClusterInitAddsHostsAndCompletesInit(t *testing.T) {
	h := &fakeClusterHandle{}
	c := &staticHostsCluster{hosts: []up.HostSpec{{Address: "127.0.0.1:8080"}}}

	c.Init(h)

	require.Equal(t, c.hosts, h.added)
	require.True(t, h.preInitComplete)
	require.Len(t, h.healthUpdates, 1)
	require.Equal(t, up.HostHealthy, h.healthUpdates[0].health)
}

func TestClusterLBChooseHost(t *testing.T) {
	host := fakeHostPtr()
	lb := &firstHealthyClusterLB{}

	chosen, completion := lb.ChooseHost(&fakeClusterLBHandle{healthyHosts: []up.HostPtr{host}}, nil)

	require.Equal(t, host, chosen)
	require.Nil(t, completion)
}

func TestClusterLBChooseHostNoHealthyHosts(t *testing.T) {
	lb := &firstHealthyClusterLB{}

	chosen, completion := lb.ChooseHost(&fakeClusterLBHandle{}, nil)

	require.Nil(t, chosen)
	require.Nil(t, completion)
}

type fakeClusterHandle struct {
	added           []up.HostSpec
	ptrs            []up.HostPtr
	preInitComplete bool
	healthUpdates   []healthUpdate
}

type healthUpdate struct {
	host   up.HostPtr
	health up.HostHealth
}

func (h *fakeClusterHandle) AddHosts(hosts []up.HostSpec) []up.HostPtr {
	h.added = append([]up.HostSpec(nil), hosts...)
	h.ptrs = make([]up.HostPtr, len(hosts))
	for i := range hosts {
		h.ptrs[i] = fakeHostPtr()
	}
	return h.ptrs
}

func (h *fakeClusterHandle) RemoveHosts(_ []up.HostPtr) {}
func (h *fakeClusterHandle) UpdateHostHealth(host up.HostPtr, health up.HostHealth) {
	h.healthUpdates = append(h.healthUpdates, healthUpdate{host: host, health: health})
}
func (h *fakeClusterHandle) FindHostByAddress(_ string) up.HostPtr { return nil }
func (h *fakeClusterHandle) PreInitComplete()                      { h.preInitComplete = true }
func (h *fakeClusterHandle) Schedule(fn func())                    { fn() }

type fakeClusterLBHandle struct {
	healthyHosts []up.HostPtr
}

func (h *fakeClusterLBHandle) ClusterName() string                 { return "test" }
func (h *fakeClusterLBHandle) PriorityCount() int                  { return 1 }
func (h *fakeClusterLBHandle) HostCount(_ uint32) int              { return len(h.healthyHosts) }
func (h *fakeClusterLBHandle) HealthyHostCount(_ uint32) int       { return len(h.healthyHosts) }
func (h *fakeClusterLBHandle) DegradedHostCount(_ uint32) int      { return 0 }
func (h *fakeClusterLBHandle) Host(_ uint32, index int) up.HostPtr { return h.healthyHosts[index] }
func (h *fakeClusterLBHandle) HealthyHost(_ uint32, index int) up.HostPtr {
	return h.healthyHosts[index]
}
func (h *fakeClusterLBHandle) HostAddress(_ uint32, _ int) (string, bool) {
	return "", false
}
func (h *fakeClusterLBHandle) HealthyHostAddress(_ uint32, _ int) (string, bool) {
	return "", false
}
func (h *fakeClusterLBHandle) HostWeight(_ uint32, _ int) uint32                  { return 0 }
func (h *fakeClusterLBHandle) HealthyHostWeight(_ uint32, _ int) uint32           { return 0 }
func (h *fakeClusterLBHandle) HostHealth(_ uint32, _ int) up.HostHealth           { return up.HostHealthy }
func (h *fakeClusterLBHandle) HostHealthByAddress(_ string) (up.HostHealth, bool) { return 0, false }
func (h *fakeClusterLBHandle) HostStat(_ uint32, _ int, _ up.HostStat) uint64     { return 0 }
func (h *fakeClusterLBHandle) FindHostByAddress(_ string) up.HostPtr              { return nil }
func (h *fakeClusterLBHandle) MemberUpdateHostAddress(_ int, _ bool) (string, bool) {
	return "", false
}
func (h *fakeClusterLBHandle) HostLocality(_ uint32, _ int) (string, string, string, bool) {
	return "", "", "", false
}
func (h *fakeClusterLBHandle) SetHostData(_ uint32, _ int, _ uintptr) bool { return false }
func (h *fakeClusterLBHandle) GetHostData(_ uint32, _ int) (uintptr, bool) { return 0, false }
func (h *fakeClusterLBHandle) HostMetadataString(_ uint32, _ int, _, _ string) (string, bool) {
	return "", false
}
func (h *fakeClusterLBHandle) HostMetadataNumber(_ uint32, _ int, _, _ string) (float64, bool) {
	return 0, false
}
func (h *fakeClusterLBHandle) HostMetadataBool(_ uint32, _ int, _, _ string) (bool, bool) {
	return false, false
}
func (h *fakeClusterLBHandle) LocalityCount(_ uint32) int            { return 0 }
func (h *fakeClusterLBHandle) LocalityHostCount(_ uint32, _ int) int { return 0 }
func (h *fakeClusterLBHandle) LocalityHostAddress(_ uint32, _, _ int) (string, bool) {
	return "", false
}
func (h *fakeClusterLBHandle) LocalityWeight(_ uint32, _ int) uint32 { return 0 }

func fakeHostPtr() up.HostPtr {
	return up.HostPtr(unsafe.Pointer(new(int)))
}
