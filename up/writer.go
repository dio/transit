package up

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"

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

// Writer provides actions the handler can take on the current request.
// A Writer is created fresh for each Envoy filter callback (OnRequestHeaders,
// OnRequestBody, etc.) and must not be retained beyond the handler call.
// On the HTTPCallout path, the same Writer is reused across the initial handler
// call and the callout callback — this is safe because both run on the same
// conceptual unit of work (the in-flight callout) and mutations are queued.
type Writer struct {
	// handle is the Envoy filter handle for this stream. All CGO calls go
	// through it. It is never nil for a live stream.
	handle shared.HttpFilterHandle

	// stopped is true after SendLocalResponse has been called. It signals
	// OnRequestHeaders to return HeadersStatusStop rather than Continue.
	stopped bool

	// async is non-nil when the handler called w.Go(). It holds the scheduler
	// and the completed flag for the goroutine lifecycle.
	async *asyncState

	// calloutCB is pre-set to the filter before the handler runs. When
	// HTTPCallout calls handle.HttpCallout, it passes calloutCB as the callback
	// so that Envoy knows to call filter.OnHttpCalloutDone when done. Using the
	// filter directly (rather than allocating a closure per callout) means zero
	// extra allocation on the HTTPCallout path.
	calloutCB shared.HttpCalloutCallback

	// calloutFn is the user-supplied callback set by HTTPCallout. It is read
	// and cleared by OnHttpCalloutDone before being invoked, so a second
	// spurious OnHttpCalloutDone (e.g. after stream complete) is a safe no-op.
	calloutFn HTTPCalloutFunc

	// calloutStarted is true once HTTPCallout has been accepted by Envoy. It
	// is set BEFORE calling handle.HttpCallout so that mutation methods called
	// from a synchronous callback (which fires inside HttpCallout) see it and
	// enqueue to the mutation queues rather than applying directly. Cleared on
	// init failure so the Writer is left in a clean state.
	calloutStarted bool

	// calloutState is the atomic handoff between OnRequestHeaders and
	// OnHttpCalloutDone. See the const block above for the full state machine.
	calloutState atomic.Int32

	// Mutation queues accumulate changes requested by the handler during an
	// async operation (callout or Go goroutine). They are applied by flush()
	// on the Envoy worker thread — never inline — so that CGO calls
	// (RequestHeaders().Set, SendLocalResponse, etc.) are never made while any
	// lock is held, eliminating the lock-while-CGO deadlock class entirely.
	//
	// On the HTTPCallout path: the handler and the callout callback both run
	// "sequentially" relative to the stream (no true concurrency), so these
	// queues are written without synchronisation.
	//
	// On the Go+Do path: the goroutine is the sole writer until it exits, then
	// the worker thread is the sole reader via flush(). No concurrent access.
	reqHeaders  []requestHeaderMutation
	filterState []filterStateMutation
	counters    []counterMutation
	override    *upstreamOverrideMutation
	localReply  *localResponse

	// body replacement — set via SetRequestBody/SetResponseBody in buffered
	// mode (RegisterWithMutableBody). Applied inline, not via flush, because
	// body replacement modifies the Envoy buffer object directly and must
	// happen during the body callback.
	requestBodyReplacement     []byte
	hasRequestBodyReplacement  bool
	responseBodyReplacement    []byte
	hasResponseBodyReplacement bool
}

// NewWriter wraps handle in a Writer. Intended for use in tests; production
// Writers are created by the filter and have calloutCB pre-set.
func NewWriter(h shared.HttpFilterHandle) *Writer { return &Writer{handle: h} }

// Log emits a message via Envoy's logging mechanism at the given severity.
func (w *Writer) Log(level LogLevel, format string, args ...any) {
	w.handle.Log(shared.LogLevel(level), format, args...)
}

// SendLocalResponse sends an immediate response to the client and stops filter
// chain processing. Subsequent filters in the chain are not invoked.
//
// In async mode (inside an HTTPCallout callback or a Go goroutine), the
// response is queued as a localReply and applied by flush(). Only the first
// call takes effect; additional calls are silently ignored so that multiple
// code paths can call SendLocalResponse defensively without double-sending.
//
// Optional headers are added to the response, e.g.:
//
//	w.SendLocalResponse(403, body, [2]string{"content-type", "application/json"})
func (w *Writer) SendLocalResponse(status int, body []byte, headers ...[2]string) {
	if w.calloutStarted || w.async != nil {
		// Async path: queue for flush(). localReply is only set once (first call wins).
		if w.localReply == nil {
			w.localReply = &localResponse{status: uint32(status), headers: headers, body: body}
		}
		w.stopped = true
		return
	}
	// Synchronous path: call directly. Safe because we are on the worker thread
	// with no lock held.
	w.handle.SendLocalResponse(uint32(status), headers, body, "")
	w.stopped = true
}

