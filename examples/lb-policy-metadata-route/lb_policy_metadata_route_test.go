package lbpolicymetadataroute

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/dio/transit/up"
)

// fakeLBHandle is a test double for up.LBHandle that supports per-host
// capability metadata via a map keyed by host index.
type fakeLBHandle struct {
	healthyCount int
	// capabilities maps host index → capability string (for "envoy.lb"/"capability").
	capabilities map[int]string
}

func (h *fakeLBHandle) HealthyHostCount(_ uint32) int                        { return h.healthyCount }
func (h *fakeLBHandle) ClusterName() string                                  { return "test" }
func (h *fakeLBHandle) PriorityCount() int                                   { return 1 }
func (h *fakeLBHandle) HostCount(_ uint32) int                               { return h.healthyCount }
func (h *fakeLBHandle) DegradedHostCount(_ uint32) int                       { return 0 }
func (h *fakeLBHandle) HostAddress(_ uint32, _ int) (string, bool)           { return "", false }
func (h *fakeLBHandle) HealthyHostAddress(_ uint32, _ int) (string, bool)    { return "", false }
func (h *fakeLBHandle) HostWeight(_ uint32, _ int) uint32                    { return 0 }
func (h *fakeLBHandle) HealthyHostWeight(_ uint32, _ int) uint32             { return 0 }
func (h *fakeLBHandle) HostHealth(_ uint32, _ int) up.HostHealth             { return up.HostHealthy }
func (h *fakeLBHandle) HostHealthByAddress(_ string) (up.HostHealth, bool)   { return 0, false }
func (h *fakeLBHandle) HostStat(_ uint32, _ int, _ up.HostStat) uint64       { return 0 }
func (h *fakeLBHandle) MemberUpdateHostAddress(_ int, _ bool) (string, bool) { return "", false }
func (h *fakeLBHandle) HostLocality(_ uint32, _ int) (string, string, string, bool) {
	return "", "", "", false
}
func (h *fakeLBHandle) SetHostData(_ uint32, _ int, _ uintptr) bool { return false }
func (h *fakeLBHandle) GetHostData(_ uint32, _ int) (uintptr, bool) { return 0, false }
func (h *fakeLBHandle) HostMetadataString(_ uint32, index int, namespace, key string) (string, bool) {
	if namespace == "envoy.lb" && key == "capability" {
		if cap, ok := h.capabilities[index]; ok {
			return cap, true
		}
	}
	return "", false
}
func (h *fakeLBHandle) HostMetadataNumber(_ uint32, _ int, _, _ string) (float64, bool) {
	return 0, false
}
func (h *fakeLBHandle) HostMetadataBool(_ uint32, _ int, _, _ string) (bool, bool) {
	return false, false
}
func (h *fakeLBHandle) LocalityCount(_ uint32) int            { return 0 }
func (h *fakeLBHandle) LocalityHostCount(_ uint32, _ int) int { return 0 }
func (h *fakeLBHandle) LocalityHostAddress(_ uint32, _, _ int) (string, bool) {
	return "", false
}
func (h *fakeLBHandle) LocalityWeight(_ uint32, _ int) uint32 { return 0 }

// fakeLBContext is a test double for up.LBContext.
type fakeLBContext struct {
	headers map[string]string
}

func (c *fakeLBContext) GetHeader(key string) (string, bool) {
	if c == nil {
		return "", false
	}
	v, ok := c.headers[key]
	return v, ok
}
func (c *fakeLBContext) GetAllHeaders() [][2]string                                  { return nil }
func (c *fakeLBContext) GetOverrideHost() (string, bool)                             { return "", false }
func (c *fakeLBContext) ComputeHashKey() (uint64, bool)                              { return 0, false }
func (c *fakeLBContext) GetHostSelectionRetryCount() uint32                          { return 0 }
func (c *fakeLBContext) ShouldSelectAnotherHost(_ up.LBHandle, _ uint32, _ int) bool { return false }

// TestChooseHost_matchesCapability verifies that when x-required-capability
// matches host 1's capability ("cpu"), index 1 is selected.
func TestChooseHost_matchesCapability(t *testing.T) {
	p := &metadataRoutePolicy{}
	lb := &fakeLBHandle{
		healthyCount: 2,
		capabilities: map[int]string{0: "gpu", 1: "cpu"},
	}
	ctx := &fakeLBContext{headers: map[string]string{"x-required-capability": "cpu"}}
	var priority, index uint32
	ok := p.ChooseHost(lb, ctx, &priority, &index)
	require.True(t, ok)
	require.Equal(t, uint32(0), priority)
	require.Equal(t, uint32(1), index)
}

// TestChooseHost_noHeader_picksFirst verifies that without the capability
// header the policy falls back to index 0.
func TestChooseHost_noHeader_picksFirst(t *testing.T) {
	p := &metadataRoutePolicy{}
	lb := &fakeLBHandle{
		healthyCount: 2,
		capabilities: map[int]string{0: "gpu", 1: "cpu"},
	}
	ctx := &fakeLBContext{headers: map[string]string{}}
	var priority, index uint32
	ok := p.ChooseHost(lb, ctx, &priority, &index)
	require.True(t, ok)
	require.Equal(t, uint32(0), priority)
	require.Equal(t, uint32(0), index)
}

// TestChooseHost_noMatch_fallsBackToFirst verifies that when no host has the
// requested capability, the policy falls back to index 0.
func TestChooseHost_noMatch_fallsBackToFirst(t *testing.T) {
	p := &metadataRoutePolicy{}
	lb := &fakeLBHandle{
		healthyCount: 2,
		capabilities: map[int]string{0: "gpu", 1: "cpu"},
	}
	ctx := &fakeLBContext{headers: map[string]string{"x-required-capability": "fpga"}}
	var priority, index uint32
	ok := p.ChooseHost(lb, ctx, &priority, &index)
	require.True(t, ok)
	require.Equal(t, uint32(0), priority)
	require.Equal(t, uint32(0), index)
}

// TestChooseHost_emptyHosts verifies that ChooseHost returns false when there
// are no healthy hosts.
func TestChooseHost_emptyHosts(t *testing.T) {
	p := &metadataRoutePolicy{}
	lb := &fakeLBHandle{healthyCount: 0}
	ctx := &fakeLBContext{headers: map[string]string{"x-required-capability": "gpu"}}
	var priority, index uint32
	ok := p.ChooseHost(lb, ctx, &priority, &index)
	require.False(t, ok)
}

// TestFactory_create verifies that the factory chain produces a usable policy.
func TestFactory_create(t *testing.T) {
	f := &metadataRouteFactory{}
	cf, err := f.Create(nil)
	require.NoError(t, err)
	require.NotNil(t, cf)
	lb := cf.NewLBPolicy()
	require.NotNil(t, lb)
}
