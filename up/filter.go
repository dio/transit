package up

import (
	"fmt"
	"strconv"
	"sync/atomic"

	"github.com/envoyproxy/envoy/source/extensions/dynamic_modules/sdk/go/shared"
)

type configFactory struct {
	name               string
	configFn           ConfigFunc
	handler            HandlerFunc
	responseHandler    ResponseHandlerFunc
	requestBodyHandler RequestBodyHandlerFunc
	bufferBody         bool
	group              *Group
}

// configHandleImpl wraps shared.HttpFilterConfigHandle to implement ConfigHandle.
type configHandleImpl struct{ h shared.HttpFilterConfigHandle }

func (c *configHandleImpl) DefineCounter(name string, tagKeys ...string) (MetricID, error) {
	id, res := c.h.DefineCounter(name, tagKeys...)
	if res != shared.MetricsSuccess {
		return 0, fmt.Errorf("up: DefineCounter %q failed (result=%d)", name, res)
	}
	return MetricID(id), nil
}

func (f *configFactory) Create(h shared.HttpFilterConfigHandle, _ []byte) (shared.HttpFilterFactory, error) {
	if f.configFn != nil {
		if err := f.configFn(&configHandleImpl{h: h}); err != nil {
			return nil, err
		}
	}
	var stop func()
	if f.group != nil {
		f.group.Start()
		stop = f.group.Stop
	}
	return &filterFactory{
		name:               f.name,
		handler:            f.handler,
		responseHandler:    f.responseHandler,
		requestBodyHandler: f.requestBodyHandler,
		bufferBody:         f.bufferBody,
		stop:               stop,
	}, nil
}

func (f *configFactory) CreatePerRoute(_ []byte) (any, error) { return nil, nil }

type filterFactory struct {
	name               string
	handler            HandlerFunc
	responseHandler    ResponseHandlerFunc
	requestBodyHandler RequestBodyHandlerFunc
	bufferBody         bool
	stop               func()
}

func (f *filterFactory) Create(handle shared.HttpFilterHandle) shared.HttpFilter {
	return &filter{
		name:               f.name,
		handle:             handle,
		handler:            f.handler,
		responseHandler:    f.responseHandler,
		requestBodyHandler: f.requestBodyHandler,
		bufferBody:         f.bufferBody,
	}
}

func (f *filterFactory) OnDestroy() {
	if f.stop != nil {
		f.stop()
	}
}

// filter is the per-stream HTTP filter instance. Envoy creates one filter per
// HTTP stream via filterFactory.Create and calls the lifecycle methods below.
// There is one filter per stream; it is never shared between streams.
//
// Concurrency model:
//
// Normally all Envoy callbacks (OnRequestHeaders, OnRequestBody, etc.) run on
// the same worker thread, and filter fields can be accessed without any lock.
// There are two exceptions that require atomic operations:
//
//  1. OnHttpCalloutDone may fire from a different goroutine when the callout
//     response arrives before OnRequestHeaders returns (the "synchronous early
//     callback" case). calloutWriter is an atomic.Pointer so both the headers
//     callback and the callout callback can safely read/nil it without a race.
//
//  2. asyncState.completed is an atomic.Bool; see asyncState for details.
//
// All other fields are only touched from the worker thread and need no locking.
type filter struct {
	shared.EmptyHttpFilter

	// name is the filter's registered name, used for per-route context lookup.
	name string

	// handle is the Envoy stream handle. All CGO calls (SetRequestHeader,
	// ContinueRequest, etc.) go through this handle. Never call CGO methods on
	// handle from a goroutine — always use scheduler.Schedule to hop back to the
	// worker thread first (see Writer.Go and asyncState.finish).
	handle shared.HttpFilterHandle

	handler            HandlerFunc
	responseHandler    ResponseHandlerFunc
	requestBodyHandler RequestBodyHandlerFunc
	bufferBody         bool

	// context is the per-stream opaque value passed to handlers via Request and
	// ResponseChunk. Handlers may store cross-callback state here (e.g. a
	// request ID parsed in OnRequestHeaders and emitted in OnResponseHeaders).
	context any

	// requestContentType and requestContentEncoding are captured from request
	// headers so the body handler receives them without re-reading the header map
	// (which may no longer be valid when OnRequestBody fires).
	requestContentType      string
	requestContentEncoding  string
	responseContentType     string
	responseContentEncoding string

	// async is set when Writer.Go is called and a goroutine is running.
	// It is nil for the HTTPCallout path. See asyncState for the goroutine
	// lifecycle and the one race it protects (finish vs OnStreamComplete).
	async *asyncState

	// calloutWriter holds the Writer for the in-flight HTTPCallout request.
	//
	// Why atomic.Pointer: OnHttpCalloutDone may fire from a different goroutine
	// (the synchronous early-callback race) while OnRequestHeaders is still
	// executing on the worker thread. Both sides read and nil this pointer.
	// A plain *Writer would be a data race; atomic.Pointer[Writer] is the
	// minimal safe primitive.
	//
	// The pointer is set in OnRequestHeaders before initiating the callout and
	// nilled in one of three places:
	//   – OnHttpCalloutDone (async path): after CAS Paused→Flushed.
	//   – flushCompletedCallout (sync path): after CAS Done→Flushed.
	//   – OnStreamComplete: if the stream ends while a callout is in-flight.
	calloutWriter atomic.Pointer[Writer]
}