// GetAttributeString returns the string stream attribute for the given ID.
// The returned Buffer is a copy; it is safe to hold after the handler returns.
func (w *Writer) GetAttributeString(id AttributeID) (Buffer, bool) {
	v, ok := w.handle.GetAttributeString(shared.AttributeID(id))
	if !ok {
		return Buffer{}, false
	}
	return newBuffer(v), true
}

// GetAttributeNumber returns the numeric stream attribute for the given ID.
func (w *Writer) GetAttributeNumber(id AttributeID) (float64, bool) {
	return w.handle.GetAttributeNumber(shared.AttributeID(id))
}

// GetAttributeBool returns the boolean stream attribute for the given ID.
func (w *Writer) GetAttributeBool(id AttributeID) (bool, bool) {
	return w.handle.GetAttributeBool(shared.AttributeID(id))
}

// GetActiveSpan returns the active tracing span for the current stream.
// The span is valid for the duration of the stream.
func (w *Writer) GetActiveSpan() shared.Span {
	return w.handle.GetActiveSpan()
}

// SetRequestHeader sets a request header, replacing any existing value.
// In async mode the change is queued and applied by flush(); in synchronous
// mode it is applied immediately via the Envoy handle.
func (w *Writer) SetRequestHeader(name, value string) {
	if w.calloutStarted || w.async != nil {
		w.reqHeaders = append(w.reqHeaders, requestHeaderMutation{name: name, value: value})
		return
	}
	w.handle.RequestHeaders().Set(name, value)
}

// AddRequestHeader adds a request header without removing existing values.
// Use this when the header can appear multiple times (e.g. Set-Cookie).
func (w *Writer) AddRequestHeader(name, value string) {
	if w.calloutStarted || w.async != nil {
		w.reqHeaders = append(w.reqHeaders, requestHeaderMutation{name: name, value: value, add: true})
		return
	}
	w.handle.RequestHeaders().Add(name, value)
}

// RemoveRequestHeader removes all values for the named request header.
func (w *Writer) RemoveRequestHeader(name string) {
	if w.calloutStarted || w.async != nil {
		w.reqHeaders = append(w.reqHeaders, requestHeaderMutation{name: name, del: true})
		return
	}
	w.handle.RequestHeaders().Remove(name)
}

// SetFilterState stores a raw per-stream value under key. Downstream filters,
// access loggers, and upstream selection callbacks (LB Policy, Cluster
// Extension) can read it. In async mode the write is queued; last write to
// the same key wins at flush time.
func (w *Writer) SetFilterState(key, value string) {
	if w.calloutStarted || w.async != nil {
		w.filterState = append(w.filterState, filterStateMutation{key: key, value: []byte(value)})
		return
	}
	w.handle.SetFilterState(key, []byte(value))
}

// SetUpstreamOverrideHost asks Envoy's load balancer to prefer host when
// selecting an upstream endpoint. If strict is true, Envoy fails the request
// if host is unavailable; if false, Envoy falls back to normal LB selection.
// Returns true if the override was accepted. In async mode, last call wins
// (the override pointer is overwritten).
func (w *Writer) SetUpstreamOverrideHost(host string, strict bool) bool {
	if w.calloutStarted || w.async != nil {
		w.override = &upstreamOverrideMutation{host: host, strict: strict}
		return true
	}
	return w.handle.SetUpstreamOverrideHost(host, strict)
}

// SetResponseHeader sets a response header. Valid only during response-phase
// callbacks (OnResponseHeaders, OnResponseBody). Not queued — applied inline.
func (w *Writer) SetResponseHeader(name, value string) {
	w.handle.ResponseHeaders().Set(name, value)
}

// AddResponseHeader adds a response header without removing existing values.
func (w *Writer) AddResponseHeader(name, value string) {
	w.handle.ResponseHeaders().Add(name, value)
}

// RemoveResponseHeader removes all values for the named response header.
func (w *Writer) RemoveResponseHeader(name string) {
	w.handle.ResponseHeaders().Remove(name)
}

// SetRequestBody marks data as the replacement for the request body buffer.
// Only effective in buffered mode (RegisterWithMutableBody); a no-op otherwise.
// The replacement is applied by the filter after the body handler returns.
func (w *Writer) SetRequestBody(data []byte) {
	w.requestBodyReplacement = data
	w.hasRequestBodyReplacement = true
}

// SetResponseBody marks data as the replacement for the response body buffer.
// Only effective in buffered mode (RegisterWithMutableBody); a no-op otherwise.
func (w *Writer) SetResponseBody(data []byte) {
	w.responseBodyReplacement = data
	w.hasResponseBodyReplacement = true
}

