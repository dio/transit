// Package down — stream_object.go: process-wide bag registry for Primitive A.
//
// This file lives in down (not up) so that both the producer side
// (up.Writer.SetStreamObject) and the consumer side
// (ClusterLBContext.GetStreamObject via down/abi_impl) can reach the bag
// without creating an import cycle.
package down

import (
	"sync"
)

// StreamObjectIDKey is the reserved filter-state key that carries the per-stream
// nonce linking a stream to its bag entry in the process-wide registry.
const StreamObjectIDKey = "up.stream_object_id"

// StreamObjects is the per-stream typed-value bag. Each key→value pair is
// written by Writer.SetStreamObject and read by Writer.GetStreamObject or
// ClusterLBContext.GetStreamObject.
type StreamObjects struct {
	mu sync.Mutex
	m  map[string]any
}

func (b *StreamObjects) Set(key string, v any) {
	b.mu.Lock()
	b.m[key] = v
	b.mu.Unlock()
}

func (b *StreamObjects) Get(key string) (any, bool) {
	b.mu.Lock()
	v, ok := b.m[key]
	b.mu.Unlock()
	return v, ok
}

// Process-wide bag registry: nonce → *StreamObjects.
// Locking idiom matches streamFinalizedEntries in up/stream_finalized.go:
// a plain sync.Mutex over a map[string]*.
var (
	streamObjectMu   sync.Mutex
	streamObjectBags = map[string]*StreamObjects{}
)

// LookupStreamObjectBag returns the *StreamObjects for nonce, or (nil, false)
// if absent. Called by ClusterLBContext.GetStreamObject.
func LookupStreamObjectBag(nonce string) (*StreamObjects, bool) {
	if nonce == "" {
		return nil, false
	}
	streamObjectMu.Lock()
	bag, ok := streamObjectBags[nonce]
	streamObjectMu.Unlock()
	return bag, ok
}

// InsertStreamObjectBag inserts a new bag for nonce, returning the existing
// bag if nonce is already present (LoadOrStore semantics). Called by up when
// minting a fresh bag.
func InsertStreamObjectBag(nonce string) (*StreamObjects, bool) {
	streamObjectMu.Lock()
	if existing, ok := streamObjectBags[nonce]; ok {
		streamObjectMu.Unlock()
		return existing, true
	}
	bag := &StreamObjects{m: make(map[string]any)}
	streamObjectBags[nonce] = bag
	streamObjectMu.Unlock()
	return bag, false
}

// DropStreamObjectBag removes the bag entry for nonce. Called by
// filter.OnStreamComplete (or finalizedLogger.OnLog for filters using
// WithOnStreamFinalized) after all user callbacks have run.
// Safe to call with an empty nonce (no-op).
func DropStreamObjectBag(nonce string) {
	if nonce == "" {
		return
	}
	streamObjectMu.Lock()
	delete(streamObjectBags, nonce)
	streamObjectMu.Unlock()
}

// StreamObjectBagCount returns the current number of bags in the process-wide
// registry. Walks the map; intended for diagnostics / tests only.
func StreamObjectBagCount() int {
	streamObjectMu.Lock()
	n := len(streamObjectBags)
	streamObjectMu.Unlock()
	return n
}
