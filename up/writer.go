package up

import (
	"context"
	"errors"

	"github.com/envoyproxy/envoy/source/extensions/dynamic_modules/sdk/go/shared"
)

// calloutState tracks the handoff between OnRequestHeaders and OnHttpCalloutDone.
//
// Background: Envoy does not guarantee that OnHttpCalloutDone fires after
// OnRequestHeaders returns. HttpCallout can invoke the callback synchronously —
// before OnRequestHeaders has had a chance to return HeadersStatusStop. This
// means we have two possible orderings for every callout:
//
//	Normal (async) path — OnRequestHeaders returns Stop BEFORE callback fires:
//	  1. HTTPCallout() sets calloutStarted=true, state=Active.
//	  2. HttpCallout() returns; the callback has NOT fired yet.
//	  3. OnRequestHeaders calls pausePendingCallout → CAS Active→Paused succeeds.
//	  4. OnRequestHeaders returns HeadersStatusStop. Envoy pauses the stream.
//	  5. Later: OnHttpCalloutDone fires. CAS Paused→Flushed succeeds.
//	     flush(true) is called → ContinueRequest resumes the stream.
//
//	Early (synchronous) path — callback fires INSIDE HttpCallout:
//	  1. HTTPCallout() sets calloutStarted=true, state=Active.
//	  2. HttpCallout() invokes the callback synchronously before returning.
//	  3. OnHttpCalloutDone fires. CAS Paused→Flushed fails (state is Active).
//	     CAS Active→Done succeeds. Mutations are queued. Callback returns.
//	  4. HttpCallout() returns. HTTPCallout() returns.
//	  5. OnRequestHeaders calls pausePendingCallout → CAS Active→Paused fails
//	     (state is Done). pausePendingCallout returns false.
//	  6. OnRequestHeaders calls flushCompletedCallout → CAS Done→Flushed succeeds.
//	     flush(false) applies mutations. NO ContinueRequest — OnRequestHeaders
//	     returns HeadersStatusContinue itself. Stream was never paused.
//
// The four states form a one-way ratchet; transitions never go backwards.
// No mutex is needed because the two competing parties (OnRequestHeaders on the
// worker thread, and OnHttpCalloutDone which may race it) each attempt exactly
// one CAS and only one wins.
const (
	// calloutStateActive is the initial state: HTTPCallout has been called but
	// neither side has yet claimed ownership of the flush.
	calloutStateActive int32 = iota

	// calloutStatePaused means OnRequestHeaders won: it returned Stop and is
	// waiting for OnHttpCalloutDone to call flush(true) + ContinueRequest.
	calloutStatePaused

	// calloutStateDone means OnHttpCalloutDone won the early path: the callback
	// fired before OnRequestHeaders checked. OnRequestHeaders will detect this
	// and call flush(false) before returning Continue.
	calloutStateDone

	// calloutStateFlushed is the terminal state: mutations have been applied,
	// the stream has been resumed (or not, in the sync case). No further action.
	calloutStateFlushed
)

// Writer is a thin per-invocation view over filter. It carries only the state
// scoped to a single handler invocation: calloutCB, calloutStarted, and
// directWrite. All mutable stream state — mutation queues, callout fn/state,
// async state — lives on the backing filter, which is long-lived for the stream.
//
// A Writer must not be retained beyond the handler call. On the HTTPCallout
// path the same Writer is reused across the initial handler call and the callout
// callback; both run sequentially within one stream operation so this is safe.
//
// directWrite mode (set only by NewWriter):
//
// Filter-created Writers (all production paths) set directWrite=false; mutation
// methods queue unconditionally so flush() applies them on the worker thread at
// the right time. NewWriter sets directWrite=true so that mutation methods apply
// directly to the handle — this preserves the behaviour expected by tests and
// examples that call handler functions outside a filter lifecycle, where there is
// no enclosing flush() call.
type Writer struct {
	// f is the backing filter for this stream. All mutable stream state is on f.
	f *filter

	// calloutCB is pre-set to the filter before the handler runs. Passed to
	// handle.HttpCallout so Envoy routes the response to filter.OnHttpCalloutDone.
	// Using the filter directly avoids a per-callout closure allocation.
	calloutCB shared.HttpCalloutCallback

	// calloutStarted is true once HTTPCallout has been accepted by Envoy.
	// It gates the Active→Paused/Done CAS transitions and the Go-after-HTTPCallout
	// panic. Also forces queueing of mutations even in directWrite mode, because
	// mutations from the callout callback must be deferred to flush().
	calloutStarted bool

	// directWrite is true only for Writers created by NewWriter. It makes
	// request-mutation methods apply directly to the handle instead of queuing,
	// unless an async operation (callout or goroutine) is in flight.
	directWrite bool
}

