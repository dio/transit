package up

import (
	"crypto/rand"
	"encoding/hex"
	"sync"

	"github.com/envoyproxy/envoy/source/extensions/dynamic_modules/sdk/go/shared"
)

// streamFinalizedEntry holds the per-stream wiring used by the SDK-internal
// access logger to deliver [FinalizedInfo] back to the filter that asked for
// it via [WithOnStreamFinalized].
//
// streamObjectNonce is a pointer into the filter's streamObjectNonce field.
// It is read by the access logger after all filter callbacks complete, at
// which point the nonce is final. After entry.fn fires, the access logger
// drains the Primitive A bag so finalized cleanup runs before the SDK removes
// stream-scoped objects. Drain order:
//
//	user OnStreamComplete → user OnStreamFinalized → dropBag
type streamFinalizedEntry struct {
	fn                OnStreamFinalizedFunc
	ctx               *any
	streamObjectNonce *string // pointer into filter.streamObjectNonce; may point to ""
	localReplyBody    *string // pointer into filter.localReplyBody; may point to ""
}

// streamFinalizedRegistry is a process-wide map keyed by (filter name,
// stream key). Entries are deposited by the filter on its first request
// callback and drained by the SDK-internal access logger at
// [AccessLogTypeDownstreamEnd]. [filter.OnStreamComplete] drains any
// leftover entry as a safety net.
//
// Composite key form: name + "\x00" + streamKey. The filter name disambiguates
// concurrent filters that share an x-request-id (multiple filter chains, or
// the same request id flowing through more than one named filter).
var (
	streamFinalizedMu      sync.Mutex
	streamFinalizedEntries = map[string]*streamFinalizedEntry{}
)

func streamFinalizedKey(name, streamKey string) string {
	return name + "\x00" + streamKey
}

// putFinalizedEntry stores e under key. If an entry already exists (e.g. the
// caller retried registration after a transient miss), it is replaced.
func putFinalizedEntry(key string, e *streamFinalizedEntry) {
	streamFinalizedMu.Lock()
	streamFinalizedEntries[key] = e
	streamFinalizedMu.Unlock()
}

// takeFinalizedEntry atomically reads and deletes the entry for key. Used by
// the SDK-internal access logger to consume the entry exactly once.
func takeFinalizedEntry(key string) (*streamFinalizedEntry, bool) {
	streamFinalizedMu.Lock()
	e, ok := streamFinalizedEntries[key]
	if ok {
		delete(streamFinalizedEntries, key)
	}
	streamFinalizedMu.Unlock()
	return e, ok
}

// dropFinalizedEntry removes the entry for key without invoking the callback.
// Called by [filter.OnStreamComplete] as a safety net in case the access
// logger never fired (e.g. listener YAML lacks the access_log stanza).
func dropFinalizedEntry(key string) {
	streamFinalizedMu.Lock()
	delete(streamFinalizedEntries, key)
	streamFinalizedMu.Unlock()
}

// finalizedFallbackKey is a 16-byte hex nonce used when the stream has no
// request id (or generate_request_id is disabled). The same nonce is written
// into a request header read by the access logger.
const finalizedFallbackKeyHeader = "x-transit-stream-id"

func mintFallbackStreamKey() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

// registerFinalized is called by [filter.OnRequestHeaders] after the user
// handler runs. It captures the request id (or mints a fallback) and deposits
// (fn, ctx*) into the registry so the SDK-internal access logger can find it
// at finalize time.
//
// Idempotent across multiple callbacks on the same stream: the filter only
// registers once per stream (finalizedKey is non-empty after the first call).
func (f *filter) registerFinalized() {
	if f.onStreamFinalized == nil || f.finalizedKey != "" {
		return
	}
	streamKey := ""
	if v, ok := f.handle.GetAttributeString(shared.AttributeIDRequestId); ok && v.Len > 0 {
		streamKey = v.ToString()
	}
	if streamKey == "" {
		// Fallback: mint a nonce and stamp it into a request header that the
		// access logger can read. This is an internal correlation header, so
		// write it immediately while still on the worker thread. If this were
		// queued behind a user SendLocalResponse, flush would return after
		// sending the local reply and the access logger would never see it.
		streamKey = mintFallbackStreamKey()
		f.handle.RequestHeaders().Set(finalizedFallbackKeyHeader, streamKey)
	}
	key := streamFinalizedKey(f.name, streamKey)
	f.finalizedKey = key
	putFinalizedEntry(key, &streamFinalizedEntry{
		fn:                f.onStreamFinalized,
		ctx:               &f.context,
		streamObjectNonce: &f.streamObjectNonce,
		localReplyBody:    &f.localReplyBody,
	})
}

