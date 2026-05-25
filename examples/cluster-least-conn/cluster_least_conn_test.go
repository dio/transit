package clusterleastconn

import (
	"testing"
	"unsafe"

	"github.com/stretchr/testify/require"

	"github.com/dio/transit/up"
)

// fakeClusterLBHandle implements up.ClusterLBHandle for unit tests.
// hosts holds all hosts in all-hosts order; stats maps index→active-request count.
type fakeClusterLBHandle struct {
	hosts []up.HostPtr
	stats []uint64 // parallel to hosts; HostStatRqActive values
}

func (h *fakeClusterLBHandle) ClusterName() string           { return "test" }
func (h *fakeClusterLBHandle) PriorityCount() int            { return 1 }
func (h *fakeClusterLBHandle) HostCount(_ uint32) int        { return len(h.hosts) }
func (h *fakeClusterLBHandle) HealthyHostCount(_ uint32) int { return len(h.hosts) }
func (h *fakeClusterLBHandle) DegradedHostCount(_ uint32) int { return 0 }
func (h *fakeClusterLBHandle) Host(_ uint32, index int) up.HostPtr {
	return h.hosts[index]
}
func (h *fakeClusterLBHandle) HealthyHost(_ uint32, index int) up.HostPtr {
	return h.hosts[index]
}
func (h *fakeClusterLBHandle) HostAddress(_ uint32, _ int) (string, bool)        { return "", false }
func (h *fakeClusterLBHandle) HealthyHostAddress(_ uint32, _ int) (string, bool) { return "", false }
func (h *fakeClusterLBHandle) HostWeight(_ uint32, _ int) uint32                 { return 0 }
func (h *fakeClusterLBHandle) HealthyHostWeight(_ uint32, _ int) uint32          { return 0 }
func (h *fakeClusterLBHandle) HostHealth(_ uint32, _ int) up.HostHealth          { return up.HostHealthy }
func (h *fakeClusterLBHandle) HostHealthByAddress(_ string) (up.HostHealth, bool) {
	return 0, false
}
func (h *fakeClusterLBHandle) HostStat(_ uint32, index int, stat up.HostStat) uint64 {
	if stat == up.HostStatRqActive && index < len(h.stats) {
		return h.stats[index]
	}
	return 0
}
func (h *fakeClusterLBHandle) FindHostByAddress(_ string) up.HostPtr { return nil }
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

func makeHandle(activeCounts []uint64) *fakeClusterLBHandle {
	hosts := make([]up.HostPtr, len(activeCounts))
	for i := range hosts {
		hosts[i] = fakeHostPtr()
	}
	return &fakeClusterLBHandle{hosts: hosts, stats: activeCounts}
}

// TestChooseHost_picksLeastActive: three hosts with active counts [5,1,3] → index 1.
func TestChooseHost_picksLeastActive(t *testing.T) {
	handle := makeHandle([]uint64{5, 1, 3})
	lb := &leastConnClusterLB{}

	chosen, completion := lb.ChooseHost(handle, nil)

	require.Equal(t, handle.hosts[1], chosen)
	require.Nil(t, completion)
}

// TestChooseHost_allEqual_picksFirst: [2,2,2] → index 0 (first minimum wins).
func TestChooseHost_allEqual_picksFirst(t *testing.T) {
	handle := makeHandle([]uint64{2, 2, 2})
	lb := &leastConnClusterLB{}

	chosen, completion := lb.ChooseHost(handle, nil)

	require.Equal(t, handle.hosts[0], chosen)
	require.Nil(t, completion)
}

// TestChooseHost_noHosts_returnsNil: empty host set → nil, nil.
func TestChooseHost_noHosts_returnsNil(t *testing.T) {
	handle := makeHandle(nil)
	lb := &leastConnClusterLB{}

	chosen, completion := lb.ChooseHost(handle, nil)

	require.Nil(t, chosen)
	require.Nil(t, completion)
}

// TestChooseHost_singleHost: only one host → picks it regardless of count.
func TestChooseHost_singleHost(t *testing.T) {
	handle := makeHandle([]uint64{42})
	lb := &leastConnClusterLB{}

	chosen, completion := lb.ChooseHost(handle, nil)

	require.Equal(t, handle.hosts[0], chosen)
	require.Nil(t, completion)
}

// TestFactory_create: valid config creates a config factory without error.
func TestFactory_create(t *testing.T) {
	f := &leastConnFactory{}
	cf, err := f.Create([]byte(`{"hosts":[{"address":"127.0.0.1:8080"}]}`))
	require.NoError(t, err)
	require.NotNil(t, cf)
}

// TestFactory_create_emptyConfig_errors: empty config returns an error.
func TestFactory_create_emptyConfig_errors(t *testing.T) {
	f := &leastConnFactory{}
	_, err := f.Create([]byte{})
	require.Error(t, err)
}