// NewWriter wraps handle in a Writer that applies mutations directly (no queue).
// Intended for use in tests and examples that invoke handler functions outside a
// filter lifecycle. Tests that need callout or async lifecycle behavior should
// instantiate filter directly.
func NewWriter(h shared.HttpFilterHandle) *Writer {
	return &Writer{f: &filter{handle: h}, directWrite: true}
}

// queued reports whether mutation methods should enqueue rather than apply directly.
// True whenever the filter is inside an active callout or goroutine, OR when the
// Writer was created by the filter factory (not by NewWriter) — in that case flush()
// is always called at the right point in the lifecycle.
func (w *Writer) queued() bool {
	return !w.directWrite || w.f.goStarted || w.calloutStarted
}

// Log emits a message via Envoy's logging mechanism at the given severity.
func (w *Writer) Log(level LogLevel, format string, args ...any) {
	w.f.handle.Log(shared.LogLevel(level), format, args...)
}

// SendLocalResponse queues (or immediately sends) a client response.
// Only the first call takes effect; additional calls are silently ignored.
//
// NOTE: SendLocalResponse from inside w.Go is NOT reliable. Envoy only honours
// it from filter callbacks. Use HTTPCallout (callback form) if the filter needs
// to reject with a local response.
func (w *Writer) SendLocalResponse(status int, body []byte, headers ...[2]string) {
	if w.queued() {
		if w.f.localReply == nil {
			w.f.localReply = &localResponse{status: uint32(status), headers: headers, body: body}
		}
		w.f.stopped = true
		return
	}
	if w.f.stopped {
		return
	}
	w.f.handle.SendLocalResponse(uint32(status), headers, body, "")
	w.f.stopped = true
}

// GetAttributeString returns the string stream attribute for the given ID.
func (w *Writer) GetAttributeString(id AttributeID) (Buffer, bool) {
	v, ok := w.f.handle.GetAttributeString(shared.AttributeID(id))
	if !ok {
		return Buffer{}, false
	}
	return newBuffer(v), true
}

// GetAttributeNumber returns the numeric stream attribute for the given ID.
func (w *Writer) GetAttributeNumber(id AttributeID) (float64, bool) {
	return w.f.handle.GetAttributeNumber(shared.AttributeID(id))
}

// GetAttributeBool returns the boolean stream attribute for the given ID.
func (w *Writer) GetAttributeBool(id AttributeID) (bool, bool) {
	return w.f.handle.GetAttributeBool(shared.AttributeID(id))
}

// GetActiveSpan returns the active tracing span for the current stream.
func (w *Writer) GetActiveSpan() shared.Span {
	return w.f.handle.GetActiveSpan()
}

// SetRequestHeader queues (or immediately sets) a request header.
func (w *Writer) SetRequestHeader(name, value string) {
	if w.queued() {
		w.f.reqHeaders = append(w.f.reqHeaders, requestHeaderMutation{name: name, value: value})
		return
	}
	w.f.handle.RequestHeaders().Set(name, value)
}

