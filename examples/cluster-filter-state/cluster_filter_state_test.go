package clusterfilterstate

import (
	"testing"
	"unsafe"

	"github.com/stretchr/testify/require"

	"github.com/dio/transit/up"
)

func TestChooseHost_matchesFilterState(t *testing.T) {
	ptrs := []up.HostPtr{fakeHostPtr(), fakeHostPtr()}
	lb := &filterStateRouterLB{
		hosts: []up.HostSpec{
			{Address: "127.0.0.1:9001"},
			{Address: "127.0.0.1:9002"},
		},
		ptrs: ptrs,
	}
	h := &fakeClusterLBHandle{healthyHosts: ptrs}
	ctx := &fakeClusterLBContext{filterState: map[string]string{
		"transit.target_host": "127.0.0.1:9001",
	}}

	chosen, completion := lb.ChooseHost(h, ctx)

	require.Equal(t, ptrs[0], chosen)
	require.Nil(t, completion)
}

func TestChooseHost_noFilterState_fallsBack(t *testing.T) {
	ptrs := []up.HostPtr{fakeHostPtr(), fakeHostPtr()}
	lb := &filterStateRouterLB{
		hosts: []up.HostSpec{
			{Address: "127.0.0.1:9001"},
			{Address: "127.0.0.1:9002"},
		},
		ptrs: ptrs,
	}
	h := &fakeClusterLBHandle{healthyHosts: ptrs}
	ctx := &fakeClusterLBContext{filterState: map[string]string{}}

	chosen, completion := lb.ChooseHost(h, ctx)

	// Falls back to first healthy host (index 0).
	require.Equal(t, ptrs[0], chosen)
	require.Nil(t, completion)
}

func TestChooseHost_unknownHost_fallsBack(t *testing.T) {
	ptrs := []up.HostPtr{fakeHostPtr(), fakeHostPtr()}
	lb := &filterStateRouterLB{
		hosts: []up.HostSpec{
			{Address: "127.0.0.1:9001"},
			{Address: "127.0.0.1:9002"},
		},
		ptrs: ptrs,
	}
	h := &fakeClusterLBHandle{healthyHosts: ptrs}
	ctx := &fakeClusterLBContext{filterState: map[string]string{
		"transit.target_host": "10.0.0.1:99",
	}}

	chosen, completion := lb.ChooseHost(h, ctx)

	// Unknown address: no match, FindHostByAddress returns nil → fallback.
	require.Equal(t, ptrs[0], chosen)
	require.Nil(t, completion)
}

// =============================================================================
// Fakes
// =============================================================================

type fakeClusterLBContext struct {
	filterState map[string]string
}

func (c *fakeClusterLBContext) GetFilterState(key string) (string, bool) {
	v, ok := c.filterState[key]
	return v, ok
}

func (c *fakeClusterLBContext) GetFilterStateTyped(key string) (string, bool) {
	v, ok := c.filterState[key]
	return v, ok
}

func (c *fakeClusterLBContext) GetAllHeaders() [][2]string         { return nil }
func (c *fakeClusterLBContext) GetOverrideHost() (string, bool)    { return "", false }
func (c *fakeClusterLBContext) GetHeader(_ string) (string, bool)  { return "", false }
func (c *fakeClusterLBContext) GetDownstreamSNI() (string, bool)   { return "", false }
func (c *fakeClusterLBContext) ComputeHashKey() (uint64, bool)     { return 0, false }
func (c *fakeClusterLBContext) GetHostSelectionRetryCount() uint32 { return 0 }
func (c *fakeClusterLBContext) ShouldSelectAnotherHost(_ up.ClusterLBHandle, _ uint32, _ int) bool {
	return false
}
func (c *fakeClusterLBContext) NewCompletion() *up.ClusterLBCompletion { return nil }

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
func (h *fakeClusterLBHandle) HostWeight(_ uint32, _ int) uint32        { return 0 }
func (h *fakeClusterLBHandle) HealthyHostWeight(_ uint32, _ int) uint32 { return 0 }
func (h *fakeClusterLBHandle) HostHealth(_ uint32, _ int) up.HostHealth { return up.HostHealthy }
func (h *fakeClusterLBHandle) HostHealthByAddress(_ string) (up.HostHealth, bool) {
	return 0, false
}
func (h *fakeClusterLBHandle) HostStat(_ uint32, _ int, _ up.HostStat) uint64 { return 0 }
func (h *fakeClusterLBHandle) FindHostByAddress(_ string) up.HostPtr          { return nil }
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
