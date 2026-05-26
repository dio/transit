package lbpolicyheaderhash

import (
	"hash/fnv"
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

// fakeLBContext is a minimal LBContext for unit-testing ChooseHost.
type fakeLBContext struct {
	headers map[string]string
}

func (c *fakeLBContext) GetHeader(name string) (string, bool) {
	if c == nil {
		return "", false
	}
	v, ok := c.headers[name]
	return v, ok
}
func (c *fakeLBContext) GetAllHeaders() [][2]string                                  { return nil }
func (c *fakeLBContext) GetOverrideHost() (string, bool)                             { return "", false }
func (c *fakeLBContext) ComputeHashKey() (uint64, bool)                              { return 0, false }
func (c *fakeLBContext) GetHostSelectionRetryCount() uint32                          { return 0 }
func (c *fakeLBContext) ShouldSelectAnotherHost(_ up.LBHandle, _ uint32, _ int) bool { return false }

func fnv32a(s string) uint32 {
	h := fnv.New32a()
	h.Write([]byte(s))
	return h.Sum32()
}

func TestChooseHost_hashesHeader(t *testing.T) {
	p := &headerHashPolicy{}
	lb := &fakeLBHandle{healthyCount: 3}
	ctx := &fakeLBContext{headers: map[string]string{"x-session-id": "abc"}}
	var priority, index uint32
	ok := p.ChooseHost(lb, ctx, &priority, &index)
	require.True(t, ok)
	require.Equal(t, uint32(0), priority)
	require.Equal(t, uint32(int(fnv32a("abc"))%3), index)
}

func TestChooseHost_noHeader_picksFirst(t *testing.T) {
	p := &headerHashPolicy{}
	lb := &fakeLBHandle{healthyCount: 3}
	ctx := &fakeLBContext{headers: map[string]string{}}
	var priority, index uint32
	ok := p.ChooseHost(lb, ctx, &priority, &index)
	require.True(t, ok)
	require.Equal(t, uint32(0), priority)
	require.Equal(t, uint32(0), index)
}

func TestChooseHost_emptyHosts(t *testing.T) {
	p := &headerHashPolicy{}
	lb := &fakeLBHandle{healthyCount: 0}
	ctx := &fakeLBContext{headers: map[string]string{"x-session-id": "abc"}}
	var priority, index uint32
	ok := p.ChooseHost(lb, ctx, &priority, &index)
	require.False(t, ok)
}

func TestChooseHost_sameHeaderSameIndex(t *testing.T) {
	p := &headerHashPolicy{}
	lb := &fakeLBHandle{healthyCount: 5}
	ctx := &fakeLBContext{headers: map[string]string{"x-session-id": "stable-key"}}
	var first uint32
	for i := range 10 {
		var priority, index uint32
		ok := p.ChooseHost(lb, ctx, &priority, &index)
		require.True(t, ok)
		if i == 0 {
			first = index
		} else {
			require.Equal(t, first, index, "iteration %d produced different index", i)
		}
	}
}

func TestFactory_create(t *testing.T) {
	f := &headerHashFactory{}
	cf, err := f.Create(nil)
	require.NoError(t, err)
	require.NotNil(t, cf)
	lb := cf.NewLBPolicy()
	require.NotNil(t, lb)
}
