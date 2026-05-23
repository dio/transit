// Package up provides the user-facing API for transit HTTP filter handlers.
// This file defines all types related to asynchronous HTTP callouts and the
// two async modes a handler can use:
//
//  1. HTTPCallout — callback form. The filter stops the request, Envoy sends
//     an outbound HTTP request to a named cluster, and the user-supplied
//     HTTPCalloutFunc runs when the response arrives. The func may queue
//     mutations (SetRequestHeader, etc.) or send a local response. Transit
//     then applies those mutations and resumes (or terminates) the stream.
//     This is the only path that supports SendLocalResponse reliably, because
//     the callback runs from a filter callback (OnHttpCalloutDone), not from
//     a scheduler, and Envoy only honours SendLocalResponse from filter callbacks.
//
//  2. Go + Do — goroutine form. The handler calls w.Go(fn); fn runs in a
//     goroutine and may call w.Do(...) to issue callouts from that goroutine.
//     After fn returns, Transit hops back to the Envoy worker thread via
//     scheduler.Schedule and applies queued mutations, then continues the
//     request. SendLocalResponse from this path is NOT reliable — Envoy ignores
//     it from scheduled callbacks. Use Go+Do only for work that forwards the
//     request.
//
// Both paths are mutex-free by design. See the calloutState comment in
// writer.go for the full concurrency model.
package up

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"

	"github.com/envoyproxy/envoy/source/extensions/dynamic_modules/sdk/go/shared"
)

// HTTPCalloutRequest carries the parameters for an outbound Envoy HTTP callout.
// Cluster must name a cluster defined in the Envoy bootstrap config; if it
// does not exist, HTTPCallout returns HTTPCalloutInitClusterNotFound.
// Headers must include at minimum :method, :path, and host. Include :scheme
// when the upstream or route logic needs it.
// TimeoutMillis of 0 uses Envoy's default callout timeout.
type HTTPCalloutRequest struct {
	Cluster       string
	Headers       [][2]string
	Body          []byte
	TimeoutMillis uint64
}

// HTTPCalloutInitResult reports whether Envoy accepted the callout request.
// A non-Success result means no callout was initiated and no callback will fire;
// the handler should treat it as an immediate error.
type HTTPCalloutInitResult uint32

const (
	HTTPCalloutInitSuccess                HTTPCalloutInitResult = HTTPCalloutInitResult(shared.HttpCalloutInitSuccess)
	HTTPCalloutInitMissingRequiredHeaders HTTPCalloutInitResult = HTTPCalloutInitResult(shared.HttpCalloutInitMissingRequiredHeaders)
	HTTPCalloutInitClusterNotFound        HTTPCalloutInitResult = HTTPCalloutInitResult(shared.HttpCalloutInitClusterNotFound)
	HTTPCalloutInitDuplicateCalloutID     HTTPCalloutInitResult = HTTPCalloutInitResult(shared.HttpCalloutInitDuplicateCalloutId)
	HTTPCalloutInitCannotCreateRequest    HTTPCalloutInitResult = HTTPCalloutInitResult(shared.HttpCalloutInitCannotCreateRequest)
)

// HTTPCalloutResult is the terminal outcome of an accepted callout, delivered
// to HTTPCalloutFunc or via HTTPCalloutResponse.Result from Writer.Do.
type HTTPCalloutResult uint32

const (
	// HTTPCalloutSuccess means the upstream responded; headers and body are valid.
	HTTPCalloutSuccess HTTPCalloutResult = HTTPCalloutResult(shared.HttpCalloutSuccess)

	// HTTPCalloutReset means the connection was reset before a response arrived.
	// headers and body will be empty.
	HTTPCalloutReset HTTPCalloutResult = HTTPCalloutResult(shared.HttpCalloutReset)

	// HTTPCalloutExceedResponseBufferLimit means the response body was larger
	// than Envoy's configured callout buffer limit. headers may be present;
	// body will be truncated or empty.
	HTTPCalloutExceedResponseBufferLimit HTTPCalloutResult = HTTPCalloutResult(shared.HttpCalloutExceedResponseBufferLimit)
)

// HTTPCalloutResponse is the value returned by Writer.Do.
//
// Headers and Body are Go-owned copies made inside the callout callback before
// the response is sent across the goroutine channel. They are safe to read after
// Do returns, even though the underlying Envoy memory may have been freed.
//
// For Writer.HTTPCallout (callback form), the buffers are NOT copied — they are
// passed directly to HTTPCalloutFunc and are only valid during that call.
type HTTPCalloutResponse struct {
	Result  HTTPCalloutResult
	Headers [][2]shared.UnsafeEnvoyBuffer
	Body    []shared.UnsafeEnvoyBuffer
}

