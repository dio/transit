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

// filter is the per-stream HTTP filter instance. Envoy creates one per HTTP
// stream and calls the lifecycle methods below. It is never shared across streams.
//
// Phase 2 ownership model:
//
// All mutable stream state now lives here instead of on the ephemeral Writer.
// Writer is a thin per-invocation view (see writer.go). The critical rule:
// flush resets all queues immediately after applying them to prevent stale
// mutations from replaying on a later callback (e.g. a header mutation queued
// in OnRequestHeaders must not appear when OnRequestBody runs).
//
// Goroutine safety (Go+Do path):
//
// Once Writer.Go launches a goroutine, that goroutine is the sole writer to the
// mutation queues until it returns. OnStreamComplete must not reset those queues
// while the goroutine may still be writing — it only touches async.cancel and
// async.completed. The queue reset happens inside flush, which runs on the
// worker thread after the goroutine has called scheduler.Schedule and exited.
//
// HTTPCallout concurrency:
//
// calloutFn and calloutState replace the former calloutWriter atomic.Pointer.
// calloutFn is set before handle.HttpCallout is called; the go statement (or CGO
// call) inside the fake/real HttpCallout establishes the happens-before that makes
// OnHttpCalloutDone's access to calloutFn safe without an additional mutex.
// streamDone.Store(true) in OnStreamComplete prevents a late OnHttpCalloutDone
// from calling flush on a dead stream.
type filter struct {
	shared.EmptyHttpFilter

	name               string
	handle             shared.HttpFilterHandle
	handler            HandlerFunc
	responseHandler    ResponseHandlerFunc
	requestBodyHandler RequestBodyHandlerFunc
	bufferBody         bool
	context            any

	// captured from headers callbacks for body callbacks
	requestContentType      string
	requestContentEncoding  string
	responseContentType     string
	responseContentEncoding string

	// async holds the goroutine lifecycle for the Go+Do path. nil on HTTPCallout path.
	// Only touched by OnStreamComplete (cancel/complete) and Writer.Go (set).
	// Never reset or read for queue operations while a goroutine may be writing.
	async *asyncState

	// --- Mutation queues (request phase) ---
	//
	// Written by Writer mutation methods (unconditionally). Applied and reset
	// by flush(). Response mutation methods bypass these queues and apply inline.
	//
	// Stale-prevention invariant: flush resets every queue to zero length before
	// returning. A callback that runs after flush sees an empty queue.
	stopped     bool // set by SendLocalResponse; cleared by flush on non-local-reply path
	reqHeaders  []requestHeaderMutation
	filterState []filterStateMutation
	counters    []counterMutation
	override    *upstreamOverrideMutation
	localReply  *localResponse // set by SendLocalResponse; cleared by flush after sending

	// stripFramingOnResume is set by OnRequestHeaders when a body is expected and
	// buffered-body replacement is active. flush removes content-length and
	// transfer-encoding after applying all queued header mutations, so that a
	// handler-queued framing value cannot reach upstream before OnRequestBody
	// writes the correct content-length after body replacement.
	stripFramingOnResume bool

	// Body replacements are set by SetRequestBody/SetResponseBody and applied
	// inline in the body callbacks (not via flush). The flags are cleared
	// immediately after application to prevent replay on the next body frame.
	requestBodyReplacement     []byte
	hasRequestBodyReplacement  bool
	responseBodyReplacement    []byte
	hasResponseBodyReplacement bool

	// --- HTTPCallout path state ---
	//
	// calloutFn is the user callback set by Writer.HTTPCallout. Read and cleared
	// exactly once by OnHttpCalloutDone. Protected by the happens-before of the
	// go statement (or CGO call) inside handle.HttpCallout.
	calloutFn HTTPCalloutFunc

	// calloutState is the atomic handoff between OnRequestHeaders and
	// OnHttpCalloutDone. See the const block in writer.go for the full state
	// machine. Zero value = calloutStateActive.
	calloutState atomic.Int32

	// streamDone is set by OnStreamComplete to prevent a late OnHttpCalloutDone
	// from calling flush on a terminated stream.
	streamDone atomic.Bool
}

