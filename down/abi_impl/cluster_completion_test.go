package abi_impl_test

import (
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/dio/transit/down"
)

func TestClusterLBCompletion_Complete_once(t *testing.T) {
	var completed atomic.Uint32
	var finished atomic.Uint32
	c := &down.ClusterLBCompletion{}
	c.SetCompleteFn(func(down.HostPtr, string) {
		completed.Add(1)
	})
	c.SetFinishFn(func() {
		finished.Add(1)
	})

	require.True(t, c.Complete(nil, ""))
	require.False(t, c.Complete(nil, "again"))
	require.False(t, c.Cancel())
	require.Equal(t, uint32(1), completed.Load())
	require.Equal(t, uint32(1), finished.Load())
}

func TestClusterLBCompletion_Cancel_preventsComplete(t *testing.T) {
	var completed atomic.Uint32
	var cancelled atomic.Uint32
	var finished atomic.Uint32
	c := &down.ClusterLBCompletion{}
	c.SetCompleteFn(func(down.HostPtr, string) {
		completed.Add(1)
	})
	c.SetCancelFn(func() {
		cancelled.Add(1)
	})
	c.SetFinishFn(func() {
		finished.Add(1)
	})

	require.True(t, c.Cancel())
	require.False(t, c.Complete(nil, "late"))
	require.False(t, c.Cancel())
	require.Equal(t, uint32(0), completed.Load())
	require.Equal(t, uint32(1), cancelled.Load())
	require.Equal(t, uint32(1), finished.Load())
}

func TestClusterLBCompletion_SetFinishFn_afterComplete(t *testing.T) {
	var finished atomic.Uint32
	c := &down.ClusterLBCompletion{}

	require.True(t, c.Complete(nil, ""))
	c.SetFinishFn(func() {
		finished.Add(1)
	})

	require.Equal(t, uint32(1), finished.Load())
}