// HTTPCalloutFunc is the callback invoked when an Envoy HTTP callout completes.
//
// LIFETIME: headers and body point into Envoy-owned memory that is only valid
// for the duration of this call. Copy any value that must outlive the callback.
// Specifically: do NOT send these slices to another goroutine or store them in a
// struct that outlives the function — Envoy may reuse or free that memory as soon
// as the callback returns.
//
// The function runs on the Envoy worker thread (or the goroutine that called
// HttpCallout synchronously). It may call any Writer mutation method; those
// mutations are queued and applied by flush() after the callback returns.
type HTTPCalloutFunc func(result HTTPCalloutResult, headers [][2]shared.UnsafeEnvoyBuffer, body []shared.UnsafeEnvoyBuffer)

// requestHeaderMutation is a deferred request header operation.
// del takes priority; add uses HeaderMap.Add (multi-value); default uses Set.
type requestHeaderMutation struct {
	name  string
	value string
	del   bool
	add   bool
}

// localResponse is a deferred SendLocalResponse payload.
// Only the first call to SendLocalResponse in async mode takes effect;
// subsequent calls are silently dropped (localReply is only set once).
type localResponse struct {
	status  uint32
	headers [][2]string
	body    []byte
}

// filterStateMutation is a deferred SetFilterState operation.
// Filter state is visible to downstream filters, access loggers, and
// upstream selection callbacks (e.g. LB Policy, Cluster Extension).
type filterStateMutation struct {
	key   string
	value []byte
}

// counterMutation is a deferred IncrementCounter operation.
// Multiple increments to the same MetricID are applied in order; they are
// not coalesced.
type counterMutation struct {
	id    MetricID
	delta uint64
}

// upstreamOverrideMutation is a deferred SetUpstreamOverrideHost operation.
// Only one override is kept; the last call wins (pointer is overwritten).
type upstreamOverrideMutation struct {
	host   string
	strict bool
}

// asyncState holds coordination state for the Go+Do path only.
//
// Why no handle, no mutex, no mutation queues:
//
// The Go+Do path has single-writer discipline. The goroutine started by
// Writer.Go is the sole writer to Writer's mutation slices from the moment Go
// returns until the goroutine exits. No other goroutine or Envoy callback
// touches those slices during that window. After the goroutine exits,
// scheduler.Schedule hops back to the Envoy worker thread, where flush()
// runs as the sole reader — again, no concurrent access.
//
// The only genuine race is between the goroutine finishing normally
// (asyncState.finish) and OnStreamComplete cancelling the stream before the
// goroutine exits (asyncState.completeWithoutResume). A single atomic.Bool
// is sufficient to resolve that race: whichever side wins the Swap(true) owns
// the terminal action; the other side is a no-op.
//
// Why no handle: the Envoy handle is stored on filter and accessed by Writer
// via w.handle. asyncState does not need it; keeping it out avoids accidental
// CGO calls from the wrong thread.
type asyncState struct {
	// scheduler is used to hop back to the Envoy worker thread after the
	// goroutine exits. It is acquired once in Writer.Go and stored here so
	// that Do (which may call Schedule from a different goroutine) uses the
	// same scheduler instance as finish.
	scheduler shared.Scheduler

	// cancel is the context.CancelFunc for the goroutine's context. Calling it
	// unblocks ctx.Done() inside the goroutine so it can exit promptly when the
	// stream is cancelled. It is also deferred by the goroutine itself so that
	// resources are cleaned up even on normal exit.
	cancel context.CancelFunc

	// completed is set to true exactly once, by whichever of finish or
	// completeWithoutResume wins the race. The loser sees Swap return true and
	// does nothing.
	completed atomic.Bool
}

// finish is called by the goroutine after fn(ctx) returns normally. It
// schedules resume on the Envoy worker thread so that queued mutations are
// applied and ContinueRequest is called to resume the stream.
//
// resume is a closure (func() { f.flush(true) }) rather than a *Writer so
// that asyncState has no dependency on filter's concrete type.
//
// Why Schedule and not a direct call: flush() calls CGO functions
// (ContinueRequest, SendLocalResponse) that Envoy requires to be invoked on
// the stream's worker thread. The goroutine runs on an arbitrary Go OS thread,
// which is not the worker thread. Schedule posts a task to Envoy's event loop
// so flush runs in the correct context.
func (s *asyncState) finish(resume func()) {
	s.scheduler.Schedule(func() {
		// completed.Swap returns the old value. If it was already true,
		// completeWithoutResume won the race: the stream is dead, do not resume.
		if s.completed.Swap(true) {
			return
		}
		resume()
	})
}

