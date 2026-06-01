// Package up — stream_object.go: Primitive A typed per-stream object handoff.
//
// Why the drain lives in OnStreamComplete (for filters without
// WithOnStreamFinalized) / finalizedLogger.OnLog (for filters with it):
//
// OnStreamComplete fires unconditionally for every stream Envoy terminates —
// normal end-of-stream, client disconnect, idle/request timeout, foreign local
// reply, stream reset. This is exactly the "teardown matrix" described in
// docs/orange-token-correlation-risks.md. For filters that also use
// WithOnStreamFinalized, the access logger's OnLog fires AFTER OnStreamComplete,
// so drain is deferred to OnLog so finalized cleanup runs before the SDK
// removes stream-scoped objects. Drain order:
//
//	user OnStreamComplete → user OnStreamFinalized → dropBag
package up

import (
	"crypto/rand"
	"encoding/hex"

	"github.com/dio/transit/down"
)

// streamObjectIDKey is the reserved filter-state key that carries the per-stream
// nonce. Re-exported from down for use within the up package.
const streamObjectIDKey = down.StreamObjectIDKey

// mintStreamObjectNonce returns a 16-byte hex random nonce. Collisions are
// astronomically unlikely (2^128 space) but getOrCreateBag handles them.
func mintStreamObjectNonce() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

// getOrCreateBag returns the *down.StreamObjects bag for the stream backed by
// the given Writer. On first call it mints a nonce, writes it to filter state
// via the Writer (using the same queued-vs-direct path as SetFilterState so
// that ordering is preserved in all modes), and inserts a new bag into the
// process-wide registry. On subsequent calls it looks up the existing bag.
//
// Must be called on the worker thread (same constraint as SetFilterState).
func getOrCreateBag(w *Writer) *down.StreamObjects {
	f := w.f
	// Fast path: nonce already assigned for this stream.
	if f.streamObjectNonce != "" {
		if bag, ok := down.LookupStreamObjectBag(f.streamObjectNonce); ok {
			return bag
		}
	}

	// Slow path: mint nonce, insert bag, write nonce through the Writer so
	// the correct path (queued vs direct) is used automatically.
	nonce := mintStreamObjectNonce()
	bag, loaded := down.InsertStreamObjectBag(nonce)
	for loaded {
		// Astronomically unlikely collision — try again.
		nonce = mintStreamObjectNonce()
		bag, loaded = down.InsertStreamObjectBag(nonce)
	}

	// Record nonce on the filter so future calls take the fast path and
	// so OnStreamComplete knows which bag to drain.
	f.streamObjectNonce = nonce

	// Write the nonce to filter state via the Writer. This ensures:
	//   • In queued mode (production): goes through f.filterState queue,
	//     applied in the same flush() pass as other SetFilterState calls.
	//   • In directWrite mode (tests, NewWriter): applied immediately to the
	//     handle so FakeClusterLBContext.GetStreamObject can read it.
	w.SetFilterState(streamObjectIDKey, nonce)
	return bag
}

// lookupBag returns the *down.StreamObjects for nonce, or (nil, false) if the
// stream never called SetStreamObject or the bag has already been drained.
func lookupBag(nonce string) (*down.StreamObjects, bool) {
	return down.LookupStreamObjectBag(nonce)
}

// dropBag removes the bag entry for nonce from the process-wide map.
// Called by filter.OnStreamComplete or finalizedLogger.OnLog.
// Safe to call with an empty nonce (no-op).
func dropBag(nonce string) {
	down.DropStreamObjectBag(nonce)
}
