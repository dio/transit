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
// scoped to a single handler invocation: calloutCB and calloutStarted. All
// mutable stream state — mutation queues, callout fn/state, async state — lives
// on the backing filter, which is long-lived for the stream.
//
// A Writer must not be retained beyond the handler call. On the HTTPCallout
// path the same Writer is reused across the initial handler call and the callout
// callback; both run sequentially within one stream operation so this is safe.
//
// Ownership: moving mutable state from the ephemeral Writer to the long-lived
// filter changes the failure mode from "lost mutation" to "stale mutation
// replayed on a later callback." filter.flush resets all queues after applying
// them to prevent this.
type Writer struct {
	// f is the backing filter for this stream. All mutable state is on f. Never nil.
	f *filter

	// calloutCB is pre-set to the filter before the handler runs. Passed to
	// handle.HttpCallout so Envoy routes the response to filter.OnHttpCalloutDone.
	// Using the filter directly avoids a per-callout closure allocation.
	calloutCB shared.HttpCalloutCallback

	// calloutStarted is true once HTTPCallout has been accepted by Envoy.
	// It gates the Active→Paused/Done CAS transitions in pausePendingCallout and
	// flushCompletedCallout, and the Go-after-HTTPCallout panic.
	calloutStarted bool
}

// NewWriter wraps handle in a Writer backed by a minimal filter. For tests;
// production Writers are created by filter with calloutCB pre-set. Tests that
// need callout or async behavior should instantiate filter directly.
func NewWriter(h shared.HttpFilterHandle) *Writer { return &Writer{f: &filter{handle: h}} }

// Log emits a message via Envoy's logging mechanism at the given severity.
func (w *Writer) Log(level LogLevel, format string, args ...any) {
	w.f.handle.Log(shared.LogLevel(level), format, args...)
}

// SendLocalResponse queues an immediate client response. Only the first call
// takes effect; additional calls are silently ignored.
//
// The response is applied by flush() on the worker thread. After flush, the
// backing filter's stopped flag stays true so the caller can return Stop.
//
// NOTE: SendLocalResponse from inside w.Go is NOT reliable. Envoy only honours
// it from filter callbacks (OnRequestHeaders, OnHttpCalloutDone, etc.). Use
// HTTPCallout (callback form) if the filter needs to reject with a local response.
func (w *Writer) SendLocalResponse(status int, body []byte, headers ...[2]string) {
	if w.f.localReply == nil {
		w.f.localReply = &localResponse{status: uint32(status), headers: headers, body: body}
	}
	w.f.stopped = true
}

// GetAttributeString returns the string stream attribute for the given ID.
// The returned Buffer is a copy safe to hold after the handler returns.
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

// SetRequestHeader queues a request header set. Applied by flush() on the worker thread.
// For response-phase use, call SetResponseHeader instead.
func (w *Writer) SetRequestHeader(name, value string) {
	w.f.reqHeaders = append(w.f.reqHeaders, requestHeaderMutation{name: name, value: value})
}

// AddRequestHeader queues a request header add (multi-value, no existing values removed).
func (w *Writer) AddRequestHeader(name, value string) {
	w.f.reqHeaders = append(w.f.reqHeaders, requestHeaderMutation{name: name, value: value, add: true})
}

// RemoveRequestHeader queues removal of all values for the named request header.
func (w *Writer) RemoveRequestHeader(name string) {
	w.f.reqHeaders = append(w.f.reqHeaders, requestHeaderMutation{name: name, del: true})
}

// SetFilterState queues a per-stream filter state write.
// Downstream filters, access loggers, and upstream selection callbacks can read it.
func (w *Writer) SetFilterState(key, value string) {
	w.f.filterState = append(w.f.filterState, filterStateMutation{key: key, value: []byte(value)})
}

// SetUpstreamOverrideHost queues an upstream host override. Last call wins.
// Returns true (optimistic; actual Envoy acceptance is not known until flush).
func (w *Writer) SetUpstreamOverrideHost(host string, strict bool) bool {
	w.f.override = &upstreamOverrideMutation{host: host, strict: strict}
	return true
}

// SetResponseHeader sets a response header inline. Response-phase callbacks only.
// Not queued — applied immediately. Not visible to flush().
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

