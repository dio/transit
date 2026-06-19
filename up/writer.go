package up

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync"

	"github.com/envoyproxy/envoy/source/extensions/dynamic_modules/sdk/go/shared"
)

// calloutState tracks the handoff between a request callback and OnHttpCalloutDone.
//
// Background: Envoy does not guarantee that OnHttpCalloutDone fires after
// the initiating request callback returns. HttpCallout can invoke the callback
// synchronously — before Transit has had a chance to return Stop. This
// means we have two possible orderings for every callout:
//
//	Normal (async) path — request callback returns Stop BEFORE callback fires:
//	  1. HTTPCallout() sets calloutStarted=true, state=Active.
//	  2. HttpCallout() returns; the callback has NOT fired yet.
//	  3. request callback calls pausePendingCallout → CAS Active→Paused succeeds.
//	  4. request callback returns Stop. Envoy pauses the stream.
//	  5. Later: OnHttpCalloutDone fires. CAS Paused→Flushed succeeds.
//	     flush(true) is called → ContinueRequest resumes the stream.
//
//	Early (synchronous) path — callback fires INSIDE HttpCallout:
//	  1. HTTPCallout() sets calloutStarted=true, state=Active.
//	  2. HttpCallout() invokes the callback synchronously before returning.
//	  3. OnHttpCalloutDone fires. CAS Paused→Flushed fails (state is Active).
//	     CAS Active→Done succeeds. Mutations are queued. Callback returns.
//	  4. HttpCallout() returns. HTTPCallout() returns.
//	  5. request callback calls pausePendingCallout → CAS Active→Paused fails
//	     (state is Done). pausePendingCallout returns false.
//	  6. request callback calls flushCompletedCallout → CAS Done→Flushed succeeds.
//	     flush(false) applies mutations. NO ContinueRequest — the callback
//	     returns Continue itself. Stream was never paused.
//
// The four states form a one-way ratchet; transitions never go backwards.
// No mutex is needed because the two competing parties (the initiating request
// callback on the worker thread, and OnHttpCalloutDone which may race it) each
// attempt exactly one CAS and only one wins.
const (
	// calloutStateActive is the initial state: HTTPCallout has been called but
	// neither side has yet claimed ownership of the flush.
	calloutStateActive int32 = iota

	// calloutStatePaused means the request callback won: it returned Stop and is
	// waiting for OnHttpCalloutDone to call flush(true) + ContinueRequest.
	calloutStatePaused

	// calloutStateDone means OnHttpCalloutDone won the early path: the callback
	// fired before the request callback checked. That callback will detect this
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
// See Do for goroutine-safety rules when using w.Go and fan-out.
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

	// goStarted is set when w.Go is called and remains true for the Writer's
	// lifetime. It is sticky — unlike f.goStarted which clears after the goroutine
	// finishes — so that HTTPCallout can reliably panic even if the goroutine
	// completes before the handler checks it.
	goStarted bool

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

// RequestHeaders returns all current request headers as copied Go strings.
// Header mutations queued earlier in the same callback are reflected in the
// returned view, even though they are applied to Envoy only when the callback
// flushes.
func (w *Writer) RequestHeaders() [][2]string {
	if w.f == nil || w.f.handle == nil {
		return nil
	}
	raw := w.f.handle.RequestHeaders().GetAll()
	out := make([][2]string, len(raw))
	for i, h := range raw {
		out[i] = [2]string{h[0].ToString(), h[1].ToString()}
	}
	for _, m := range w.f.reqHeaders {
		out = applyRequestHeaderMutation(out, m)
	}
	return out
}

// RequestHeader returns the first current value of the named request header, or
// "" if absent. Header mutations queued earlier in the same callback are
// reflected in the returned value.
func (w *Writer) RequestHeader(name string) string {
	for _, h := range w.RequestHeaders() {
		if strings.EqualFold(h[0], name) {
			return h[1]
		}
	}
	return ""
}

func applyRequestHeaderMutation(headers [][2]string, m requestHeaderMutation) [][2]string {
	if m.del {
		return removeRequestHeader(headers, m.name)
	}
	if m.add {
		return append(headers, [2]string{m.name, m.value})
	}
	headers = removeRequestHeader(headers, m.name)
	return append(headers, [2]string{m.name, m.value})
}

func removeRequestHeader(headers [][2]string, name string) [][2]string {
	out := headers[:0]
	for _, h := range headers {
		if !strings.EqualFold(h[0], name) {
			out = append(out, h)
		}
	}
	return out
}

// Slog returns a [*slog.Logger] whose handler routes through Envoy's logging
// mechanism. The logger automatically prepends filter=<name> to every line,
// followed by any attributes registered via [WithAttributes]. Call once at the
// top of a handler and reuse the result rather than calling Slog repeatedly.
func (w *Writer) Slog() *slog.Logger {
	return slog.New(&writerHandler{w: w, attrs: w.f.logAttrs})
}

// AddLogAttrs appends key-value pairs to the per-request log context.
//
// When [WithLogMetadata] is configured, each attr is also written to
// dynamic metadata under the filter's namespace so it is accessible from
// the Envoy access log via %DYNAMIC_METADATA(ns:key)%. Value types that
// Envoy's metadata store does not natively support (structs, slices, etc.)
// are serialised to their string representation rather than panicking.
//
// By default the attrs are NOT printed inline in the process-log message;
// they flow to the access log through metadata. Use [WithInlineLogAttrs] to
// also print them inline (useful for local debugging without a JSON access log).
//
// For a one-off derived logger without persisting attrs on the writer, use
// w.Slog().With(kvs...) directly instead.
func (w *Writer) AddLogAttrs(kvs ...any) {
	attrs := argsToAttrs(kvs)
	w.f.reqLogAttrs = append(w.f.reqLogAttrs, attrs...)
	if ns := w.f.logMetadataNS; ns != "" {
		for _, a := range attrs {
			w.SetMetadata(ns, a.Key, attrToMetadataValue(a))
		}
	}
}

// attrToMetadataValue extracts a metadata-safe value from a slog.Attr.
// Numeric, string, and bool kinds are returned as their native Go types.
// All other kinds (Duration, Time, Group, Any with unsupported type) fall
// back to the slog string representation — never panics.
func attrToMetadataValue(a slog.Attr) any {
	v := a.Value.Resolve()
	switch v.Kind() {
	case slog.KindString:
		return v.String()
	case slog.KindBool:
		return v.Bool()
	case slog.KindInt64:
		return v.Int64()
	case slog.KindUint64:
		return v.Uint64()
	case slog.KindFloat64:
		return v.Float64()
	case slog.KindAny:
		switch raw := v.Any().(type) {
		case string, bool,
			int, int8, int16, int32, int64,
			uint, uint8, uint16, uint32, uint64,
			float32, float64:
			return raw
		default:
			_ = raw
			return v.String()
		}
	default:
		return v.String()
	}
}

// writerHandler is the slog.Handler backing Writer.Slog.
type writerHandler struct {
	w     *Writer
	attrs []slog.Attr
	group string
}

func (h *writerHandler) Enabled(_ context.Context, _ slog.Level) bool { return true }

func (h *writerHandler) Handle(_ context.Context, r slog.Record) error {
	var b strings.Builder
	b.WriteString(r.Message)
	b.WriteString(" filter=")
	b.WriteString(h.w.f.name)
	if id := h.w.f.reqID; id != "" {
		b.WriteString(" request_id=")
		b.WriteString(id)
	}
	writeAttrs := func(attrs []slog.Attr) {
		for _, a := range attrs {
			b.WriteByte(' ')
			b.WriteString(a.Key)
			b.WriteByte('=')
			b.WriteString(a.Value.String())
		}
	}
	writeAttrs(h.attrs)
	r.Attrs(func(a slog.Attr) bool {
		b.WriteByte(' ')
		if h.group != "" {
			b.WriteString(h.group)
			b.WriteByte('.')
		}
		b.WriteString(a.Key)
		b.WriteByte('=')
		b.WriteString(a.Value.String())
		return true
	})
	h.w.f.handle.Log(shared.LogLevel(slogLevelToUp(r.Level)), "%s", b.String())
	return nil
}

func (h *writerHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	merged := make([]slog.Attr, len(h.attrs)+len(attrs))
	copy(merged, h.attrs)
	copy(merged[len(h.attrs):], attrs)
	return &writerHandler{w: h.w, attrs: merged, group: h.group}
}

func (h *writerHandler) WithGroup(name string) slog.Handler {
	return &writerHandler{w: h.w, attrs: h.attrs, group: name}
}

func slogLevelToUp(l slog.Level) LogLevel {
	switch {
	case l >= slog.LevelError+4:
		return LogCritical
	case l >= slog.LevelError:
		return LogError
	case l >= slog.LevelWarn:
		return LogWarn
	case l >= slog.LevelInfo:
		return LogInfo
	case l >= slog.LevelDebug:
		return LogDebug
	default:
		return LogTrace
	}
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
			w.f.localReplyBody = string(body)
			w.f.localReply = &localResponse{status: uint32(status), headers: headers, body: body}
		}
		w.f.stopped = true
		return
	}
	if w.f.stopped {
		return
	}
	w.f.localReplyBody = string(body)
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