// flush applies all queued request-phase mutations and optionally resumes the stream.
//
// Must be called on the Envoy worker thread. All CGO calls made here
// (RequestHeaders().Set, SetFilterState, ContinueRequest, etc.) require it.
// On the HTTPCallout path this is guaranteed by OnHttpCalloutDone. On the
// Go+Do path, asyncState.finish uses scheduler.Schedule to hop back first.
//
// continueReq=true:  async path — stream was paused; call ContinueRequest to resume.
// continueReq=false: sync-done path — OnRequestHeaders returns Continue itself;
//                    do NOT call ContinueRequest (would double-resume).
//
// Stale-mutation reset: every queue is drained to zero before flush returns so
// that a later callback on the same stream starts with empty queues.
//
// Local-reply path: if a local response was queued, it is sent and flush returns
// immediately without applying other mutations or resetting stopped. The stream
// is terminal; stopped stays true so the caller can return Stop.
func (f *filter) flush(continueReq bool) {
	if f.localReply != nil {
		f.handle.SendLocalResponse(f.localReply.status, f.localReply.headers, f.localReply.body, "")
		f.localReply = nil
		// stopped stays true; caller checks it to return HeadersStatusStop.
		return
	}
	if len(f.reqHeaders) > 0 {
		hdrs := f.handle.RequestHeaders()
		for _, m := range f.reqHeaders {
			switch {
			case m.del:
				hdrs.Remove(m.name)
			case m.add:
				hdrs.Add(m.name, m.value)
			default:
				hdrs.Set(m.name, m.value)
			}
		}
		f.reqHeaders = f.reqHeaders[:0]
	}
	// Strip framing headers after all queued mutations so a handler-queued
	// content-length or transfer-encoding is removed, not forwarded upstream.
	// The correct content-length is written by OnRequestBody after replacement.
	if f.stripFramingOnResume {
		hdrs := f.handle.RequestHeaders()
		hdrs.Remove("content-length")
		hdrs.Remove("transfer-encoding")
		f.stripFramingOnResume = false
	}
	for _, m := range f.filterState {
		f.handle.SetFilterState(m.key, m.value)
	}
	f.filterState = f.filterState[:0]
	for _, m := range f.counters {
		f.handle.IncrementCounterValue(shared.MetricID(m.id), m.delta)
	}
	f.counters = f.counters[:0]
	if f.override != nil {
		f.handle.SetUpstreamOverrideHost(f.override.host, f.override.strict)
		f.override = nil
	}
	// Body replacement on the async path: a goroutine may have called
	// SetRequestBody, setting hasRequestBodyReplacement before scheduler.Schedule.
	// The sync body path clears these flags in OnRequestBody before calling flush,
	// so this branch only fires on the async body path.
	if f.hasRequestBodyReplacement {
		buf := f.handle.BufferedRequestBody()
		if buf != nil {
			buf.Drain(buf.GetSize())
			buf.Append(f.requestBodyReplacement)
			// content-length was cleared above; set the replacement body's value.
			f.handle.RequestHeaders().Set("content-length", fmt.Sprintf("%d", len(f.requestBodyReplacement)))
		}
		f.hasRequestBodyReplacement = false
		f.requestBodyReplacement = nil
	}
	f.stopped = false
	if continueReq {
		f.handle.ContinueRequest()
	}
}