// IncrementCounter queues a counter increment. Applied in order at flush time.
// id must have been obtained from ConfigHandle.DefineCounter at config time.
func (w *Writer) IncrementCounter(id MetricID, delta uint64) {
	w.f.counters = append(w.f.counters, counterMutation{id: id, delta: delta})
}

// Go upgrades this request to asynchronous mode. fn runs in a new goroutine
// and may call w.Do to issue outbound HTTP callouts. After fn returns, Transit
// hops back to the Envoy worker thread, applies queued mutations, and calls
// ContinueRequest to resume the stream — unless the stream was cancelled while
// fn was running.
//
// Goroutine ownership: once Go is called, the goroutine is the sole writer to
// the backing filter's mutation queues until it returns. OnStreamComplete must
// not reset those queues while the goroutine may still be writing — it only
// touches the async cancellation/completion state.
//
// Panics if called twice or after HTTPCallout.
//
// SendLocalResponse from inside fn is NOT reliable — see type-level docs.
func (w *Writer) Go(fn func(ctx context.Context)) {
	if w.f.async != nil {
		panic("up: Go called twice on the same request")
	}
	if w.calloutStarted {
		panic("up: Go cannot be started after HTTPCallout")
	}
	// GetScheduler must be called on the worker thread, not from the goroutine.
	scheduler := w.f.handle.GetScheduler()
	state := &asyncState{scheduler: scheduler}
	ctx, cancel := context.WithCancel(context.Background())
	state.cancel = cancel
	w.f.async = state
	// Capture f before spawning; w may be collected after the goroutine starts.
	f := w.f
	go func() {
		defer cancel()
		fn(ctx)
		// If OnStreamComplete fired while fn was running, done() is true.
		// Do not call finish — the stream is dead.
		if state.done() {
			return
		}
		state.finish(func() { f.flush(true) })
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
	if w.calloutStarted || w.f.async != nil {
		panic("up: HTTPCallout cannot be started after Go or another HTTPCallout")
	}
	// Set calloutFn and calloutStarted BEFORE calling handle.HttpCallout.
	// handle.HttpCallout may invoke the callback synchronously. If we set these
	// fields after, the synchronous callback would fire with calloutFn == nil
	// and calloutStarted == false, breaking both the routing and the state machine.
	w.f.calloutFn = fn
	w.calloutStarted = true
	init, _ := w.f.handle.HttpCallout(
		req.Cluster,
		req.Headers,
		req.Body,
		req.TimeoutMillis,
		w.calloutCB, // pre-set to filter; routes to filter.OnHttpCalloutDone
	)
	calloutInit := HTTPCalloutInitResult(init)
	if calloutInit != HTTPCalloutInitSuccess {
		// Roll back: no callback will fire, leave Writer in a clean state.
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
// Multiple Do calls may be in flight concurrently. However, Writer mutation
// methods are NOT goroutine-safe; call them only after all fan-out goroutines
// have joined (e.g. after wg.Wait()).
//
// Panics if called outside a Go goroutine.
//
// Response buffers (Headers, Body) are Go-owned copies safe to use after Do returns.
func (w *Writer) Do(ctx context.Context, req HTTPCalloutRequest) (*HTTPCalloutResponse, error) {
	if w.f.async == nil {
		panic("up: Do called outside Go")
	}
	type result struct {
		resp *HTTPCalloutResponse
		err  error
	}
	// Buffered so the doCallbackFunc send never blocks if Do already returned
	// due to context cancellation.
	ch := make(chan result, 1)
	w.f.async.scheduler.Schedule(func() {
		if w.f.async.done() {
			ch <- result{err: context.Canceled}
			return
		}
		init, _ := w.f.handle.HttpCallout(
			req.Cluster,
			req.Headers,
			req.Body,
			req.TimeoutMillis,
			doCallbackFunc(func(_ uint64, r HTTPCalloutResult, headers [][2]shared.UnsafeEnvoyBuffer, body []shared.UnsafeEnvoyBuffer) {
				// Copy before sending: Envoy may free the memory as soon as
				// this callback returns, before the goroutine reads from ch.
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
