package lbpolicyretryaware

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/dio/transit/up"
)

// fakeLBHandle is a minimal LBHandle for unit-testing ChooseHost.
type fakeLBHandle struct {
	healthyCount int
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
func (h *fakeLBHandle) HostMetadataString(_ uint32, _ int, _, _ string) (string, bool) {
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

// fakeLBContext is an LBContext whose ShouldSelectAnotherHost delegates to a
// user-supplied closure. The closure receives the host index being evaluated
// and returns true when that host should be skipped (already tried).
type fakeLBContext struct {
	shouldSkip func(index int) bool
}

func (c *fakeLBContext) GetAllHeaders() [][2]string                  { return nil }
func (c *fakeLBContext) GetOverrideHost() (string, bool)             { return "", false }
func (c *fakeLBContext) GetHeader(_ string) (string, bool)           { return "", false }
func (c *fakeLBContext) ComputeHashKey() (uint64, bool)              { return 0, false }
func (c *fakeLBContext) GetHostSelectionRetryCount() uint32          { return 0 }
func (c *fakeLBContext) ShouldSelectAnotherHost(_ up.LBHandle, _ uint32, index int) bool {
	if c.shouldSkip == nil {
		return false
	}
	return c.shouldSkip(index)
}

// noneSkipped returns a context where no host is considered already tried.
func noneSkipped() *fakeLBContext {
	return &fakeLBContext{shouldSkip: func(_ int) bool { return false }}
}

// skipIndices returns a context that marks the given indices as already tried.
func skipIndices(tried ...int) *fakeLBContext {
	set := make(map[int]bool, len(tried))
	for _, i := range tried {
		set[i] = true
	}
	return &fakeLBContext{shouldSkip: func(i int) bool { return set[i] }}
}

// TestChooseHost_noRetry_picksFirst verifies that when no host has been tried
// the policy picks index 0.
func TestChooseHost_noRetry_picksFirst(t *testing.T) {
	p := &retryAwarePolicy{}
	lb := &fakeLBHandle{healthyCount: 3}
	ctx := noneSkipped()
	var priority, index uint32
	ok := p.ChooseHost(lb, ctx, &priority, &index)
	require.True(t, ok)
	require.Equal(t, uint32(0), priority)
	require.Equal(t, uint32(0), index)
}

// TestChooseHost_firstHostTried_picksSecond verifies that when the first host
// has already been tried the policy skips it and picks index 1.
func TestChooseHost_firstHostTried_picksSecond(t *testing.T) {
	p := &retryAwarePolicy{}
	lb := &fakeLBHandle{healthyCount: 3}
	ctx := skipIndices(0)
	var priority, index uint32
	ok := p.ChooseHost(lb, ctx, &priority, &index)
	require.True(t, ok)
	require.Equal(t, uint32(0), priority)
	require.Equal(t, uint32(1), index)
}

// TestChooseHost_allHostsTried_fallsBackToFirst verifies that when all hosts
// have been tried the policy falls back to index 0 and still returns true.
func TestChooseHost_allHostsTried_fallsBackToFirst(t *testing.T) {
	p := &retryAwarePolicy{}
	lb := &fakeLBHandle{healthyCount: 3}
	ctx := skipIndices(0, 1, 2)
	var priority, index uint32
	ok := p.ChooseHost(lb, ctx, &priority, &index)
	require.True(t, ok)
	require.Equal(t, uint32(0), priority)
	require.Equal(t, uint32(0), index)
}

// TestChooseHost_emptyHosts_returnsFalse verifies that when there are no
// healthy hosts the policy returns false.
func TestChooseHost_emptyHosts_returnsFalse(t *testing.T) {
	p := &retryAwarePolicy{}
	lb := &fakeLBHandle{healthyCount: 0}
	ctx := noneSkipped()
	var priority, index uint32
	ok := p.ChooseHost(lb, ctx, &priority, &index)
	require.False(t, ok)
}

// TestFactory_create verifies that the factory creates a usable LBPolicy.
func TestFactory_create(t *testing.T) {
	f := &retryAwareFactory{}
	cf, err := f.Create(nil)
	require.NoError(t, err)
	require.NotNil(t, cf)
	lb := cf.NewLBPolicy()
	require.NotNil(t, lb)
}