// OnRequestHeaders is the first callback Envoy invokes for each HTTP request.
// It runs the user handler and then drives the callout and async state machines.
//
// Return values and their meaning:
//
//	HeadersStatusStop     A callout or goroutine is running; Envoy pauses the
//	                      stream. Either OnHttpCalloutDone or scheduler.Schedule
//	                      will call ContinueRequest to resume it.
//
//	HeadersStatusContinue The handler finished synchronously (or the callout
//	                      callback fired before OnRequestHeaders returned, so no
//	                      explicit resume is needed). Envoy proceeds immediately.
//
// There are four distinct outcomes after f.handler(w, req) returns:
//
//  1. HTTPCallout initiated, callback NOT yet fired (calloutStarted && Active):
//     pausePendingCallout CAS Active→Paused succeeds → return Stop.
//
//  2. HTTPCallout initiated, callback fired BEFORE handler returned (calloutStarted && Done):
//     pausePendingCallout fails; flushCompletedCallout CAS Done→Flushed → flush inline,
//     continue normally (return Continue or Stop based on w.stopped).
//
//  3. w.Go called: f.async is set → return Stop (scheduler will resume).
//
//  4. Synchronous (no callout, no goroutine): flush inline, return based on w.stopped.
func (f *filter) OnRequestHeaders(headers shared.HeaderMap, endOfStream bool) shared.HeadersStatus {
	if f.requestBodyHandler != nil {
		f.requestContentType = headers.GetOne("content-type").ToString()
		f.requestContentEncoding = headers.GetOne("content-encoding").ToString()
	}

	// Pre-set calloutWriter and calloutCB before the handler runs. This allows
	// Writer.HTTPCallout to store the callback directly on the Writer without
	// allocating a closure, and filter.OnHttpCalloutDone can load the Writer
	// atomically in the callback. The pointer is cleared after the handler
	// returns (or after the callout completes in async path).
	w := &Writer{handle: f.handle, calloutCB: f}
	f.calloutWriter.Store(w)
	f.handler(w, newRequestWithContext(headers, f.name, &f.context))

	if f.pausePendingCallout(w) {
		// HTTPCallout was initiated and the callback has not fired yet (Active→Paused
		// CAS succeeded). Return Stop; OnHttpCalloutDone will call ContinueRequest.
		return shared.HeadersStatusStop
	}
	if w.async != nil {
		// w.Go was called; a goroutine is running. Park the asyncState on the
		// filter so OnStreamComplete can cancel it, then return Stop. The goroutine
		// will call scheduler.Schedule → flush(true) → ContinueRequest when done.
		f.async = w.async
		f.calloutWriter.Store(nil)
		return shared.HeadersStatusStop
	}
	if w.stopped {
		// The handler called SendLocalResponse. Flush mutations (which will send
		// the local response) without calling ContinueRequest.
		f.flushCompletedCallout(w)
		f.calloutWriter.Store(nil)
		return shared.HeadersStatusStop
	}

	if endOfStream {
		// No body is coming (GET, DELETE, HEAD, etc.). Synthesize a body call
		// so requestBodyHandler sees EndStream=true and can finalize its work.
		if f.requestBodyHandler != nil {
			f.requestBodyHandler(w, &BodyChunk{
				EndStream:       true,
				ContentType:     f.requestContentType,
				ContentEncoding: f.requestContentEncoding,
				Context:         &f.context,
			})
		}
		if f.pausePendingCallout(w) {
			return shared.HeadersStatusStop
		}
		if w.async != nil {
			f.async = w.async
			f.calloutWriter.Store(nil)
			return shared.HeadersStatusStop
		}
		f.flushCompletedCallout(w)
		f.calloutWriter.Store(nil)
		if w.stopped {
			return shared.HeadersStatusStop
		}
		return shared.HeadersStatusContinue
	}

	f.flushCompletedCallout(w)
	f.calloutWriter.Store(nil)
	if w.stopped {
		return shared.HeadersStatusStop
	}

	// A request body is coming. Strip content-length and transfer-encoding now
	// so upstream never sees a stale value after SetRequestBody replaces the body.
	// The correct content-length is written in OnRequestBody after any replacement.
	//
	// Why NOT return HeadersStatusStopAllAndBuffer here: returning that status from
	// OnRequestHeaders freezes the filter chain permanently. Envoy buffers body
	// data but never calls OnRequestBody because the SDK has no asynchronous resume
	// path for that status. Use BodyStatusStopAndBuffer from OnRequestBody instead.
	if f.bufferBody && f.requestBodyHandler != nil {
		headers.Remove("content-length")
		headers.Remove("transfer-encoding")
	}
	return shared.HeadersStatusContinue
}

