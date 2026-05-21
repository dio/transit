// Package requestui is a transit filter that records every request and response
// into a Postgres database (or in-memory ring buffer) and serves a near-realtime
// web UI.
//
// Per-request state is initialized in the response handler's headers call
// (when chunk.StatusCode != 0) and accumulated through body chunks until
// EndStream. Request-side fields (method, path, host) come from attributes on
// the Writer. The ResponseChunk.Context slot carries per-request state across
// callbacks.
//
// Correlation with the access logger: the response handler deposits a partial
// Record into PendingRecords keyed by x-request-id. The access logger pops it,
// enriches it with finalized stream fields (duration, byte counts, response
// flags), and sends it to the Sink.
package requestui

import (
	"encoding/json"
	"strings"
	"sync"

	"github.com/dio/transit/examples/request-ui/sink"
	"github.com/dio/transit/up"
	"github.com/envoyproxy/envoy/source/extensions/dynamic_modules/sdk/go/shared"
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

// reqState is the per-request accumulated data stored in ResponseChunk.Context.
type reqState struct {
	requestID      string
	method         string
	path           string
	host           string
	traceID        string
	spanID         string
	requestHeaders [][2]string
	requestBody    string

	upstreamAddress string
	responseHeaders [][2]string
	responseBody    string
}

var statePool = sync.Pool{New: func() any { return &reqState{} }}

// Register wires the filter into the transit registry.
// Call from init() in cmd/main.go after constructing the sink.
func Register(name string, s *sink.Sink, pending *PendingRecords) {
	up.RegisterWithResponse(
		name,
		func(_ *up.Writer, _ *up.Request) {},
		func(w *up.Writer, chunk *up.ResponseChunk) {
			cfgMu.RLock()
			c := cfg
			cfgMu.RUnlock()

			// Headers call: StatusCode != 0, Data == nil.
			if chunk.StatusCode != 0 {
				st := statePool.Get().(*reqState)
				*st = reqState{}

				if v, ok := w.GetAttributeString(up.AttributeIDRequestId); ok {
					st.requestID = v.ToString()
				}
				if v, ok := w.GetAttributeString(up.AttributeIDRequestMethod); ok {
					st.method = v.ToString()
				}
				if v, ok := w.GetAttributeString(up.AttributeIDRequestPath); ok {
					st.path = v.ToString()
				}
				if v, ok := w.GetAttributeString(up.AttributeIDRequestHost); ok {
					st.host = v.ToString()
				}

				if span := w.GetActiveSpan(); span != nil {
					if id, ok := span.GetTraceID(); ok {
						st.traceID = id.ToString()
					}
					if id, ok := span.GetSpanID(); ok {
						st.spanID = id.ToString()
					}
				}
				if addr, ok := w.GetAttributeString(up.AttributeIDUpstreamAddress); ok {
					st.upstreamAddress = addr.ToString()
				}
				if c.RecordResponseHeaders && chunk.Headers != nil {
					st.responseHeaders = copyHeaders(chunk.Headers.GetAll())
				}

				if c.RecordResponseBody {
					*chunk.Context = st
					return
				}
				nr := buildRecord(chunk.StatusCode, st)
				pending.Store(st.requestID, nr)
				*st = reqState{}
				statePool.Put(st)
				return
			}

			// Body call: only reached when RecordResponseBody is true.
			if *chunk.Context == nil {
				return
			}
			st, ok := (*chunk.Context).(*reqState)
			if !ok || st == nil {
				return
			}
			if chunk.EndStream {
				if len(chunk.Data) > 0 {
					data := chunk.Data
					if len(data) > c.MaxBodyBytes {
						data = data[:c.MaxBodyBytes]
					}
					st.responseBody = string(data)
				}
				nr := buildRecord(chunk.StatusCode, st)
				pending.Store(st.requestID, nr)
				*st = reqState{}
				statePool.Put(st)
			}
		},
	)
}

// buildRecord constructs a partial Record from state available at response
// headers time. Finalized fields (duration, byte counts, flags, code_details)
// are left zero; the access logger sets them in OnLog after stream finalization.
func buildRecord(statusCode int, st *reqState) *sink.Record {
	r := &sink.Record{
		RequestID:       st.requestID,
		Method:          st.method,
		Path:            st.path,
		Host:            st.host,
		TraceID:         st.traceID,
		SpanID:          st.spanID,
		RequestBody:     st.requestBody,
		UpstreamAddress: st.upstreamAddress,
		ResponseBody:    st.responseBody,
		ResponseCode:    float64(statusCode),
		UpstreamStatus:  statusStr(statusCode),
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

func hasError(r *sink.Record) bool {
	return r.ErrorDetails != "" ||
		r.UpstreamFailure != "" ||
		(r.ResponseFlags != "" && containsErrorFlag(r.ResponseFlags)) ||
		r.ResponseCode >= 500
}

func containsErrorFlag(flags string) bool {
	for _, f := range []string{"UF", "UH", "UC", "UT", "UO", "NR"} {
		if strings.Contains(flags, f) {
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

func copyHeaders(raw [][2]shared.UnsafeEnvoyBuffer) [][2]string {
	out := make([][2]string, len(raw))
	for i, h := range raw {
		out[i] = [2]string{h[0].ToString(), h[1].ToString()}
	}
	return out
}
