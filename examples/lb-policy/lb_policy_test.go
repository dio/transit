package lbpolicy

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
func (h *fakeLBHandle) HealthyHostAddress(_ uint32, _ int) (string, bool)    { return "", false }
func (h *fakeLBHandle) HealthyHostWeight(_ uint32, _ int) uint32             { return 0 }
func (h *fakeLBHandle) HostHealth(_ uint32, _ int) up.HostHealth             { return up.HostHealthy }
func (h *fakeLBHandle) HostStat(_ uint32, _ int, _ up.HostStat) uint64       { return 0 }
func (h *fakeLBHandle) MemberUpdateHostAddress(_ int, _ bool) (string, bool) { return "", false }

func TestChooseHost_picksFirst(t *testing.T) {
	p := &firstHostPolicy{}
	lb := &fakeLBHandle{healthyCount: 3}
	var priority, index uint32
	ok := p.ChooseHost(lb, nil, &priority, &index)
	require.True(t, ok)
	require.Equal(t, uint32(0), priority)
	require.Equal(t, uint32(0), index)
}

func TestChooseHost_noHostsReturnsFalse(t *testing.T) {
	p := &firstHostPolicy{}
	lb := &fakeLBHandle{healthyCount: 0}
	var priority, index uint32
	ok := p.ChooseHost(lb, nil, &priority, &index)
	require.False(t, ok)
}

func TestFactory_create(t *testing.T) {
	f := &firstHostFactory{}
	cf, err := f.Create(nil)
	require.NoError(t, err)
	require.NotNil(t, cf)
	lb := cf.NewLBPolicy()
	require.NotNil(t, lb)
}