// completeWithoutResume is called by OnStreamComplete when Envoy terminates
// the stream while a Go goroutine is still running. It prevents finish from
// resuming a stream that no longer exists, and cancels the goroutine's context
// so it exits promptly rather than blocking indefinitely on Do or other ops.
//
// Note: cancel() unblocks ctx.Done() but does NOT wait for the goroutine to
// actually return. The goroutine may still be running after this call. For
// Phase 4 (filter pooling), this means the filter must not be reused until
// the goroutine has actually exited — not just until cancel() has been called.
func (s *asyncState) completeWithoutResume() {
	// Store(true) instead of Swap: we do not need to check the old value here
	// because completeWithoutResume never calls flush regardless.
	s.completed.Store(true)
	if s.cancel != nil {
		s.cancel()
	}
}

// done returns true if the stream has been completed without resume, meaning
// any pending scheduled flush should be skipped.
func (s *asyncState) done() bool {
	return s.completed.Load()
}

// errCalloutInitResult converts a non-success HTTPCalloutInitResult to an error.
// Returns nil for HTTPCalloutInitSuccess.
func errCalloutInitResult(r HTTPCalloutInitResult) error {
	switch r {
	case HTTPCalloutInitSuccess:
		return nil
	case HTTPCalloutInitMissingRequiredHeaders:
		return errors.New("up: HTTP callout: missing required headers")
	case HTTPCalloutInitClusterNotFound:
		return errors.New("up: HTTP callout: cluster not found")
	case HTTPCalloutInitDuplicateCalloutID:
		return errors.New("up: HTTP callout: duplicate callout ID")
	case HTTPCalloutInitCannotCreateRequest:
		return errors.New("up: HTTP callout: cannot create request")
	default:
		return fmt.Errorf("up: HTTP callout: unknown init result %d", r)
	}
}

// doCallbackFunc is the per-callout callback used by Writer.Do inside Go mode.
//
// Why separate from filter.OnHttpCalloutDone (which handles HTTPCallout):
// Writer.Do may be called multiple times concurrently from fan-out goroutines
// inside a single w.Go(fn). Each Do call issues its own HttpCallout with a
// unique callout ID, and needs its own callback to route the response back to
// the correct blocking goroutine via a dedicated channel. filter.OnHttpCalloutDone
// is registered once for the whole filter and handles only the single HTTPCallout
// path, which does not support concurrent in-flight callouts.
//
// Why a function type implementing shared.HttpCalloutCallback: this lets each
// Do invocation register a fresh closure capturing its own response channel,
// without allocating a named struct per callout.
type doCallbackFunc func(calloutID uint64, result HTTPCalloutResult, headers [][2]shared.UnsafeEnvoyBuffer, body []shared.UnsafeEnvoyBuffer)

func (f doCallbackFunc) OnHttpCalloutDone(
	calloutID uint64,
	result shared.HttpCalloutResult,
	headers [][2]shared.UnsafeEnvoyBuffer,
	body []shared.UnsafeEnvoyBuffer,
) {
	// Pass through directly. Copying is done by the caller (Writer.Do) before
	// sending the response across the channel, not here, so that the copy logic
	// is in one place and this adapter stays trivial.
	f(calloutID, HTTPCalloutResult(result), headers, body)
}

// copyUnsafeEnvoyBuffer returns a Go-owned copy of a single Envoy buffer.
// The copy is safe to hold after the enclosing Envoy callback returns.
// An empty buffer (Len==0 or Ptr==nil) produces a zero UnsafeEnvoyBuffer.
func copyUnsafeEnvoyBuffer(raw shared.UnsafeEnvoyBuffer) shared.UnsafeEnvoyBuffer {
	owned := raw.ToBytes()
	if len(owned) == 0 {
		return shared.UnsafeEnvoyBuffer{}
	}
	return shared.UnsafeEnvoyBuffer{Ptr: &owned[0], Len: uint64(len(owned))}
}

// copyUnsafeEnvoyBuffers copies a slice of Envoy body buffers into Go memory.
// Returns nil if raw is nil (preserving the nil/empty distinction for callers).
func copyUnsafeEnvoyBuffers(raw []shared.UnsafeEnvoyBuffer) []shared.UnsafeEnvoyBuffer {
	if raw == nil {
		return nil
	}
	out := make([]shared.UnsafeEnvoyBuffer, len(raw))
	for i, b := range raw {
		out[i] = copyUnsafeEnvoyBuffer(b)
	}
	return out
}

// copyUnsafeEnvoyHeaderBuffers copies a slice of [name, value] Envoy header
// buffer pairs into Go memory. Returns nil if raw is nil.
func copyUnsafeEnvoyHeaderBuffers(raw [][2]shared.UnsafeEnvoyBuffer) [][2]shared.UnsafeEnvoyBuffer {
	if raw == nil {
		return nil
	}
	out := make([][2]shared.UnsafeEnvoyBuffer, len(raw))
	for i, h := range raw {
		out[i] = [2]shared.UnsafeEnvoyBuffer{
			copyUnsafeEnvoyBuffer(h[0]),
			copyUnsafeEnvoyBuffer(h[1]),
		}
	}
	return out
}
