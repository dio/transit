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
// Envoy does not guarantee that OnHttpCalloutDone fires after OnRequestHeaders
// returns. A callback can fire synchronously inside HttpCallout — before
// OnRequestHeaders has had a chance to return HeadersStatusStop. The four
// states handle both orderings without a mutex:
//
//   Active  → Paused   OnRequestHeaders got here first; it returns Stop and
//                      expects OnHttpCalloutDone to call flush+ContinueRequest.
//   Active  → Done     OnHttpCalloutDone fired first (synchronous callback);
//                      OnRequestHeaders detects Done, flushes inline, returns
//                      Continue — no Stop, no ContinueRequest needed.
//   Paused  → Flushed  Normal async path: callback fires after Stop was returned;
//                      flush(true) calls ContinueRequest to resume the stream.
//   Done    → Flushed  Sync-done path: OnRequestHeaders flushes with flush(false)
//                      because it will return Continue itself.
const (
	calloutStateActive int32 = iota
	calloutStatePaused
	calloutStateDone
	calloutStateFlushed
)

// Writer provides actions the handler can take on the current request.
type Writer struct {
	handle         shared.HttpFilterHandle
	stopped        bool
	async          *asyncState
	calloutCB      shared.HttpCalloutCallback // pre-set to filter before handler runs
	calloutFn      HTTPCalloutFunc
	calloutStarted bool
	calloutState   atomic.Int32

	// Mutation queues accumulate changes requested by the handler. They are
	// applied by flush() on the Envoy worker thread — never inline — so that
	// CGO calls (RequestHeaders().Set, SendLocalResponse, etc.) are never made
	// while a lock is held, eliminating the lock-while-CGO deadlock class.
	reqHeaders  []requestHeaderMutation
	filterState []filterStateMutation
	counters    []counterMutation
	override    *upstreamOverrideMutation
	localReply  *localResponse

	// body replacement — set via SetRequestBody/SetResponseBody in buffered mode
	requestBodyReplacement     []byte
	hasRequestBodyReplacement  bool
	responseBodyReplacement    []byte
	hasResponseBodyReplacement bool
}

// NewWriter wraps handle in a Writer. Intended for use in tests.
func NewWriter(h shared.HttpFilterHandle) *Writer { return &Writer{handle: h} }

// Log emits a message via Envoy's logging mechanism.
func (w *Writer) Log(level LogLevel, format string, args ...any) {
	w.handle.Log(shared.LogLevel(level), format, args...)
}

// SendLocalResponse sends an immediate response to the client and stops filter chain
// processing. Subsequent filters are not invoked. Optional headers are sent with the
// response (e.g. [2]string{"content-type", "application/json"}).
func (w *Writer) SendLocalResponse(status int, body []byte, headers ...[2]string) {
	if w.calloutStarted || w.async != nil {
		if w.localReply == nil {
			w.localReply = &localResponse{status: uint32(status), headers: headers, body: body}
		}
		w.stopped = true
		return
	}
	w.handle.SendLocalResponse(uint32(status), headers, body, "")
	w.stopped = true
}

// GetAttributeString returns the string stream attribute for the given ID.
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
func (w *Writer) GetActiveSpan() shared.Span {
	return w.handle.GetActiveSpan()
}

// SetRequestHeader sets a request header. Valid during request-phase callbacks.
func (w *Writer) SetRequestHeader(name, value string) {
	if w.calloutStarted || w.async != nil {
		w.reqHeaders = append(w.reqHeaders, requestHeaderMutation{name: name, value: value})
		return
	}
	w.handle.RequestHeaders().Set(name, value)
}

// AddRequestHeader adds a request header without removing existing values.
func (w *Writer) AddRequestHeader(name, value string) {
	if w.calloutStarted || w.async != nil {
		w.reqHeaders = append(w.reqHeaders, requestHeaderMutation{name: name, value: value, add: true})
		return
	}
	w.handle.RequestHeaders().Add(name, value)
}

// RemoveRequestHeader removes a request header.
func (w *Writer) RemoveRequestHeader(name string) {
	if w.calloutStarted || w.async != nil {
		w.reqHeaders = append(w.reqHeaders, requestHeaderMutation{name: name, del: true})
		return
	}
	w.handle.RequestHeaders().Remove(name)
}

// SetFilterState stores raw per-stream filter state for later filters, access
// loggers, or upstream selection callbacks.
func (w *Writer) SetFilterState(key, value string) {
	if w.calloutStarted || w.async != nil {
		w.filterState = append(w.filterState, filterStateMutation{key: key, value: []byte(value)})
		return
	}
	w.handle.SetFilterState(key, []byte(value))
}