// OnRequestBody is called for each body data frame. If bufferBody is true,
// Transit returns BodyStatusStopAndBuffer until endOfStream, accumulating all
// chunks in Envoy's buffer before delivering the full body to the handler.
//
// This is the only callback that may call SetRequestBody with a replacement.
// If a replacement was set, Transit drains the Envoy buffer and appends the new
// body, then sets the correct content-length on the request headers.
func (f *filter) OnRequestBody(body shared.BodyBuffer, endOfStream bool) shared.BodyStatus {
	if f.requestBodyHandler == nil {
		return shared.BodyStatusContinue
	}
	if f.bufferBody && !endOfStream {
		// More data coming; accumulate in Envoy's buffer.
		return shared.BodyStatusStopAndBuffer
	}

	var data []byte
	src := body
	if f.bufferBody {
		// Use the accumulated buffer instead of the current chunk.
		src = f.handle.BufferedRequestBody()
	}
	for _, chunk := range src.GetChunks() {
		data = append(data, chunk.ToBytes()...)
	}

	w := &Writer{handle: f.handle}
	f.requestBodyHandler(w, &BodyChunk{
		Data:            data,
		EndStream:       endOfStream,
		ContentType:     f.requestContentType,
		ContentEncoding: f.requestContentEncoding,
		Context:         &f.context,
	})
	if w.async != nil {
		f.async = w.async
		return shared.BodyStatusStopAndBuffer
	}

	if f.bufferBody && w.hasRequestBodyReplacement {
		buf := f.handle.BufferedRequestBody()
		buf.Drain(buf.GetSize())
		buf.Append(w.requestBodyReplacement)
		// content-length was cleared in OnRequestHeaders; set the correct value now.
		f.handle.RequestHeaders().Set("content-length", strconv.Itoa(len(w.requestBodyReplacement)))
	}
	return shared.BodyStatusContinue
}

