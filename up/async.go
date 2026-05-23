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
//     goScheduler.Schedule and applies queued mutations, then continues the
//     request. SendLocalResponse from this path is NOT reliable — Envoy ignores
//     it from scheduled callbacks. Use Go+Do only for work that forwards the
//     request.
//
// Both paths are mutex-free by design. See the calloutState comment in
// writer.go for the full concurrency model.
package up

import (
	"errors"
	"fmt"

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