// OnRequestHeaders is the first per-request callback. It runs the handler then
// drives the callout/async state machines.
//
// Synchronous exits (no callout, no goroutine) call flush(false) before returning
// so that mutations queued during the handler are applied. This is necessary
// because mutations are now always queued — without flush they would never fire.
//
// See filter type comment for the HTTPCallout state machine and goroutine rules.
func (f *filter) OnRequestHeaders(headers shared.HeaderMap, endOfStream bool) shared.HeadersStatus {
	if f.requestBodyHandler != nil {
		f.requestContentType = headers.GetOne("content-type").ToString()
		f.requestContentEncoding = headers.GetOne("content-encoding").ToString()
	}

	w := &Writer{f: f, calloutCB: f}
	f.handler(w, newRequestWithContext(headers, f.name, &f.context))

	// Mark that framing headers must be stripped. The actual removal happens in
	// flush after all queued header mutations are applied, so a handler-queued
	// content-length or transfer-encoding cannot survive past the strip point.
	// Not needed when endOfStream is true (no body replacement will follow).
	//
	// Do NOT use HeadersStatusStopAllAndBuffer: the SDK has no async resume path
	// for that status and it freezes the filter chain permanently.
	if !endOfStream && f.bufferBody && f.requestBodyHandler != nil {
		f.stripFramingOnResume = true
	}

	if f.pausePendingCallout(w.calloutStarted) {
		// Callback has not fired yet; OnHttpCalloutDone will call flush(true).
		return shared.HeadersStatusStop
	}
	if f.async != nil {
		// Goroutine started; scheduler.Schedule will call flush(true) after fn returns.
		return shared.HeadersStatusStop
	}

	if endOfStream {
		// No body coming — synthesize a body call so requestBodyHandler sees EndStream.
		if f.requestBodyHandler != nil {
			f.requestBodyHandler(w, &BodyChunk{
				EndStream:       true,
				ContentType:     f.requestContentType,
				ContentEncoding: f.requestContentEncoding,
				Context:         &f.context,
			})
		}
		if f.pausePendingCallout(w.calloutStarted) {
			return shared.HeadersStatusStop
		}
		if f.async != nil {
			return shared.HeadersStatusStop
		}
		f.flushCompletedCallout(w.calloutStarted)
		f.flush(false)
		if f.stopped {
			return shared.HeadersStatusStop
		}
		return shared.HeadersStatusContinue
	}

	f.flushCompletedCallout(w.calloutStarted)
	f.flush(false)
	if f.stopped {
		return shared.HeadersStatusStop
	}
	return shared.HeadersStatusContinue
}

// OnRequestBody is called for each body data frame. Mutations queued during the
// body handler are applied by flush(false) before returning Continue, so they
// are visible to upstream before the request is forwarded.
//
// Body replacement flags are cleared after application so they cannot replay
// if another body frame arrives (buffered mode delivers all frames at endOfStream,
// but defensive clearing prevents confusion).
func (f *filter) OnRequestBody(body shared.BodyBuffer, endOfStream bool) shared.BodyStatus {
	if f.requestBodyHandler == nil {
		return shared.BodyStatusContinue
	}
	if f.bufferBody && !endOfStream {
		return shared.BodyStatusStopAndBuffer
	}

	var data []byte
	src := body
	if f.bufferBody {
		src = f.handle.BufferedRequestBody()
	}
	for _, chunk := range src.GetChunks() {
		data = append(data, chunk.ToBytes()...)
	}

	w := &Writer{f: f}
	f.requestBodyHandler(w, &BodyChunk{
		Data:            data,
		EndStream:       endOfStream,
		ContentType:     f.requestContentType,
		ContentEncoding: f.requestContentEncoding,
		Context:         &f.context,
	})
	if f.async != nil {
		// Goroutine started; flush(true) will run after it exits via scheduler.
		// Do not reset body replacement here — the goroutine may call SetRequestBody.
		return shared.BodyStatusStopAndBuffer
	}

	// Sync path: apply body replacement inline before flush.
	// Clear flags first so flush's async-body-replacement branch does not double-apply.
	if f.bufferBody && f.hasRequestBodyReplacement {
		buf := f.handle.BufferedRequestBody()
		buf.Drain(buf.GetSize())
		buf.Append(f.requestBodyReplacement)
		f.handle.RequestHeaders().Set("content-length", strconv.Itoa(len(f.requestBodyReplacement)))
		f.hasRequestBodyReplacement = false
		f.requestBodyReplacement = nil
	}
	f.flush(false) // apply header mutations and other queued state
	return shared.BodyStatusContinue
}

// OnHttpCalloutDone implements shared.HttpCalloutCallback. Envoy invokes it
// when an outbound callout response arrives. It may fire from a different
// goroutine (early synchronous callback) before OnRequestHeaders returns.
//
// streamDone guards against late callbacks after OnStreamComplete. The calloutFn
// nil-check guards against spurious duplicate deliveries.
//
// State machine:
//
//	Paused→Flushed  Normal path: stream was paused. flush(true) resumes it.
//	Active→Done     Early path: stream not yet paused. OnRequestHeaders detects
//	                Done and calls flush(false) before returning Continue.
func (f *filter) OnHttpCalloutDone(
	_ uint64,
	result shared.HttpCalloutResult,
	headers [][2]shared.UnsafeEnvoyBuffer,
	body []shared.UnsafeEnvoyBuffer,
) {
	if f.streamDone.Load() || f.calloutFn == nil {
		return
	}
	fn := f.calloutFn
	f.calloutFn = nil
	fn(HTTPCalloutResult(result), headers, body)
	if f.calloutState.CompareAndSwap(calloutStatePaused, calloutStateFlushed) {
		// Normal path: OnRequestHeaders already returned Stop. We own the resume.
		f.flush(true)
		return
	}
	// Early path: callback fired before OnRequestHeaders reached pausePendingCallout.
	// Mark Done; OnRequestHeaders will CAS Done→Flushed and call flush(false).
	f.calloutState.CompareAndSwap(calloutStateActive, calloutStateDone)
}

