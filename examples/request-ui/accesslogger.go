// accesslogger.go: access logger half of the request-ui filter.
//
// The response handler deposits a partial record into PendingRecords when it
// sees the response headers (chunk.StatusCode != 0). This access logger pops
// it, enriches it with finalized stream fields, and calls Sink.Send so the
// record appears in the UI with correct duration, byte counts, and flags.
package requestui

import (
	"sync"

	"github.com/dio/transit/examples/request-ui/sink"
	"github.com/dio/transit/up"
)

// PendingRecords maps request ID to a partially filled Record waiting for
// finalized fields from the access logger. Exported so cmd/main.go can pass
// the same map to both the filter (via Register) and the access logger factory.
type PendingRecords struct {
	m sync.Map
}

// Store deposits a record for the access logger to consume.
func (p *PendingRecords) Store(requestID string, r *sink.Record) {
	if requestID != "" {
		p.m.Store(requestID, r)
	}
}

// LoadAndDelete retrieves and removes a record by request ID.
func (p *PendingRecords) LoadAndDelete(requestID string) (*sink.Record, bool) {
	val, ok := p.m.LoadAndDelete(requestID)
	if !ok {
		return nil, false
	}
	r, ok := val.(*sink.Record)
	return r, ok
}

// Delete removes a record, used by cleanup paths.
func (p *PendingRecords) Delete(requestID string) {
	p.m.Delete(requestID)
}

// RegisterLogger wires the access logger into the transit registry.
// Call from init() in cmd/main.go alongside Register.
func RegisterLogger(name string, s *sink.Sink, pending *PendingRecords) {
	up.RegisterAccessLogger(name, NewAccessLoggerFactory(pending, s))
}

// NewAccessLoggerFactory returns an access logger config factory that pops
// partial records from pending, enriches them with finalized stream fields,
// and sends them to the sink.
func NewAccessLoggerFactory(pending *PendingRecords, s *sink.Sink) up.AccessLoggerConfigFactory {
	return &alConfigFactory{pending: pending, sink: s}
}

type alConfigFactory struct {
	pending *PendingRecords
	sink    *sink.Sink
}

func (f *alConfigFactory) Create(_ up.AccessLoggerConfigHandle, _ []byte) (up.AccessLoggerFactory, error) {
	return &alFactory{pending: f.pending, sink: f.sink}, nil
}

type alFactory struct {
	pending *PendingRecords
	sink    *sink.Sink
}

func (f *alFactory) NewLogger() up.AccessLogger {
	return &alLogger{pending: f.pending, sink: f.sink}
}

func (f *alFactory) OnDestroy() {}

type alLogger struct {
	up.EmptyAccessLogger
	pending *PendingRecords
	sink    *sink.Sink
}

func (l *alLogger) OnLog(h up.AccessLoggerHandle, logType up.AccessLogType) {
	if logType != up.AccessLogTypeDownstreamEnd {
		return
	}

	v, ok := h.GetHeader(up.HttpHeaderTypeRequest, "x-request-id")
	if !ok {
		return
	}
	requestID := v.ToString()
	if requestID == "" {
		return
	}

	r, ok := l.pending.LoadAndDelete(requestID)
	if !ok {
		// DC case: client disconnected before response headers arrived.
		// The response handler never fired so no record was deposited.
		r = l.buildMinimalRecord(h, requestID)
		if r == nil {
			return
		}
	}

	// Enrich with finalized stream fields.
	timing := h.GetTimingInfo()
	if timing.RequestCompleteDurationNs >= 0 {
		r.DurationMs = float64(timing.RequestCompleteDurationNs) / 1e6
	}
	r.FirstUpstreamTxByteSentNs = timing.FirstUpstreamTxByteSentNs
	r.LastUpstreamRxByteReceivedNs = timing.LastUpstreamRxByteReceivedNs

	b := h.GetBytesInfo()
	r.RequestSizeBytes = float64(b.BytesReceived)
	r.ResponseSizeBytes = float64(b.BytesSent)
	r.WireBytesReceived = b.WireBytesReceived
	r.WireBytesSent = b.WireBytesSent

	if code := h.GetResponseCode(); code > 0 {
		r.ResponseCode = float64(code)
	}
	if v, ok := h.GetAttributeString(up.AttributeIDResponseCodeDetails); ok && v.Len > 0 {
		r.ResponseCodeDetails = v.ToString()
	}
	if flags := up.ResponseFlagsString(h.GetResponseFlags()); flags != "" {
		r.ResponseFlags = flags
	}
	if v, ok := h.GetAttributeString(up.AttributeIDUpstreamTransportFailureReason); ok && v.Len > 0 {
		r.UpstreamFailure = v.ToString()
	}

	if ns := h.GetUpstreamPoolReadyDurationNs(); ns >= 0 {
		r.UpstreamCxPoolReadyMs = float64(ns) / 1e6
	}
	r.UpstreamRequestAttempts = h.GetUpstreamRequestAttemptCount()

	if v, ok := h.GetAttributeString(up.AttributeIDUpstreamLocalAddress); ok && v.Len > 0 {
		r.UpstreamLocalAddress = v.ToString()
	}
	if v, ok := h.GetAttributeString(up.AttributeIDRequestProtocol); ok && v.Len > 0 {
		r.RequestProtocol = v.ToString()
	}

	if v, ok := h.GetTraceID(); ok && v.Len > 0 {
		r.TraceIDFinal = v.ToString()
	}
	if v, ok := h.GetSpanID(); ok && v.Len > 0 {
		r.SpanIDFinal = v.ToString()
	}
	r.TraceSampled = h.IsTraceSampled()

	if v, ok := h.GetLocalReplyBody(); ok && v.Len > 0 {
		r.LocalReplyBody = v.ToString()
	}

	r.HasError = hasError(r)
	l.sink.Send(r)
}

// buildMinimalRecord constructs a record for requests where the response
// handler never fired (e.g. client disconnect before upstream responded).
func (l *alLogger) buildMinimalRecord(h up.AccessLoggerHandle, requestID string) *sink.Record {
	r := &sink.Record{RequestID: requestID}
	if v, ok := h.GetHeader(up.HttpHeaderTypeRequest, ":method"); ok {
		r.Method = v.ToString()
	}
	if v, ok := h.GetHeader(up.HttpHeaderTypeRequest, ":path"); ok {
		r.Path = v.ToString()
	}
	if v, ok := h.GetHeader(up.HttpHeaderTypeRequest, ":authority"); ok {
		r.Host = v.ToString()
	}
	if v, ok := h.GetAttributeString(up.AttributeIDUpstreamAddress); ok && v.Len > 0 {
		r.UpstreamAddress = v.ToString()
	}
	return r
}
