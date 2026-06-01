// Package requestui is a transit filter that records every request and response
// into a Postgres database (or in-memory ring buffer) and serves a near-realtime
// web UI.
//
// Per-request state is initialized in the request handler (method, path, host,
// request id, trace ids, request headers), enriched in the response handler
// (status, response headers, response body), and finalized in the
// OnStreamFinalized callback where Envoy provides byte counts, durations,
// response flags, and other end-of-stream fields. The Record is sent to the
// Sink from OnStreamFinalized so a single delivery path covers every stream
// outcome — success, local reply, upstream failure, downstream disconnect.
package requestui

import (
	"encoding/json"
	"strings"
	"sync"

	"github.com/dio/transit/examples/request-ui/sink"
	"github.com/dio/transit/up"
)

const defaultMaxBody = 4096

var (
	cfgMu sync.RWMutex
	cfg   = filterConfig{
		RecordRequestHeaders:  true,
		RecordResponseHeaders: true,
		MaxBodyBytes:          defaultMaxBody,
	}
)

type filterConfig struct {
	RecordRequestHeaders  bool `json:"record_request_headers"`
	RecordResponseHeaders bool `json:"record_response_headers"`
	RecordRequestBody     bool `json:"record_request_body"`
	RecordResponseBody    bool `json:"record_response_body"`
	MaxBodyBytes          int  `json:"max_body_bytes"`
}

// reqState is the per-request accumulator stored in *Request.Context /
// *ResponseChunk.Context. The OnStreamFinalized callback builds a sink.Record
// from it plus the FinalizedInfo Envoy delivers at stream finalization.
type reqState struct {
	requestID      string
	method         string
	path           string
	host           string
	traceID        string
	spanID         string
	requestHeaders [][2]string

	statusCode      int
	responseHeaders [][2]string
	responseBody    string
}

var statePool = sync.Pool{New: func() any { return &reqState{} }}

// Register wires the filter into the transit registry. Call from init() in
// cmd/main.go after constructing the sink.
func Register(name string, s *sink.Sink) {
	up.Register(
		name,
		func(w *up.Writer, r *up.Request) {
			cfgMu.RLock()
			c := cfg
			cfgMu.RUnlock()

			st, ok := statePool.Get().(*reqState)
			if !ok || st == nil {
				st = &reqState{}
			}
			*st = reqState{}

			st.method = r.Method
			st.path = r.Path
			st.host = r.Host
			if v, ok := w.GetAttributeString(up.AttributeIDRequestId); ok {
				st.requestID = v.ToString()
			}
			if span := w.GetActiveSpan(); span != nil {
				if id, ok := span.GetTraceID(); ok {
					st.traceID = id.ToString()
				}
				if id, ok := span.GetSpanID(); ok {
					st.spanID = id.ToString()
				}
			}
			if c.RecordRequestHeaders {
				st.requestHeaders = r.AllHeaders()
			}
			*r.Context = st
		},
		up.WithResponse(func(_ *up.Writer, chunk *up.ResponseChunk) {
			cfgMu.RLock()
			c := cfg
			cfgMu.RUnlock()

			if *chunk.Context == nil {
				return
			}
			st, ok := (*chunk.Context).(*reqState)
			if !ok || st == nil {
				return
			}

			// Headers call: StatusCode != 0, Data == nil.
			if chunk.StatusCode != 0 {
				st.statusCode = chunk.StatusCode
				if c.RecordResponseHeaders {
					st.responseHeaders = chunk.AllHeaders()
				}
				return
			}

			// Body call: only reached when RecordResponseBody is true.
			if chunk.EndStream && len(chunk.Data) > 0 {
				data := chunk.Data
				if len(data) > c.MaxBodyBytes {
					data = data[:c.MaxBodyBytes]
				}
				st.responseBody = string(data)
			}
		}),
		up.WithOnStreamFinalized(func(ctx *any, info up.FinalizedInfo) {
			if ctx == nil || *ctx == nil {
				return
			}
			st, ok := (*ctx).(*reqState)
			if !ok || st == nil {
				return
			}

			r := buildRecord(st)
			enrichWithFinalized(r, info)
			r.HasError = hasError(r)
			s.Send(r)

			*st = reqState{}
			statePool.Put(st)
			*ctx = nil
		}),
	)
}