// SetUpstreamOverrideHost asks Envoy's upstream load balancer to prefer host.
// When strict is true, Envoy fails the request if host is unavailable.
func (w *Writer) SetUpstreamOverrideHost(host string, strict bool) bool {
	if w.calloutStarted || w.async != nil {
		w.override = &upstreamOverrideMutation{host: host, strict: strict}
		return true
	}
	return w.handle.SetUpstreamOverrideHost(host, strict)
}

// SetResponseHeader sets a response header. Valid during response-phase callbacks.
func (w *Writer) SetResponseHeader(name, value string) {
	w.handle.ResponseHeaders().Set(name, value)
}

// AddResponseHeader adds a response header without removing existing values.
func (w *Writer) AddResponseHeader(name, value string) {
	w.handle.ResponseHeaders().Add(name, value)
}

// RemoveResponseHeader removes a response header.
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
func (w *Writer) IncrementCounter(id MetricID, delta uint64) {
	if w.calloutStarted || w.async != nil {
		w.counters = append(w.counters, counterMutation{id: id, delta: delta})
		return
	}
	w.handle.IncrementCounterValue(shared.MetricID(id), delta)
}

// Go upgrades this request to asynchronous mode. fn runs in a goroutine and
// Transit resumes the request after fn returns, unless fn sent a local response
// or the stream completed first.
func (w *Writer) Go(fn func(ctx context.Context)) {
	if w.async != nil {
		panic("up: Go called twice on the same request")
	}
	scheduler := w.handle.GetScheduler()
	state := &asyncState{scheduler: scheduler}
	ctx, cancel := context.WithCancel(context.Background())
	state.cancel = cancel
	w.async = state
	go func() {
		defer cancel()
		fn(ctx)
		if state.done() {
			return
		}
		state.finish(w)
	}()
}

// HTTPCallout initiates an outbound Envoy HTTP callout. It must be called from
// a request callback. Transit pauses the request and resumes after fn returns.
func (w *Writer) HTTPCallout(req HTTPCalloutRequest, fn HTTPCalloutFunc) (HTTPCalloutInitResult, error) {
	if w.calloutStarted || w.async != nil {
		panic("up: HTTPCallout cannot be started after Go or another HTTPCallout")
	}
	// Set calloutStarted before HttpCallout so the callback (which may fire
	// synchronously or from a goroutine before HttpCallout returns) always sees
	// calloutStarted = true and queues mutations via Writer fields rather than
	// applying them directly on the handle.
	w.calloutFn = fn
	w.calloutStarted = true
	init, _ := w.handle.HttpCallout(
		req.Cluster,
		req.Headers,
		req.Body,
		req.TimeoutMillis,
		w.calloutCB,
	)
	calloutInit := HTTPCalloutInitResult(init)
	if calloutInit != HTTPCalloutInitSuccess {
		w.calloutFn = nil
		w.calloutStarted = false
		w.calloutState.Store(calloutStateActive)
		return calloutInit, errCalloutInitResult(calloutInit)
	}
	return HTTPCalloutInitSuccess, nil
}

// Do performs an Envoy HTTP callout from inside Go mode and blocks until the
// callout completes or ctx is cancelled. Multiple Do calls may be in flight at
// once; callers should join fanout goroutines before mutating Writer state.
func (w *Writer) Do(ctx context.Context, req HTTPCalloutRequest) (*HTTPCalloutResponse, error) {
	if w.async == nil {
		panic("up: Do called outside Go")
	}
	type result struct {
		resp *HTTPCalloutResponse
		err  error
	}
	ch := make(chan result, 1)
	w.async.scheduler.Schedule(func() {
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
				ch <- result{resp: &HTTPCalloutResponse{Result: r, Headers: headers, Body: body}}
			}),
		)
		calloutInit := HTTPCalloutInitResult(init)
		if calloutInit != HTTPCalloutInitSuccess {
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

// flush applies all queued mutations and optionally resumes the request.
// Must run on the Envoy worker thread — CGO calls are only safe there.
//
// continueReq=true on the async paths (callout callback, scheduler hop) where
// Envoy is waiting for an explicit ContinueRequest to resume the stream.
// continueReq=false on the sync-done path where OnRequestHeaders returns
// HeadersStatusContinue itself and ContinueRequest must not be called twice.
func (w *Writer) flush(continueReq bool) {
	if w.localReply != nil {
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
			w.handle.RequestHeaders().Set("content-length", fmt.Sprintf("%d", len(w.requestBodyReplacement)))
		}
	}
	if continueReq {
		w.handle.ContinueRequest()
	}
}
