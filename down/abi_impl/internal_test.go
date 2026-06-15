package abi_impl

import (
	"testing"
	"unsafe"

	"github.com/stretchr/testify/require"
)

func TestManager_recordUnwrapRemove(t *testing.T) {
	t.Parallel()

	m := newManager[int]()
	value := 42

	ptr := m.record(&value)
	require.NotNil(t, ptr)
	require.NotEqual(t, unsafe.Pointer(&value), ptr)
	require.Same(t, &value, m.unwrap(ptr))

	shard := uintptr(ptr) % numManagerShards
	m.mutex[shard].Lock()
	_, recorded := m.data[shard][uintptr(ptr)]
	m.mutex[shard].Unlock()
	require.True(t, recorded)

	m.remove(ptr)

	m.mutex[shard].Lock()
	_, recorded = m.data[shard][uintptr(ptr)]
	m.mutex[shard].Unlock()
	require.False(t, recorded)
}

func TestClusterHandleImpl_runPending(t *testing.T) {
	t.Parallel()

	h := &clusterHandleImpl{
		pending: map[uint64]func(){
			7: func() {},
		},
	}

	called := false
	h.pending[7] = func() {
		called = true
	}

	h.runPending(7)
	require.True(t, called)
	require.NotContains(t, h.pending, uint64(7))

	require.NotPanics(t, func() {
		h.runPending(7)
	})
}