// pausePendingCallout returns true if a callout is in flight and the CAS
// Active→Paused succeeded. OnRequestHeaders should return Stop and wait for
// OnHttpCalloutDone. Returns false if no callout started or the callback
// already fired (state is Done — use flushCompletedCallout instead).
func (f *filter) pausePendingCallout(calloutStarted bool) bool {
	if !calloutStarted {
		return false
	}
	return f.calloutState.CompareAndSwap(calloutStateActive, calloutStatePaused)
}

// flushCompletedCallout handles the early-callback case: CAS Done→Flushed.
// Does NOT call flush itself — the caller calls flush(false) after this.
// This keeps flush as the single application point, avoiding a double-flush.
func (f *filter) flushCompletedCallout(calloutStarted bool) {
	if !calloutStarted {
		return
	}
	f.calloutState.CompareAndSwap(calloutStateDone, calloutStateFlushed)
}

// OnStreamComplete is called when Envoy terminates the stream. It prevents
// any in-flight callout from resuming a dead stream and cancels a running
// goroutine.
//
// Does NOT reset mutation queues: a goroutine may still be writing them at
// this point (cancel() unblocks ctx.Done() but the goroutine may not have
// returned yet). The goroutine sees done()==true and exits without calling
// flush, so the stale mutations are never applied.
func (f *filter) OnStreamComplete() {
	f.streamDone.Store(true)
	if f.async != nil {
		f.async.completeWithoutResume()
		f.async = nil
	}
}

// OnResponseHeaders runs the responseHandler against the upstream response.
// In buffered mode, content-length is stripped here; the correct value is
// written in OnResponseBody after any SetResponseBody replacement.
func (f *filter) OnResponseHeaders(headers shared.HeaderMap, endOfStream bool) shared.HeadersStatus {
	if f.responseHandler == nil {
		return shared.HeadersStatusContinue
	}

	f.responseContentType = headers.GetOne("content-type").ToString()
	f.responseContentEncoding = headers.GetOne("content-encoding").ToString()

	if f.bufferBody {
		headers.Remove("content-length")
	}

	statusCode := 0
	if v := headers.GetOne(":status"); v.Len > 0 {
		statusCode, _ = strconv.Atoi(v.ToString())
	}

	w := &Writer{f: f}
	f.responseHandler(w, &ResponseChunk{
		StatusCode:      statusCode,
		Headers:         headers,
		EndStream:       endOfStream,
		ContentEncoding: f.responseContentEncoding,
		ContentType:     f.responseContentType,
		Context:         &f.context,
	})

	if endOfStream {
		// Synthesize a body call for bodyless responses (204, HEAD, 304, etc.).
		f.responseHandler(w, &ResponseChunk{
			EndStream:       true,
			ContentEncoding: f.responseContentEncoding,
			ContentType:     f.responseContentType,
			Context:         &f.context,
		})
	}
	return shared.HeadersStatusContinue
}

// OnResponseBody applies any SetResponseBody replacement after the handler runs.
// Body replacement flags are cleared after application to prevent replay.
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

	w := &Writer{f: f}
	f.responseHandler(w, &ResponseChunk{
		Data:            data,
		EndStream:       endOfStream,
		ContentEncoding: f.responseContentEncoding,
		ContentType:     f.responseContentType,
		Context:         &f.context,
	})

	if f.bufferBody && f.hasResponseBodyReplacement {
		buf := f.handle.BufferedResponseBody()
		buf.Drain(buf.GetSize())
		buf.Append(f.responseBodyReplacement)
		f.handle.ResponseHeaders().Set("content-length", strconv.Itoa(len(f.responseBodyReplacement)))
		f.hasResponseBodyReplacement = false
		f.responseBodyReplacement = nil
	}
	return shared.BodyStatusContinue
}