// registerStreamFinalizedAccessLogger registers an internal access-logger
// config factory under the same name as the filter. The factory reads the
// stream key (x-request-id, or the fallback header) at OnLog time and looks
// up the entry deposited by the filter.
//
// Called from [Register] when [WithOnStreamFinalized] is set. The listener
// Envoy YAML must include an access_log entry pointing at this dynamic
// module with logger_name equal to the filter name.
func registerStreamFinalizedAccessLogger(name string) {
	registerAccessLogger(name, &finalizedLoggerConfigFactory{name: name})
}

type finalizedLoggerConfigFactory struct{ name string }

func (f *finalizedLoggerConfigFactory) Create(_ AccessLoggerConfigHandle, _ []byte) (AccessLoggerFactory, error) {
	return &finalizedLoggerFactory{name: f.name}, nil
}

type finalizedLoggerFactory struct{ name string }

func (f *finalizedLoggerFactory) NewLogger() AccessLogger {
	return &finalizedLogger{name: f.name}
}

func (f *finalizedLoggerFactory) OnDestroy() {}

type finalizedLogger struct {
	EmptyAccessLogger
	name string
}

func (l *finalizedLogger) OnLog(h AccessLoggerHandle, logType AccessLogType) {
	if logType != AccessLogTypeDownstreamEnd {
		return
	}
	streamKey := ""
	if v, ok := h.GetHeader(HttpHeaderTypeRequest, "x-request-id"); ok && v.Len > 0 {
		streamKey = v.ToString()
	}
	if streamKey == "" {
		if v, ok := h.GetHeader(HttpHeaderTypeRequest, finalizedFallbackKeyHeader); ok && v.Len > 0 {
			streamKey = v.ToString()
		}
	}
	if streamKey == "" {
		return
	}
	entry, ok := takeFinalizedEntry(streamFinalizedKey(l.name, streamKey))
	if !ok {
		return
	}

	info := FinalizedInfo{
		Timing:                      h.GetTimingInfo(),
		Bytes:                       h.GetBytesInfo(),
		ResponseCode:                h.GetResponseCode(),
		ResponseFlags:               h.GetResponseFlags(),
		UpstreamPoolReadyDurationNs: h.GetUpstreamPoolReadyDurationNs(),
		UpstreamRequestAttempts:     h.GetUpstreamRequestAttemptCount(),
		TraceSampled:                h.IsTraceSampled(),
	}
	if v, ok := h.GetAttributeString(AttributeIDResponseCodeDetails); ok && v.Len > 0 {
		info.ResponseCodeDetails = v.ToString()
	}
	if v, ok := h.GetAttributeString(AttributeIDUpstreamTransportFailureReason); ok && v.Len > 0 {
		info.UpstreamFailure = v.ToString()
	}
	if v, ok := h.GetAttributeString(AttributeIDUpstreamLocalAddress); ok && v.Len > 0 {
		info.UpstreamLocalAddress = v.ToString()
	}
	if v, ok := h.GetAttributeString(AttributeIDUpstreamAddress); ok && v.Len > 0 {
		info.UpstreamAddress = v.ToString()
	}
	if v, ok := h.GetAttributeString(AttributeIDRequestProtocol); ok && v.Len > 0 {
		info.RequestProtocol = v.ToString()
	}
	if v, ok := h.GetTraceID(); ok && v.Len > 0 {
		info.TraceID = v.ToString()
	}
	if v, ok := h.GetSpanID(); ok && v.Len > 0 {
		info.SpanID = v.ToString()
	}
	if v, ok := h.GetLocalReplyBody(); ok && v.Len > 0 {
		info.LocalReplyBody = v.ToString()
	} else if entry.localReplyBody != nil {
		info.LocalReplyBody = *entry.localReplyBody
	}

	entry.fn(entry.ctx, info)
	// Drain the Primitive A bag AFTER the user callback runs so that the
	// finalized callback runs before the SDK removes stream-scoped objects.
	// This establishes the guaranteed order:
	//   user OnStreamComplete → user OnStreamFinalized → dropBag
	if entry.streamObjectNonce != nil {
		dropBag(*entry.streamObjectNonce)
	}
}