// IncrementCounter adds delta to the counter metric identified by id.
// id must have been obtained from ConfigHandle.DefineCounter at config time.
// In async mode, increments are queued and applied in order at flush time.
func (w *Writer) IncrementCounter(id MetricID, delta uint64) {
	if w.calloutStarted || w.async != nil {
		w.counters = append(w.counters, counterMutation{id: id, delta: delta})
		return
	}
	w.handle.IncrementCounterValue(shared.MetricID(id), delta)
}

// Go upgrades this request to asynchronous mode. fn runs in a new goroutine
// and may call w.Do to issue outbound HTTP callouts. After fn returns, Transit
// hops back to the Envoy worker thread and applies any queued mutations, then
// resumes the request — unless the stream was completed while fn was running.
//
// Panics if called twice on the same Writer or after HTTPCallout.
//
// IMPORTANT: SendLocalResponse from inside fn is NOT reliable. Envoy only
// honours it from filter callbacks (OnRequestHeaders, OnHttpCalloutDone), not
// from scheduled callbacks. Use HTTPCallout (callback form) if the filter
// needs to reject the request with a local response.
func (w *Writer) Go(fn func(ctx context.Context)) {
	if w.async != nil {
		panic("up: Go called twice on the same request")
	}
	if w.calloutStarted {
		panic("up: Go cannot be started after HTTPCallout")
	}
	// Acquire the scheduler on the worker thread before launching the goroutine.
	// GetScheduler is a CGO call; it must not be called from the goroutine.
	scheduler := w.handle.GetScheduler()
	state := &asyncState{scheduler: scheduler}
	ctx, cancel := context.WithCancel(context.Background())
	state.cancel = cancel
	w.async = state
	go func() {
		// defer cancel so that resources are cleaned up on both normal and
		// panicking exits. cancel is idempotent.
		defer cancel()
		fn(ctx)
		// If OnStreamComplete fired while fn was running, done() is true.
		// Do not call finish — the stream is dead and ContinueRequest would
		// be a no-op at best and a crash at worst.
		if state.done() {
			return
		}
		state.finish(w)
	}()
}

// HTTPCallout initiates an outbound Envoy HTTP callout and pauses the request
// until the callout completes. fn is invoked on the Envoy worker thread (or
// synchronously if the callback fires before HttpCallout returns) with the
// callout result. fn may queue mutations or call SendLocalResponse.
//
// Returns HTTPCalloutInitSuccess and nil error if Envoy accepted the callout.
// Returns a non-nil error if Envoy rejected the callout; in that case fn is
// never called.
//
// Panics if called after Go or after a previous HTTPCallout.
func (w *Writer) HTTPCallout(req HTTPCalloutRequest, fn HTTPCalloutFunc) (HTTPCalloutInitResult, error) {
	if w.calloutStarted || w.async != nil {
		panic("up: HTTPCallout cannot be started after Go or another HTTPCallout")
	}
	// Store calloutFn and set calloutStarted BEFORE calling handle.HttpCallout.
	// Reason: HttpCallout may invoke the callback synchronously before returning.
	// If we set calloutStarted after, that synchronous callback would call
	// mutation methods that apply directly to the handle (wrong — we need them
	// queued), and calloutFn would be nil when OnHttpCalloutDone fires.
	w.calloutFn = fn
	w.calloutStarted = true
	init, _ := w.handle.HttpCallout(
		req.Cluster,
		req.Headers,
		req.Body,
		req.TimeoutMillis,
		w.calloutCB, // pre-set to the filter; routes to filter.OnHttpCalloutDone
	)
	calloutInit := HTTPCalloutInitResult(init)
	if calloutInit != HTTPCalloutInitSuccess {
		// Callout was rejected before any callback could fire. Roll back so
		// the Writer is left in a clean, reusable state.
		w.calloutFn = nil
		w.calloutStarted = false
		w.calloutState.Store(calloutStateActive)
		return calloutInit, errCalloutInitResult(calloutInit)
	}
	return HTTPCalloutInitSuccess, nil
}