// OnHttpCalloutDone implements shared.HttpCalloutCallback. Envoy invokes this
// when an outbound HTTP callout response (or error) arrives.
//
// This method may be called from a different goroutine than the worker thread
// when the callback fires synchronously before OnRequestHeaders returns —
// Envoy does not guarantee deferred delivery. The calloutState CAS determines
// which of the two orderings occurred and takes the correct action:
//
//	Paused→Flushed  (normal async path)
//	  OnRequestHeaders already returned HeadersStatusStop. This callback owns the
//	  resume. Call flush(true) which applies mutations and calls ContinueRequest.
//
//	Active→Done     (early synchronous callback)
//	  OnRequestHeaders has not yet reached pausePendingCallout. Mark Done so
//	  that flushCompletedCallout (called by OnRequestHeaders) will pick it up,
//	  flush mutations inline, and let OnRequestHeaders return Continue itself.
//	  We must NOT call ContinueRequest here — OnRequestHeaders hasn't returned Stop.
func (f *filter) OnHttpCalloutDone(
	_ uint64,
	result shared.HttpCalloutResult,
	headers [][2]shared.UnsafeEnvoyBuffer,
	body []shared.UnsafeEnvoyBuffer,
) {
	w := f.calloutWriter.Load()
	if w == nil || w.calloutFn == nil {
		// No callout in flight (stream may have been cancelled), or the callback
		// was already consumed. Nothing to do.
		return
	}
	// Consume the callback exactly once: nil it before invoking so re-entrant
	// or duplicate callbacks are safe no-ops.
	fn := w.calloutFn
	w.calloutFn = nil
	fn(HTTPCalloutResult(result), headers, body)

	// Now drive the state machine. Only one of these two CAS calls succeeds:
	if w.calloutState.CompareAndSwap(calloutStatePaused, calloutStateFlushed) {
		// Normal path: OnRequestHeaders already returned Stop. We own the resume.
		f.calloutWriter.Store(nil)
		w.flush(true)
		return
	}
	// Early path: callback fired before OnRequestHeaders called pausePendingCallout.
	// Transition Active→Done so that flushCompletedCallout (in OnRequestHeaders)
	// can detect the completed state and flush without calling ContinueRequest.
	w.calloutState.CompareAndSwap(calloutStateActive, calloutStateDone)
}

// pausePendingCallout returns true if a callout was started and the CAS
// Active→Paused succeeded, meaning OnRequestHeaders should return Stop and wait
// for OnHttpCalloutDone to resume the stream.
//
// Returns false if:
//   - No callout was started (calloutStarted == false).
//   - The callout callback already fired (state is Done, not Active): the
//     sync-done path applies; OnRequestHeaders should flush inline and continue.
func (f *filter) pausePendingCallout(w *Writer) bool {
	if !w.calloutStarted {
		return false
	}
	// CAS Active→Paused. If state is already Done (early callback), this fails.
	return w.calloutState.CompareAndSwap(calloutStateActive, calloutStatePaused)
}

// flushCompletedCallout handles the synchronous early-callback path: the callout
// callback fired and transitioned Active→Done before OnRequestHeaders reached
// pausePendingCallout. We CAS Done→Flushed, apply mutations inline (flush(false)),
// and let OnRequestHeaders return HeadersStatusContinue itself (no ContinueRequest).
//
// If calloutStarted is false or the CAS fails (state is not Done), this is a no-op.
func (f *filter) flushCompletedCallout(w *Writer) {
	if !w.calloutStarted || !w.calloutState.CompareAndSwap(calloutStateDone, calloutStateFlushed) {
		return
	}
	f.calloutWriter.Store(nil)
	// flush(false): apply mutations but do NOT call ContinueRequest.
	// OnRequestHeaders will return HeadersStatusContinue to resume the stream.
	w.flush(false)
}

