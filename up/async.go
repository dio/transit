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
type HTTPCalloutResponse struct {
	Result  HTTPCalloutResult
	Headers [][2]Buffer
	Body    []Buffer
}

// HTTPCalloutFunc is invoked when an Envoy HTTP callout completes.
type HTTPCalloutFunc func(result HTTPCalloutResult, headers [][2]Buffer, body []Buffer)

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

// asyncState holds only the coordination state needed for Go+Do mode.
// Mutation queues live on Writer; asyncState has no mutex.
type asyncState struct {
	scheduler shared.Scheduler
	cancel    context.CancelFunc
	completed atomic.Bool
}

func (s *asyncState) finish(w *Writer) {
	s.scheduler.Schedule(func() {
		if s.completed.Swap(true) {
			return
		}
		w.flush(true)
	})
}

func (s *asyncState) completeWithoutResume() {
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

// doCallbackFunc is used by Writer.Do for per-Do callbacks inside Go mode.
type doCallbackFunc func(calloutID uint64, result HTTPCalloutResult, headers [][2]Buffer, body []Buffer)

func (f doCallbackFunc) OnHttpCalloutDone(
	calloutID uint64,
	result shared.HttpCalloutResult,
	headers [][2]shared.UnsafeEnvoyBuffer,
	body []shared.UnsafeEnvoyBuffer,
) {
	f(calloutID, HTTPCalloutResult(result), newHeaderBuffers(headers), newBuffers(body))
}