// AddRequestHeader queues (or immediately adds) a request header (multi-value).
func (w *Writer) AddRequestHeader(name, value string) {
	if w.queued() {
		w.f.reqHeaders = append(w.f.reqHeaders, requestHeaderMutation{name: name, value: value, add: true})
		return
	}
	w.f.handle.RequestHeaders().Add(name, value)
}

// RemoveRequestHeader queues (or immediately removes) all values for the named header.
func (w *Writer) RemoveRequestHeader(name string) {
	if w.queued() {
		w.f.reqHeaders = append(w.f.reqHeaders, requestHeaderMutation{name: name, del: true})
		return
	}
	w.f.handle.RequestHeaders().Remove(name)
}

// SetFilterState queues (or immediately writes) a per-stream filter state value.
func (w *Writer) SetFilterState(key, value string) {
	if w.queued() {
		w.f.filterState = append(w.f.filterState, filterStateMutation{key: key, value: []byte(value)})
		return
	}
	w.f.handle.SetFilterState(key, []byte(value))
}

// SetUpstreamOverrideHost queues (or immediately sets) an upstream host override.
// Returns true when queued (optimistic). Returns the handle's result in direct-write mode.
func (w *Writer) SetUpstreamOverrideHost(host string, strict bool) bool {
	if w.queued() {
		w.f.override = &upstreamOverrideMutation{host: host, strict: strict}
		return true
	}
	return w.f.handle.SetUpstreamOverrideHost(host, strict)
}

// SetResponseHeader sets a response header inline. Response-phase callbacks only.
// Not queued — response mutations are always applied immediately.
func (w *Writer) SetResponseHeader(name, value string) {
	w.f.handle.ResponseHeaders().Set(name, value)
}

// AddResponseHeader adds a response header inline. Response-phase only; not queued.
func (w *Writer) AddResponseHeader(name, value string) {
	w.f.handle.ResponseHeaders().Add(name, value)
}

// RemoveResponseHeader removes a response header inline. Response-phase only; not queued.
func (w *Writer) RemoveResponseHeader(name string) {
	w.f.handle.ResponseHeaders().Remove(name)
}

// SetRequestBody marks data as the replacement for the request body buffer.
// Only effective in buffered mode (RegisterWithMutableBody).
func (w *Writer) SetRequestBody(data []byte) {
	w.f.requestBodyReplacement = data
	w.f.hasRequestBodyReplacement = true
}

// SetResponseBody marks data as the replacement for the response body buffer.
// Only effective in buffered mode (RegisterWithMutableBody).
func (w *Writer) SetResponseBody(data []byte) {
	w.f.responseBodyReplacement = data
	w.f.hasResponseBodyReplacement = true
}

// IncrementCounter queues (or immediately applies) a counter increment.
func (w *Writer) IncrementCounter(id MetricID, delta uint64) {
	if w.queued() {
		w.f.counters = append(w.f.counters, counterMutation{id: id, delta: delta})
		return
	}
	w.f.handle.IncrementCounterValue(shared.MetricID(id), delta)
}

// Go upgrades this request to asynchronous mode. fn runs in a new goroutine
// and may call w.Do to issue outbound HTTP callouts. After fn returns, Transit
// hops back to the Envoy worker thread, applies queued mutations, and resumes
// the stream — unless the stream was cancelled while fn was running.
//
// goStarted is cleared inside the scheduled finish before flush(true) so that
// subsequent callbacks (e.g. OnRequestBody on a request that still has a body)
// do not see stale goroutine state and spuriously return StopAndBuffer.
//
// Panics if called twice or after HTTPCallout.
//
// SendLocalResponse from inside fn is NOT reliable — see type-level docs.
func (w *Writer) Go(fn func(ctx context.Context)) {
	if w.f.goStarted {
		panic("up: Go called twice on the same request")
	}
	if w.calloutStarted {
		panic("up: Go cannot be started after HTTPCallout")
	}
	ctx, cancel := context.WithCancel(context.Background())
	f := w.f
	f.goScheduler = w.f.handle.GetScheduler()
	f.goCancel = cancel
	f.goCompleted.Store(false)
	f.goStarted = true
	go func() {
		defer cancel()
		fn(ctx)
		if f.goCompleted.Load() {
			// OnStreamComplete won the race; stream is dead. Skip scheduling.
			return
		}
		f.goScheduler.Schedule(func() {
			if f.goCompleted.Swap(true) {
				// OnStreamComplete won the race. Do not resume.
				return
			}
			// Clear goroutine active flag before flush so that callbacks arriving
			// after ContinueRequest (e.g. OnRequestBody) see goStarted=false.
			f.goStarted = false
			f.flush(true)
		})
	}()
}