// GetFilterState reads a string filter state value previously written by SetFilterState.
// Returns the empty string and false if the key is absent.
func (w *Writer) GetFilterState(key string) (string, bool) {
	buf, ok := w.f.handle.GetFilterState(key)
	if !ok {
		return "", false
	}
	return buf.ToString(), true
}

// SetStreamObject stores v under key in the per-stream typed-value bag
// (Primitive A). Must be called on the worker thread — same constraint as
// SetFilterState.
//
// On the first call for a stream, getOrCreateBag mints a short random nonce,
// queues a SetFilterState write of that nonce under the reserved key
// "up.stream_object_id" (via the same queued-mutation path as SetFilterState),
// and creates the bag. Subsequent calls for the same stream reuse the bag.
//
// The nonce in filter state lets ClusterLBContext.GetStreamObject look up the
// bag. The bag is drained by OnStreamComplete (or finalizedLogger.OnLog for
// filters using WithOnStreamFinalized) after all user callbacks have run.
func (w *Writer) SetStreamObject(key string, v any) {
	bag := getOrCreateBag(w)
	bag.Set(key, v)
}

// GetStreamObject returns the value stored under key for this stream, or
// (nil, false) if the key was never set. Must be called on the worker thread.
// If SetStreamObject was never called on this stream no allocation occurs and
// no nonce is minted.
func (w *Writer) GetStreamObject(key string) (any, bool) {
	if w.f.streamObjectNonce == "" {
		nonce, ok := w.GetFilterState(streamObjectIDKey)
		if !ok || nonce == "" {
			return nil, false
		}
		w.f.streamObjectNonce = nonce
	}
	bag, ok := lookupBag(w.f.streamObjectNonce)
	if !ok {
		return nil, false
	}
	return bag.Get(key)
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
// Only effective in mutable buffered request mode ([WithMutableBody]).
func (w *Writer) SetRequestBody(data []byte) {
	if !w.f.mutableRequestBody {
		return
	}
	w.f.requestBodyReplacement = data
	w.f.hasRequestBodyReplacement = true
}

// SetResponseBody marks data as the replacement for the response body buffer.
// Only effective in buffered mode ([WithMutableBody]).
func (w *Writer) SetResponseBody(data []byte) {
	w.f.responseBodyReplacement = data
	w.f.hasResponseBodyReplacement = true
}

// IncrementCounter queues (or immediately applies) a counter increment.
func (w *Writer) IncrementCounter(id MetricID, delta uint64) {
	w.IncrementCounterLabels(id, delta)
}

// IncrementCounterLabels queues (or immediately applies) a counter increment
// with label values matching the tag keys used when the counter was defined.
func (w *Writer) IncrementCounterLabels(id MetricID, delta uint64, labelValues ...string) {
	if w.queued() {
		w.f.counters = append(w.f.counters, counterMutation{id: id, delta: delta, labels: cloneLabels(labelValues)})
		return
	}
	w.f.handle.IncrementCounterValue(shared.MetricID(id), delta, labelValues...)
}

// IncrementGauge queues (or immediately applies) a gauge increment.
func (w *Writer) IncrementGauge(id MetricID, delta uint64) {
	if w.queued() {
		w.f.gauges = append(w.f.gauges, gaugeMutation{id: id, value: delta, op: gaugeOpIncrement})
		return
	}
	w.f.handle.IncrementGaugeValue(shared.MetricID(id), delta)
}

// DecrementGauge queues (or immediately applies) a gauge decrement.
func (w *Writer) DecrementGauge(id MetricID, delta uint64) {
	if w.queued() {
		w.f.gauges = append(w.f.gauges, gaugeMutation{id: id, value: delta, op: gaugeOpDecrement})
		return
	}
	w.f.handle.DecrementGaugeValue(shared.MetricID(id), delta)
}

// SetGauge queues (or immediately applies) a gauge absolute assignment.
func (w *Writer) SetGauge(id MetricID, value uint64) {
	if w.queued() {
		w.f.gauges = append(w.f.gauges, gaugeMutation{id: id, value: value, op: gaugeOpSet})
		return
	}
	w.f.handle.SetGaugeValue(shared.MetricID(id), value)
}

// RecordHistogram queues (or immediately applies) a histogram observation.
func (w *Writer) RecordHistogram(id MetricID, value uint64) {
	w.RecordHistogramLabels(id, value)
}

// RecordHistogramLabels queues (or immediately applies) a histogram
// observation with label values matching the tag keys used when the histogram
// was defined.
func (w *Writer) RecordHistogramLabels(id MetricID, value uint64, labelValues ...string) {
	if w.queued() {
		w.f.histograms = append(w.f.histograms, histogramMutation{id: id, value: value, labels: cloneLabels(labelValues)})
		return
	}
	w.f.handle.RecordHistogramValue(shared.MetricID(id), value, labelValues...)
}

func cloneLabels(labels []string) []string {
	if len(labels) == 0 {
		return nil
	}
	return append([]string(nil), labels...)
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
	if w.goStarted {
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
	w.goStarted = true
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

// HTTPCallout initiates an outbound Envoy HTTP callout from a request callback
// and pauses the request until the callout completes. fn is invoked with the
// callout result; it may queue mutations or call SendLocalResponse.
//
// Returns HTTPCalloutInitSuccess and nil error if Envoy accepted the callout.
// A non-nil error means fn will never be called.
//
// Panics if called after Go or after a previous HTTPCallout.
func (w *Writer) HTTPCallout(req HTTPCalloutRequest, fn HTTPCalloutFunc) (HTTPCalloutInitResult, error) {
	if w.calloutStarted || w.goStarted {
		panic("up: HTTPCallout cannot be started after Go or another HTTPCallout")
	}
	// Set calloutFn and calloutStarted BEFORE calling handle.HttpCallout.
	// handle.HttpCallout may invoke the callback synchronously. If we set these
	// fields after, the synchronous callback would fire with calloutFn == nil.
	w.f.calloutFn = fn
	w.calloutStarted = true
	w.f.calloutState.Store(calloutStateActive)
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

// HTTPCalloutAllSettled initiates multiple outbound Envoy HTTP callouts from a
// request callback and pauses the request until all accepted callouts complete.
// fn is invoked exactly once with one response slot per request; init failures
// are recorded in their corresponding response slot.
//
// Response headers and body buffers are Go-owned copies safe to read after
// individual callout callbacks return. fn may queue mutations or call
// SendLocalResponse.
//
// Panics if called after Go or after another HTTPCallout/HTTPCalloutAllSettled.
func (w *Writer) HTTPCalloutAllSettled(reqs []HTTPCalloutRequest, fn HTTPCalloutAllSettledFunc) error {
	if w.calloutStarted || w.goStarted {
		panic("up: HTTPCalloutAllSettled cannot be started after Go or another HTTPCallout")
	}
	if fn == nil {
		panic("up: HTTPCalloutAllSettled called with nil callback")
	}
	if len(reqs) == 0 {
		fn(nil)
		return nil
	}

	w.calloutStarted = true
	w.f.calloutState.Store(calloutStateActive)

	b := &allSettledBatch{
		f:         w.f,
		fn:        fn,
		responses: make([]HTTPCalloutAllSettledResponse, len(reqs)),
		remaining: len(reqs),
	}
	for i, req := range reqs {
		init, _ := w.f.handle.HttpCallout(
			req.Cluster,
			req.Headers,
			req.Body,
			req.TimeoutMillis,
			doCallbackFunc(func(_ uint64, r HTTPCalloutResult, headers [][2]shared.UnsafeEnvoyBuffer, body []shared.UnsafeEnvoyBuffer) {
				b.finish(i, HTTPCalloutAllSettledResponse{
					Init:    HTTPCalloutInitSuccess,
					Result:  r,
					Err:     errCalloutResult(r),
					Headers: copyUnsafeEnvoyHeaderBuffers(headers),
					Body:    copyUnsafeEnvoyBuffers(body),
				})
			}),
		)
		if calloutInit := HTTPCalloutInitResult(init); calloutInit != HTTPCalloutInitSuccess {
			b.finish(i, HTTPCalloutAllSettledResponse{
				Init: calloutInit,
				Err:  errCalloutInitResult(calloutInit),
			})
		}
	}
	return nil
}

type allSettledBatch struct {
	f *filter

	mu        sync.Mutex
	fn        HTTPCalloutAllSettledFunc
	responses []HTTPCalloutAllSettledResponse
	remaining int
	flushed   bool
}

func (b *allSettledBatch) finish(i int, resp HTTPCalloutAllSettledResponse) {
	b.mu.Lock()
	if b.flushed {
		b.mu.Unlock()
		return
	}
	b.responses[i] = resp
	b.remaining--
	if b.remaining > 0 {
		b.mu.Unlock()
		return
	}
	b.flushed = true
	responses := append([]HTTPCalloutAllSettledResponse(nil), b.responses...)
	fn := b.fn
	b.mu.Unlock()

	if b.f.streamDone.Load() {
		return
	}
	fn(responses)
	if b.f.streamDone.Load() {
		return
	}
	if b.f.calloutState.CompareAndSwap(calloutStatePaused, calloutStateFlushed) {
		b.f.flush(true)
		return
	}
	b.f.calloutState.CompareAndSwap(calloutStateActive, calloutStateDone)
}

// HTTPCalloutSequence initiates outbound Envoy HTTP callouts one at a time from
// a request callback and pauses the request until done runs. next receives the
// previous response and decides whether to issue another callout. done runs from
// the callback path, so SendLocalResponse is reliable there.
//
// Panics if called after Go or after another HTTPCallout/HTTPCalloutAllSettled.
func (w *Writer) HTTPCalloutSequence(next HTTPCalloutSequenceNextFunc, done HTTPCalloutSequenceDoneFunc) error {
	if w.calloutStarted || w.goStarted {
		panic("up: HTTPCalloutSequence cannot be started after Go or another HTTPCallout")
	}
	if next == nil {
		panic("up: HTTPCalloutSequence called with nil next callback")
	}
	if done == nil {
		panic("up: HTTPCalloutSequence called with nil done callback")
	}
	w.calloutStarted = true
	w.f.calloutState.Store(calloutStateActive)

	s := &calloutSequence{f: w.f, next: next, done: done}
	s.step(nil)
	return nil
}

type calloutSequence struct {
	f *filter

	next      HTTPCalloutSequenceNextFunc
	done      HTTPCalloutSequenceDoneFunc
	responses []HTTPCalloutAllSettledResponse
	finished  bool
}

func (s *calloutSequence) step(previous *HTTPCalloutAllSettledResponse) {
	if s.finished || s.f.streamDone.Load() {
		return
	}
	req, ok := s.next(len(s.responses), previous)
	if !ok {
		s.finish()
		return
	}
	init, _ := s.f.handle.HttpCallout(
		req.Cluster,
		req.Headers,
		req.Body,
		req.TimeoutMillis,
		doCallbackFunc(func(_ uint64, r HTTPCalloutResult, headers [][2]shared.UnsafeEnvoyBuffer, body []shared.UnsafeEnvoyBuffer) {
			resp := HTTPCalloutAllSettledResponse{
				Init:    HTTPCalloutInitSuccess,
				Result:  r,
				Err:     errCalloutResult(r),
				Headers: copyUnsafeEnvoyHeaderBuffers(headers),
				Body:    copyUnsafeEnvoyBuffers(body),
			}
			s.responses = append(s.responses, resp)
			s.step(&s.responses[len(s.responses)-1])
		}),
	)
	if calloutInit := HTTPCalloutInitResult(init); calloutInit != HTTPCalloutInitSuccess {
		resp := HTTPCalloutAllSettledResponse{
			Init: calloutInit,
			Err:  errCalloutInitResult(calloutInit),
		}
		s.responses = append(s.responses, resp)
		s.step(&s.responses[len(s.responses)-1])
	}
}

func (s *calloutSequence) finish() {
	if s.finished || s.f.streamDone.Load() {
		return
	}
	s.finished = true
	responses := append([]HTTPCalloutAllSettledResponse(nil), s.responses...)
	s.done(responses)
	if s.f.streamDone.Load() {
		return
	}
	if s.f.calloutState.CompareAndSwap(calloutStatePaused, calloutStateFlushed) {
		s.f.flush(true)
		return
	}
	s.f.calloutState.CompareAndSwap(calloutStateActive, calloutStateDone)
}

// Do performs an Envoy HTTP callout from inside a Go goroutine and blocks
// until the callout completes or ctx is cancelled.
//
// Multiple Do calls may be in flight concurrently. Writer mutation methods
// (SetRequestHeader, SetFilterState, etc.) are NOT goroutine-safe and must not be
// called while any Do goroutine is still running. Join all fan-out goroutines before
// issuing any mutations; the race detector will catch violations if this rule is broken.
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
