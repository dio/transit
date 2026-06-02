package up

// recordingClusterHandle is a ClusterHandle test double that tracks call order
// for AddHosts, RemoveHosts, and UpdateHostHealth. It is local to the
// host_set/host_refresh_loop tests and does not modify the shared fakeClusterHandle
// defined in cluster_group_test.go.

import (
	"sync"
	"sync/atomic"
	"unsafe"
)

// hostCallKind identifies the kind of ClusterHandle call recorded.
type hostCallKind string

const (
	callAddHosts         hostCallKind = "AddHosts"
	callRemoveHosts      hostCallKind = "RemoveHosts"
	callUpdateHostHealth hostCallKind = "UpdateHostHealth"
)

// hostCall records one ClusterHandle mutation call.
type hostCall struct {
	Kind   hostCallKind
	Hosts  []HostPtr
	Specs  []HostSpec
	Health HostHealth
}

// hostBump is the backing store for generated HostPtrs. Each call to AddHosts
// allocates a new uint64 per spec; the pointer to that uint64 is used as the
// HostPtr. This keeps HostPtrs unique, non-nil, and GC-safe.
var hostBump atomic.Uint64

func newTestHostPtr() HostPtr {
	v := new(uint64)
	*v = hostBump.Add(1)
	return HostPtr(unsafe.Pointer(v))
}

// recordingHandle satisfies ClusterHandle and records all mutation calls in
// the order they occur. Thread-safe.
type recordingHandle struct {
	mu    sync.Mutex
	calls []hostCall

	// scheduleFn optionally replaces the default synchronous Schedule behaviour.
	// Useful for simulating a slow main thread.
	scheduleFn func(fn func())
}

func (h *recordingHandle) AddHosts(specs []HostSpec) []HostPtr {
	ptrs := make([]HostPtr, len(specs))
	for i := range specs {
		ptrs[i] = newTestHostPtr()
	}
	h.mu.Lock()
	cp := make([]HostSpec, len(specs))
	copy(cp, specs)
	pcp := make([]HostPtr, len(ptrs))
	copy(pcp, ptrs)
	h.calls = append(h.calls, hostCall{Kind: callAddHosts, Specs: cp, Hosts: pcp})
	h.mu.Unlock()
	return ptrs
}

func (h *recordingHandle) RemoveHosts(hosts []HostPtr) {
	h.mu.Lock()
	cp := make([]HostPtr, len(hosts))
	copy(cp, hosts)
	h.calls = append(h.calls, hostCall{Kind: callRemoveHosts, Hosts: cp})
	h.mu.Unlock()
}

func (h *recordingHandle) UpdateHostHealth(host HostPtr, health HostHealth) {
	h.mu.Lock()
	h.calls = append(h.calls, hostCall{Kind: callUpdateHostHealth, Hosts: []HostPtr{host}, Health: health})
	h.mu.Unlock()
}

func (h *recordingHandle) FindHostByAddress(_ string) HostPtr { return nil }
func (h *recordingHandle) PreInitComplete()                   {}
func (h *recordingHandle) Schedule(fn func()) {
	if h.scheduleFn != nil {
		h.scheduleFn(fn)
		return
	}
	fn()
}

// Calls returns a copy of the call log.
func (h *recordingHandle) Calls() []hostCall {
	h.mu.Lock()
	defer h.mu.Unlock()
	cp := make([]hostCall, len(h.calls))
	copy(cp, h.calls)
	return cp
}

// Reset clears the call log.
func (h *recordingHandle) Reset() {
	h.mu.Lock()
	h.calls = h.calls[:0]
	h.mu.Unlock()
}

// callsOfKind returns only calls of a given kind.
func (h *recordingHandle) callsOfKind(k hostCallKind) []hostCall {
	var out []hostCall
	for _, c := range h.Calls() {
		if c.Kind == k {
			out = append(out, c)
		}
	}
	return out
}