// Do performs an Envoy HTTP callout from inside a Go goroutine and blocks
// until the callout completes or ctx is cancelled.
//
// Multiple Do calls may be in flight concurrently — fan-out is supported.
// However, Writer mutation methods (SetRequestHeader, etc.) are NOT goroutine-
// safe. Only call them after all fan-out goroutines have joined (e.g. after
// wg.Wait()), when the Go goroutine is again the sole writer.
//
// Panics if called outside a Go goroutine (w.async == nil).
//
// How it works:
//  1. Schedule posts a task to the Envoy worker thread (the only thread
//     allowed to call handle.HttpCallout).
//  2. The task calls HttpCallout with a per-Do doCallbackFunc.
//  3. When the callout completes, doCallbackFunc copies the response into
//     Go-owned memory and sends it on ch.
//  4. Do receives from ch and returns the copied response, which is safe to
//     use after Do returns because it no longer points to Envoy memory.
//  5. If ctx is cancelled before the response arrives, Do returns ctx.Err().
//     The callout may still complete in the background; its response is
//     discarded (ch is buffered, so the send does not block).
func (w *Writer) Do(ctx context.Context, req HTTPCalloutRequest) (*HTTPCalloutResponse, error) {
	if w.async == nil {
		panic("up: Do called outside Go")
	}
	type result struct {
		resp *HTTPCalloutResponse
		err  error
	}
	// Buffered channel of size 1 so the doCallbackFunc send never blocks,
	// even if Do has already returned due to context cancellation.
	ch := make(chan result, 1)
	w.async.scheduler.Schedule(func() {
		// Check for stream cancellation before issuing the callout. If
		// OnStreamComplete already fired, done() is true and issuing a new
		// callout would be pointless (and potentially unsafe).
		if w.async.done() {
			ch <- result{err: context.Canceled}
			return
		}
		init, _ := w.handle.HttpCallout(
			req.Cluster,
			req.Headers,
			req.Body,
			req.TimeoutMillis,
			doCallbackFunc(func(_ uint64, r HTTPCalloutResult, headers [][2]shared.UnsafeEnvoyBuffer, body []shared.UnsafeEnvoyBuffer) {
				// Copy before sending: headers and body point into Envoy-owned
				// memory that is only valid during this callback. By the time
				// the goroutine reads from ch, that memory may be freed.
				ch <- result{resp: &HTTPCalloutResponse{
					Result:  r,
					Headers: copyUnsafeEnvoyHeaderBuffers(headers),
					Body:    copyUnsafeEnvoyBuffers(body),
				}}
			}),
		)
		calloutInit := HTTPCalloutInitResult(init)
		if calloutInit != HTTPCalloutInitSuccess {
			ch <- result{err: errCalloutInitResult(calloutInit)}
		}
	})
	select {
	case <-ctx.Done():
		// ctx was cancelled (stream complete or caller timeout). The callout
		// may still be in flight; its result will be sent to the buffered ch
		// and discarded. No cleanup needed.
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

// flush applies all queued mutations to the live Envoy stream and optionally
// calls ContinueRequest to resume a paused stream.
//
// This MUST be called on the Envoy worker thread. All CGO calls it makes
// (RequestHeaders().Set, SetFilterState, IncrementCounterValue,
// SetUpstreamOverrideHost, SendLocalResponse, ContinueRequest) are only safe
// from the worker thread. On the HTTPCallout path, OnHttpCalloutDone provides
// that guarantee. On the Go+Do path, asyncState.finish uses scheduler.Schedule
// to hop back to the worker thread before calling flush.
//
// continueReq semantics:
//   - true:  caller is an async path that paused the stream (returned Stop).
//     ContinueRequest must be called to resume processing. Used by
//     OnHttpCalloutDone (Paused→Flushed) and asyncState.finish.
//   - false: caller is the sync-done path where OnRequestHeaders detected
//     a completed callout and is about to return Continue itself.
//     ContinueRequest must NOT be called — Envoy would see a double-resume.
//
// If a local response was queued, it is sent and flush returns immediately
// without applying other mutations or calling ContinueRequest. The local
// response terminates the request, so the other mutations are moot.
func (w *Writer) flush(continueReq bool) {
	if w.localReply != nil {
		// Local responses take priority over all other mutations. Once sent,
		// the stream is terminal — no point applying header mutations.
		w.handle.SendLocalResponse(w.localReply.status, w.localReply.headers, w.localReply.body, "")
		return
	}
	hdrs := w.handle.RequestHeaders()
	for _, m := range w.reqHeaders {
		switch {
		case m.del:
			hdrs.Remove(m.name)
		case m.add:
			hdrs.Add(m.name, m.value)
		default:
			hdrs.Set(m.name, m.value)
		}
	}
	for _, m := range w.filterState {
		w.handle.SetFilterState(m.key, m.value)
	}
	for _, m := range w.counters {
		w.handle.IncrementCounterValue(shared.MetricID(m.id), m.delta)
	}
	if w.override != nil {
		w.handle.SetUpstreamOverrideHost(w.override.host, w.override.strict)
	}
	if w.hasRequestBodyReplacement {
		buf := w.handle.BufferedRequestBody()
		if buf != nil {
			buf.Drain(buf.GetSize())
			buf.Append(w.requestBodyReplacement)
			// content-length was pre-cleared in OnRequestHeaders; set the
			// correct value now that we know the replacement length.
			w.handle.RequestHeaders().Set("content-length", fmt.Sprintf("%d", len(w.requestBodyReplacement)))
		}
	}
	if continueReq {
		w.handle.ContinueRequest()
	}
}