// OnStreamComplete is called when Envoy terminates the stream, regardless of
// whether the request completed normally or was reset. It runs on the worker
// thread.
//
// Two cleanup cases:
//
//  1. HTTPCallout in-flight: a callout was started but the response never arrived
//     (stream reset). Discard the calloutWriter so OnHttpCalloutDone (if it fires
//     late) finds nil and does nothing.
//
//  2. Go goroutine running: cancel the goroutine's context so it exits promptly,
//     and record completed=true so asyncState.finish does not schedule a resume
//     on a stream that no longer exists.
func (f *filter) OnStreamComplete() {
	if f.calloutWriter.Load() != nil {
		// Stream ended while a callout was in-flight: discard the writer.
		f.calloutWriter.Store(nil)
	}
	if f.async != nil {
		f.async.completeWithoutResume()
		f.async = nil
	}
}

// OnResponseHeaders is called when the upstream response headers arrive. It runs
// the responseHandler (if set) and synthesizes a body call for bodyless responses.
//
// In buffered mode (bufferBody=true), content-length is removed here because
// SetResponseBody may replace the body; the correct value is written in
// OnResponseBody after any replacement. The same caveat applies as in
// OnRequestHeaders: do NOT return HeadersStatusStopAllAndBuffer.
func (f *filter) OnResponseHeaders(headers shared.HeaderMap, endOfStream bool) shared.HeadersStatus {
	if f.responseHandler == nil {
		return shared.HeadersStatusContinue
	}

	f.responseContentType = headers.GetOne("content-type").ToString()
	f.responseContentEncoding = headers.GetOne("content-encoding").ToString()

	// Strip content-length upfront in buffered mode so downstream never sees a
	// stale value after SetResponseBody replaces the body. The correct value is
	// written in OnResponseBody after any replacement.
	if f.bufferBody {
		headers.Remove("content-length")
	}

	statusCode := 0
	if v := headers.GetOne(":status"); v.Len > 0 {
		statusCode, _ = strconv.Atoi(v.ToString())
	}

	w := &Writer{handle: f.handle}
	f.responseHandler(w, &ResponseChunk{
		StatusCode:      statusCode,
		Headers:         headers,
		EndStream:       endOfStream,
		ContentEncoding: f.responseContentEncoding,
		ContentType:     f.responseContentType,
		Context:         &f.context,
	})

	if endOfStream {
		// Synthesize a body call for bodyless responses (204, HEAD, 304, etc.)
		// so the handler always sees EndStream=true exactly once.
		f.responseHandler(w, &ResponseChunk{
			EndStream:       true,
			ContentEncoding: f.responseContentEncoding,
			ContentType:     f.responseContentType,
			Context:         &f.context,
		})
	}
	return shared.HeadersStatusContinue
}

// OnResponseBody is called for each response body data frame. Mirrors the
// request body logic: accumulates in Envoy's buffer when bufferBody=true, then
// delivers the full body to responseHandler at endOfStream. If the handler calls
// SetResponseBody, Transit drains the buffer and writes the replacement.
func (f *filter) OnResponseBody(body shared.BodyBuffer, endOfStream bool) shared.BodyStatus {
	if f.responseHandler == nil {
		return shared.BodyStatusContinue
	}
	if f.bufferBody && !endOfStream {
		return shared.BodyStatusStopAndBuffer
	}

	var data []byte
	src := body
	if f.bufferBody {
		src = f.handle.BufferedResponseBody()
	}
	for _, chunk := range src.GetChunks() {
		data = append(data, chunk.ToBytes()...)
	}

	w := &Writer{handle: f.handle}
	f.responseHandler(w, &ResponseChunk{
		Data:            data,
		EndStream:       endOfStream,
		ContentEncoding: f.responseContentEncoding,
		ContentType:     f.responseContentType,
		Context:         &f.context,
	})

	if f.bufferBody && w.hasResponseBodyReplacement {
		buf := f.handle.BufferedResponseBody()
		buf.Drain(buf.GetSize())
		buf.Append(w.responseBodyReplacement)
		// content-length was cleared in OnResponseHeaders; set the correct value now.
		f.handle.ResponseHeaders().Set("content-length", strconv.Itoa(len(w.responseBodyReplacement)))
	}
	return shared.BodyStatusContinue
}
