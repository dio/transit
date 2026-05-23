package up

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"

	"github.com/envoyproxy/envoy/source/extensions/dynamic_modules/sdk/go/shared"
)

// HTTPCalloutRequest carries the parameters for an outbound Envoy HTTP callout.
type HTTPCalloutRequest struct {
	Cluster       string
	Headers       [][2]string
	Body          []byte
	TimeoutMillis uint64
}

// HTTPCalloutInitResult reports whether Envoy accepted an HTTP callout.
type HTTPCalloutInitResult uint32

const (
	HTTPCalloutInitSuccess                HTTPCalloutInitResult = HTTPCalloutInitResult(shared.HttpCalloutInitSuccess)
	HTTPCalloutInitMissingRequiredHeaders HTTPCalloutInitResult = HTTPCalloutInitResult(shared.HttpCalloutInitMissingRequiredHeaders)
	HTTPCalloutInitClusterNotFound        HTTPCalloutInitResult = HTTPCalloutInitResult(shared.HttpCalloutInitClusterNotFound)
	HTTPCalloutInitDuplicateCalloutID     HTTPCalloutInitResult = HTTPCalloutInitResult(shared.HttpCalloutInitDuplicateCalloutId)
	HTTPCalloutInitCannotCreateRequest    HTTPCalloutInitResult = HTTPCalloutInitResult(shared.HttpCalloutInitCannotCreateRequest)
)

// HTTPCalloutResult reports the terminal result of an accepted HTTP callout.
type HTTPCalloutResult uint32

const (
	HTTPCalloutSuccess                   HTTPCalloutResult = HTTPCalloutResult(shared.HttpCalloutSuccess)
	HTTPCalloutReset                     HTTPCalloutResult = HTTPCalloutResult(shared.HttpCalloutReset)
	HTTPCalloutExceedResponseBufferLimit HTTPCalloutResult = HTTPCalloutResult(shared.HttpCalloutExceedResponseBufferLimit)
)

// HTTPCalloutResponse is the result returned by Writer.Do.
// Headers and Body are Go-owned copies, safe to use after Do returns.
type HTTPCalloutResponse struct {
	Result  HTTPCalloutResult
	Headers [][2]shared.UnsafeEnvoyBuffer
	Body    []shared.UnsafeEnvoyBuffer
}

// HTTPCalloutFunc is invoked when an Envoy HTTP callout completes.
// headers and body point into Envoy-owned memory valid only for the duration
// of the callback; copy any value that must outlive the call.
type HTTPCalloutFunc func(result HTTPCalloutResult, headers [][2]shared.UnsafeEnvoyBuffer, body []shared.UnsafeEnvoyBuffer)

type requestHeaderMutation struct {
	name  string
	value string
	del   bool
	add   bool
}

type localResponse struct {
	status  uint32
	headers [][2]string
	body    []byte
}

type filterStateMutation struct {
	key   string
	value []byte
}

type counterMutation struct {
	id    MetricID
	delta uint64
}

type upstreamOverrideMutation struct {
	host   string
	strict bool
}

// asyncState holds coordination state for Go+Do mode only.
//
// Why no handle, no mutex, no mutation queues: the Go+Do path has a
// single-writer discipline. The goroutine is the only writer to Writer's
// mutation slices until it exits; after that, scheduler.Schedule hops back
// to the Envoy worker thread and flush runs with no concurrent writers.
// An atomic.Bool is sufficient to resolve the one real race: goroutine
// finishing vs. OnStreamComplete cancelling.
type asyncState struct {
	scheduler shared.Scheduler
	cancel    context.CancelFunc
	completed atomic.Bool
}

func (s *asyncState) finish(w *Writer) {
	// Schedule hops back to the Envoy worker thread. flush must run there
	// because ContinueRequest and SendLocalResponse are CGO calls that Envoy
	// expects on the stream's worker thread.
	s.scheduler.Schedule(func() {
		if s.completed.Swap(true) {
			return
		}
		w.flush(true)
	})
}

func (s *asyncState) completeWithoutResume() {
	// Called by OnStreamComplete when the stream ends while the goroutine is
	// still running. Setting completed prevents finish from resuming a dead
	// stream; cancel unblocks the goroutine's ctx.Done() so it exits promptly.
	s.completed.Store(true)
	if s.cancel != nil {
		s.cancel()
	}
}

func (s *asyncState) done() bool {
	return s.completed.Load()
}

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
// It is distinct from filter.OnHttpCalloutDone (which handles HTTPCallout) so
// that multiple Do calls can be in flight simultaneously — each gets its own
// doCallbackFunc registered with Envoy for its specific callout ID.
type doCallbackFunc func(calloutID uint64, result HTTPCalloutResult, headers [][2]shared.UnsafeEnvoyBuffer, body []shared.UnsafeEnvoyBuffer)

func (f doCallbackFunc) OnHttpCalloutDone(
	calloutID uint64,
	result shared.HttpCalloutResult,
	headers [][2]shared.UnsafeEnvoyBuffer,
	body []shared.UnsafeEnvoyBuffer,
) {
	f(calloutID, HTTPCalloutResult(result), headers, body)
}

func copyUnsafeEnvoyBuffer(raw shared.UnsafeEnvoyBuffer) shared.UnsafeEnvoyBuffer {
	owned := raw.ToBytes()
	if len(owned) == 0 {
		return shared.UnsafeEnvoyBuffer{}
	}
	return shared.UnsafeEnvoyBuffer{Ptr: &owned[0], Len: uint64(len(owned))}
}

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