// HTTPCallout initiates an outbound Envoy HTTP callout and pauses the request
// until the callout completes. fn is invoked with the callout result; it may
// queue mutations or call SendLocalResponse.
//
// Returns HTTPCalloutInitSuccess and nil error if Envoy accepted the callout.
// A non-nil error means fn will never be called.
//
// Panics if called after Go or after a previous HTTPCallout.
func (w *Writer) HTTPCallout(req HTTPCalloutRequest, fn HTTPCalloutFunc) (HTTPCalloutInitResult, error) {
	if w.calloutStarted || w.f.goStarted {
		panic("up: HTTPCallout cannot be started after Go or another HTTPCallout")
	}
	// Set calloutFn and calloutStarted BEFORE calling handle.HttpCallout.
	// handle.HttpCallout may invoke the callback synchronously. If we set these
	// fields after, the synchronous callback would fire with calloutFn == nil.
	w.f.calloutFn = fn
	w.calloutStarted = true
	init, _ := w.f.handle.HttpCallout(
		req.Cluster,
		req.Headers,
		req.Body,
		req.TimeoutMillis,
		w.calloutCB,
	)
	calloutInit := HTTPCalloutInitResult(init)
	if calloutInit != HTTPCalloutInitSuccess {
		w.f.calloutFn = nil
		w.calloutStarted = false
		w.f.calloutState.Store(calloutStateActive)
		return calloutInit, errCalloutInitResult(calloutInit)
	}
	return HTTPCalloutInitSuccess, nil
}

// Do performs an Envoy HTTP callout from inside a Go goroutine and blocks
// until the callout completes or ctx is cancelled.
//
// Multiple Do calls may be in flight concurrently. Writer mutation methods are
// NOT goroutine-safe; call them only after all fan-out goroutines have joined.
//
// Panics if called outside a Go goroutine.
//
// Response buffers (Headers, Body) are Go-owned copies safe to use after Do returns.
func (w *Writer) Do(ctx context.Context, req HTTPCalloutRequest) (*HTTPCalloutResponse, error) {
	if !w.f.goStarted {
		panic("up: Do called outside Go")
	}
	type result struct {
		resp *HTTPCalloutResponse
		err  error
	}
	ch := make(chan result, 1)
	w.f.goScheduler.Schedule(func() {
		if w.f.goCompleted.Load() {
			ch <- result{err: context.Canceled}
			return
		}
		init, _ := w.f.handle.HttpCallout(
			req.Cluster,
			req.Headers,
			req.Body,
			req.TimeoutMillis,
			doCallbackFunc(func(_ uint64, r HTTPCalloutResult, headers [][2]shared.UnsafeEnvoyBuffer, body []shared.UnsafeEnvoyBuffer) {
				ch <- result{resp: &HTTPCalloutResponse{
					Result:  r,
					Headers: copyUnsafeEnvoyHeaderBuffers(headers),
					Body:    copyUnsafeEnvoyBuffers(body),
				}}
			}),
		)
		if calloutInit := HTTPCalloutInitResult(init); calloutInit != HTTPCalloutInitSuccess {
			ch <- result{err: errCalloutInitResult(calloutInit)}
		}
	})
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case res := <-ch:
		if res.err != nil {
			return nil, res.err
		}
		if res.resp == nil {
			return nil, errors.New("up: HTTP callout completed without response")
		}
		return res.resp, nil
	}
}
