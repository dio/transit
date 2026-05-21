package abi_impl

/*
#cgo darwin LDFLAGS: -Wl,-undefined,dynamic_lookup
#include <stdbool.h>
#include <stdint.h>
#include <stdlib.h>
#include <string.h>
#include "abi.h"
*/
import "C"
import (
	"fmt"
	"runtime"
	"sync"
	"unsafe"

	"github.com/envoyproxy/envoy/source/extensions/dynamic_modules/sdk/go/shared"
)

// manager pins Go objects so their addresses can be round-tripped through C as
// opaque pointers. Sharded to reduce lock contention across worker threads.
const numManagerShards = 32

type manager[T any] struct {
	data  [numManagerShards]map[uintptr]*T
	mutex [numManagerShards]sync.Mutex
}

func (m *manager[T]) record(item *T) unsafe.Pointer {
	ptr := unsafe.Pointer(item)
	idx := uintptr(ptr) % numManagerShards
	m.mutex[idx].Lock()
	defer m.mutex[idx].Unlock()
	m.data[idx][uintptr(ptr)] = item
	return ptr
}

func (m *manager[T]) unwrap(ptr unsafe.Pointer) *T {
	return (*T)(ptr)
}

func (m *manager[T]) remove(ptr unsafe.Pointer) {
	idx := uintptr(ptr) % numManagerShards
	m.mutex[idx].Lock()
	defer m.mutex[idx].Unlock()
	delete(m.data[idx], uintptr(ptr))
}

func newManager[T any]() *manager[T] {
	m := &manager[T]{}
	for i := range m.data {
		m.data[i] = make(map[uintptr]*T)
	}
	return m
}

// stringToModuleBuffer returns a C buffer view over a Go string.
// The caller must ensure the string outlives the C call (use runtime.KeepAlive).
func stringToModuleBuffer(s string) C.envoy_dynamic_module_type_module_buffer {
	return C.envoy_dynamic_module_type_module_buffer{
		ptr:    (*C.char)(unsafe.Pointer(unsafe.StringData(s))),
		length: C.size_t(len(s)),
	}
}

// bytesToModuleBuffer returns a C buffer view over a Go byte slice.
func bytesToModuleBuffer(b []byte) C.envoy_dynamic_module_type_module_buffer {
	return C.envoy_dynamic_module_type_module_buffer{
		ptr:    (*C.char)(unsafe.Pointer(unsafe.SliceData(b))),
		length: C.size_t(len(b)),
	}
}

func envoyBufferToStringUnsafe(buf C.envoy_dynamic_module_type_envoy_buffer) string {
	return unsafe.String((*byte)(unsafe.Pointer(buf.ptr)), buf.length)
}

func envoyBufferToBytesUnsafe(buf C.envoy_dynamic_module_type_envoy_buffer) []byte {
	return unsafe.Slice((*byte)(unsafe.Pointer(buf.ptr)), buf.length)
}

func envoyBufferToUnsafeEnvoyBuffer(buf C.envoy_dynamic_module_type_envoy_buffer) shared.UnsafeEnvoyBuffer {
	return shared.UnsafeEnvoyBuffer{
		Ptr: (*byte)(unsafe.Pointer(buf.ptr)),
		Len: uint64(buf.length),
	}
}

func hostLog(level shared.LogLevel, format string, args []any) {
	if !bool(C.envoy_dynamic_module_callback_log_enabled(
		C.envoy_dynamic_module_type_log_level(level),
	)) {
		return
	}
	msg := fmt.Sprintf(format, args...)
	C.envoy_dynamic_module_callback_log(
		C.envoy_dynamic_module_type_log_level(level),
		stringToModuleBuffer(msg),
	)
	runtime.KeepAlive(msg)
}
