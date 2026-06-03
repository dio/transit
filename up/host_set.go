package up

import (
	"maps"
	"sync/atomic"
)

// HostSnapshot is the complete desired host set keyed by caller's key type.
// A missing key means "remove that host". Pass nil or an empty map to remove all.
type HostSnapshot[K comparable] map[K]HostSpec

// HostEntry pairs a user key's resolved spec with its live Envoy HostPtr.
type HostEntry struct {
	Spec HostSpec
	Host HostPtr
}

// specEqual returns true when two HostSpecs are identical for diff purposes.
// Metadata is part of the diff key because Envoy binds it at AddHosts time.
func specEqual(a, b HostSpec) bool {
	if a.Address != b.Address || a.Hostname != b.Hostname || a.Weight != b.Weight {
		return false
	}
	if len(a.Metadata) != len(b.Metadata) {
		return false
	}
	for k, v := range a.Metadata {
		if b.Metadata[k] != v {
			return false
		}
	}
	return true
}

// HostSet manages a published, atomically-readable host map.
// Apply must be called on the cluster main thread.
// Get, Entry, and Current are safe from any goroutine.
type HostSet[K comparable] struct {
	handle  ClusterHandle
	current atomic.Pointer[map[K]HostEntry]
}

// NewHostSet creates a HostSet backed by handle. The initial published map is empty.
func NewHostSet[K comparable](handle ClusterHandle) *HostSet[K] {
	s := &HostSet[K]{handle: handle}
	empty := make(map[K]HostEntry)
	s.current.Store(&empty)
	return s
}

// Apply replaces the desired host snapshot. It must be called on the cluster
// main thread — it calls AddHosts, UpdateHostHealth, and RemoveHosts, all of
// which are main-thread-only. From a background goroutine, build the snapshot
// off-thread and use ClusterHandle.Schedule to invoke Apply on the main thread.
//
// Ordering guarantee: add → publish → remove so ChooseHost never sees a
// dangling pointer.
func (s *HostSet[K]) Apply(snapshot HostSnapshot[K]) {
	prev := *s.current.Load()

	// Build the new entry map, reusing HostPtrs for unchanged specs.
	var (
		newSpecs    []HostSpec // specs that need a new HostPtr
		newKeys     []K        // keys corresponding to newSpecs
		nextEntries = make(map[K]HostEntry, len(snapshot))
	)

	for k, spec := range snapshot {
		if entry, ok := prev[k]; ok && specEqual(entry.Spec, spec) {
			// Unchanged: reuse existing HostPtr.
			nextEntries[k] = entry
		} else {
			// New or changed: need AddHosts.
			newKeys = append(newKeys, k)
			newSpecs = append(newSpecs, spec)
		}
	}

	// Step 1: AddHosts for new/changed keys, mark them healthy.
	if len(newSpecs) > 0 {
		ptrs := s.handle.AddHosts(newSpecs)
		for i, k := range newKeys {
			nextEntries[k] = HostEntry{Spec: newSpecs[i], Host: ptrs[i]}
			s.handle.UpdateHostHealth(ptrs[i], HostHealthy)
		}
	}

	// Step 2: Publish atomically.
	s.current.Store(&nextEntries)

	// Step 3: Remove HostPtrs no longer in the published map.
	var toRemove []HostPtr
	for k, entry := range prev {
		if _, kept := nextEntries[k]; !kept {
			toRemove = append(toRemove, entry.Host)
		} else if kept := nextEntries[k]; kept.Host != entry.Host {
			// Key still present but spec changed — old HostPtr must be removed.
			toRemove = append(toRemove, entry.Host)
		}
	}
	if len(toRemove) > 0 {
		s.handle.RemoveHosts(toRemove)
	}
}

// Get returns the live HostPtr for key. Safe from any goroutine.
func (s *HostSet[K]) Get(key K) (HostPtr, bool) {
	m := *s.current.Load()
	e, ok := m[key]
	return e.Host, ok
}

// Entry returns the full HostEntry for key. Safe from any goroutine.
func (s *HostSet[K]) Entry(key K) (HostEntry, bool) {
	m := *s.current.Load()
	e, ok := m[key]
	return e, ok
}

// Current returns a freshly-copied snapshot of the published map.
// Safe from any goroutine. Callers that only need one key should prefer Get or Entry.
func (s *HostSet[K]) Current() map[K]HostEntry {
	m := *s.current.Load()
	return maps.Clone(m)
}