// buildRecord constructs a Record from the per-request accumulator. Finalized
// fields are filled separately by enrichWithFinalized.
func buildRecord(st *reqState) *sink.Record {
	r := &sink.Record{
		RequestID:      st.requestID,
		Method:         st.method,
		Path:           st.path,
		Host:           st.host,
		TraceID:        st.traceID,
		SpanID:         st.spanID,
		ResponseBody:   st.responseBody,
		ResponseCode:   float64(st.statusCode),
		UpstreamStatus: statusStr(st.statusCode),
	}
	if len(st.requestHeaders) > 0 {
		if b, err := json.Marshal(st.requestHeaders); err == nil {
			r.RequestHeaders = string(b)
		}
	}
	if len(st.responseHeaders) > 0 {
		if b, err := json.Marshal(st.responseHeaders); err == nil {
			r.ResponseHeaders = string(b)
		}
	}
	return r
}

// enrichWithFinalized copies finalized stream fields from info into r.
func enrichWithFinalized(r *sink.Record, info up.FinalizedInfo) {
	if info.Timing.RequestCompleteDurationNs >= 0 {
		r.DurationMs = float64(info.Timing.RequestCompleteDurationNs) / 1e6
	}
	r.FirstUpstreamTxByteSentNs = info.Timing.FirstUpstreamTxByteSentNs
	r.LastUpstreamRxByteReceivedNs = info.Timing.LastUpstreamRxByteReceivedNs

	r.RequestSizeBytes = float64(info.Bytes.BytesReceived)
	r.ResponseSizeBytes = float64(info.Bytes.BytesSent)
	r.WireBytesReceived = info.Bytes.WireBytesReceived
	r.WireBytesSent = info.Bytes.WireBytesSent

	// Use the finalized response code when the response handler never saw
	// headers (DC, upstream failure, local reply).
	if r.ResponseCode == 0 && info.ResponseCode > 0 {
		r.ResponseCode = float64(info.ResponseCode)
		r.UpstreamStatus = statusStr(int(info.ResponseCode))
	}
	r.ResponseCodeDetails = info.ResponseCodeDetails
	if flags := up.ResponseFlagsString(info.ResponseFlags); flags != "" {
		r.ResponseFlags = flags
	}
	r.UpstreamFailure = info.UpstreamFailure

	if info.UpstreamPoolReadyDurationNs >= 0 {
		r.UpstreamCxPoolReadyMs = float64(info.UpstreamPoolReadyDurationNs) / 1e6
	}
	r.UpstreamRequestAttempts = info.UpstreamRequestAttempts
	r.UpstreamLocalAddress = info.UpstreamLocalAddress
	r.UpstreamAddress = info.UpstreamAddress
	r.RequestProtocol = info.RequestProtocol

	r.TraceIDFinal = info.TraceID
	r.SpanIDFinal = info.SpanID
	r.TraceSampled = info.TraceSampled

	r.LocalReplyBody = info.LocalReplyBody
}

func hasError(r *sink.Record) bool {
	return r.ErrorDetails != "" ||
		r.UpstreamFailure != "" ||
		(r.ResponseFlags != "" && containsErrorFlag(r.ResponseFlags)) ||
		r.ResponseCode >= 500
}

var errorFlags = map[string]bool{
	up.ResponseFlagUpstreamConnectionFailure:     true,
	up.ResponseFlagNoHealthyUpstream:             true,
	up.ResponseFlagUpstreamConnectionTermination: true,
	up.ResponseFlagUpstreamRequestTimeout:        true,
	up.ResponseFlagUpstreamOverflow:              true,
	up.ResponseFlagNoRouteFound:                  true,
}

func containsErrorFlag(flags string) bool {
	for token := range strings.SplitSeq(flags, ",") {
		if errorFlags[token] {
			return true
		}
	}
	return false
}

func statusStr(code int) string {
	if code == 0 {
		return ""
	}
	b := make([]byte, 0, 3)
	for code > 0 {
		b = append([]byte{byte(code%10) + '0'}, b...)
		code /= 10
	}
	return string(b)
}
