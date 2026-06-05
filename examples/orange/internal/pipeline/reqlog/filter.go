// Package reqlog is an Envoy dynamic module filter that records every request
// and response as a structured [Record] and fans it out to all registered
// [Exporter] instances.
//
// Capture phases:
//   - Request headers — method, path, host, request-id, trace/span IDs,
//     request headers (if enabled).
//   - Request body — accumulated up to MaxBodyBytes (if enabled).
//   - Response headers — status code, response headers (if enabled).
//   - Response body end-of-stream — response body (if enabled) plus all
//     orange/orange_meter dynamic metadata written by match, adapt, and meter.
//   - OnStreamFinalized — Envoy timing, byte counts, upstream details. The
//     record is assembled here, field-filtered, and dispatched to exporters.
//
// The filter is position-sensitive: place it as the first entry in
// http_filters so it captures the original (pre-match) request body, and
// receives the response last — after all upstream filters (adapt, meter,
// tracer) have written their metadata.
//
// Registration: blank-import this package from cmd/main.go. To receive
// records, call [AddExporter] from an init() in your binary. A stdout JSON
// exporter is available via [NewStdoutExporter]; enable it explicitly if
// needed.
package reqlog

import (
	"strings"

	"github.com/dio/transit/examples/orange/internal/pipeline/match"
	"github.com/dio/transit/up"
)

const (
	// ExtensionName is the Envoy filter name. It must also be used as the
	// logger_name in the access_log dynamic_modules entry in envoy.yaml so
	// that WithOnStreamFinalized receives finalized timing fields.
	ExtensionName = "orange-reqlog"

	// meterNS is the dynamic metadata namespace written by orange-meter.
	meterNS = "orange_meter"
)

// reqState accumulates per-stream fields across the filter callbacks.
type reqState struct {
	requestID string
	method    string
	path      string
	host      string
	traceID   string
	spanID    string

	requestHeaders [][2]string
	requestBody    []byte
	reqBodyTrunc   bool

	statusCode      int
	responseHeaders [][2]string
	responseBody    []byte
	respBodyTrunc   bool

	// Orange LLM metadata captured at response end-of-stream.
	model                    string
	providerBackend          string
	providerKind             string
	endpoint                 string
	backendModel             string
	passthrough              string
	gatewayClient            string
	inputTokens              string
	outputTokens             string
	cachedInputTokens        string
	reasoningOutputTokens    string
	cacheCreationInputTokens string
	cacheReadInputTokens     string
	imageCount               string
	imageSize                string
	imageQuality             string
	responseModalities       string
}

func init() {
	up.Register(ExtensionName, requestHandler,
		up.WithStreamingBody(bodyHandler),
		up.WithResponse(responseHandler),
		up.WithOnStreamFinalized(finalizedHandler),
	)
}

func requestHandler(w *up.Writer, r *up.Request) {
	cfgMu.RLock()
	c := cfg
	cfgMu.RUnlock()

	st := &reqState{
		method: r.Method,
		path:   r.Path,
		host:   r.Host,
	}
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
}

func bodyHandler(w *up.Writer, chunk *up.BodyChunk) {
	if len(chunk.Data) == 0 {
		return
	}
	st, ok := (*chunk.Context).(*reqState)
	if !ok || st == nil {
		return
	}

	cfgMu.RLock()
	c := cfg
	cfgMu.RUnlock()

	if !c.RecordRequestBody {
		return
	}
	remaining := c.MaxBodyBytes - len(st.requestBody)
	if remaining <= 0 {
		st.reqBodyTrunc = true
		return
	}
	data := chunk.Data
	if len(data) > remaining {
		data = data[:remaining]
		st.reqBodyTrunc = true
	}
	st.requestBody = append(st.requestBody, data...)
}

func responseHandler(w *up.Writer, chunk *up.ResponseChunk) {
	st, ok := (*chunk.Context).(*reqState)
	if !ok || st == nil {
		return
	}

	cfgMu.RLock()
	c := cfg
	cfgMu.RUnlock()

	if chunk.StatusCode != 0 {
		st.statusCode = chunk.StatusCode
		if c.RecordResponseHeaders {
			st.responseHeaders = chunk.AllHeaders()
		}
		return
	}

	if !chunk.EndStream {
		if !c.RecordResponseBody {
			return
		}
		remaining := c.MaxBodyBytes - len(st.responseBody)
		if remaining <= 0 {
			st.respBodyTrunc = true
			return
		}
		if len(chunk.Data) > 0 {
			data := chunk.Data
			if len(data) > remaining {
				data = data[:remaining]
				st.respBodyTrunc = true
			}
			st.responseBody = append(st.responseBody, data...)
		}
		return
	}

	// EndStream: capture final body chunk then read all dynamic metadata.
	if c.RecordResponseBody && len(chunk.Data) > 0 {
		remaining := c.MaxBodyBytes - len(st.responseBody)
		if remaining > 0 {
			data := chunk.Data
			if len(data) > remaining {
				data = data[:remaining]
				st.respBodyTrunc = true
			}
			st.responseBody = append(st.responseBody, data...)
		}
	}

	// orange namespace (match + adapt)
	st.model = metaStr(w, match.MetadataNamespace, match.MetadataKeyModel)
	st.providerBackend = metaStr(w, match.MetadataNamespace, match.MetadataKeyUpstream)
	st.providerKind = metaStr(w, match.MetadataNamespace, match.MetadataKeyProvider)
	st.endpoint = metaStr(w, match.MetadataNamespace, match.MetadataKeyEndpoint)
	st.backendModel = metaStr(w, match.MetadataNamespace, match.MetadataKeyBackendModel)
	st.passthrough = metaStr(w, match.MetadataNamespace, "passthrough")
	st.gatewayClient = metaStr(w, match.MetadataNamespace, "gateway_client")

	// orange_meter namespace
	st.inputTokens = metaStr(w, meterNS, "input_tokens")
	st.outputTokens = metaStr(w, meterNS, "output_tokens")
	st.cachedInputTokens = metaStr(w, meterNS, "cached_input_tokens")
	st.reasoningOutputTokens = metaStr(w, meterNS, "reasoning_output_tokens")
	st.cacheCreationInputTokens = metaStr(w, meterNS, "cache_creation_input_tokens")
	st.cacheReadInputTokens = metaStr(w, meterNS, "cache_read_input_tokens")
	st.imageCount = metaStr(w, meterNS, "image_count")
	st.imageSize = metaStr(w, meterNS, "image_size")
	st.imageQuality = metaStr(w, meterNS, "image_quality")
	st.responseModalities = metaStr(w, meterNS, "response_modalities")
}

func finalizedHandler(ctx *any, info up.FinalizedInfo) {
	if ctx == nil || *ctx == nil {
		return
	}
	st, ok := (*ctx).(*reqState)
	if !ok || st == nil {
		return
	}

	cfgMu.RLock()
	c := cfg
	cfgMu.RUnlock()

	r := buildRecord(st, info, c)
	global.Export(r)
}

func buildRecord(st *reqState, info up.FinalizedInfo, c filterConfig) *Record {
	r := &Record{
		RequestID:       st.requestID,
		TraceID:         st.traceID,
		SpanID:          st.spanID,
		Method:          st.method,
		Path:            st.path,
		Host:            st.host,
		RequestHeaders:  st.requestHeaders,
		RequestTruncated: st.reqBodyTrunc,
		StatusCode:      st.statusCode,
		ResponseHeaders: st.responseHeaders,
		ResponseTruncated: st.respBodyTrunc,

		Model:                    st.model,
		ProviderBackend:          st.providerBackend,
		ProviderKind:             st.providerKind,
		Endpoint:                 st.endpoint,
		BackendModel:             st.backendModel,
		Passthrough:              st.passthrough,
		GatewayClient:            st.gatewayClient,
		InputTokens:              st.inputTokens,
		OutputTokens:             st.outputTokens,
		CachedInputTokens:        st.cachedInputTokens,
		ReasoningOutputTokens:    st.reasoningOutputTokens,
		CacheCreationInputTokens: st.cacheCreationInputTokens,
		CacheReadInputTokens:     st.cacheReadInputTokens,
		ImageCount:               st.imageCount,
		ImageSize:                st.imageSize,
		ImageQuality:             st.imageQuality,
		ResponseModalities:       st.responseModalities,
	}

	if len(st.requestBody) > 0 {
		r.RequestBody = string(st.requestBody)
	}
	if len(st.responseBody) > 0 {
		r.ResponseBody = string(st.responseBody)
	}

	// Finalized stream fields.
	if info.Timing.RequestCompleteDurationNs >= 0 {
		r.DurationMs = float64(info.Timing.RequestCompleteDurationNs) / 1e6
	}
	r.FirstUpstreamTxByteSentNs = info.Timing.FirstUpstreamTxByteSentNs
	r.LastUpstreamRxByteReceivedNs = info.Timing.LastUpstreamRxByteReceivedNs
	if info.UpstreamPoolReadyDurationNs >= 0 {
		r.UpstreamCxPoolReadyMs = float64(info.UpstreamPoolReadyDurationNs) / 1e6
	}
	r.RequestSizeBytes = info.Bytes.BytesReceived
	r.ResponseSizeBytes = info.Bytes.BytesSent
	r.WireBytesReceived = info.Bytes.WireBytesReceived
	r.WireBytesSent = info.Bytes.WireBytesSent

	// Use finalized response code when the response handler never saw headers.
	if r.StatusCode == 0 && info.ResponseCode > 0 {
		r.StatusCode = int(info.ResponseCode)
	}
	r.ResponseDetails = info.ResponseCodeDetails
	if flags := up.ResponseFlagsString(info.ResponseFlags); flags != "" {
		r.ResponseFlags = flags
	}
	r.UpstreamFailure = info.UpstreamFailure
	r.UpstreamAttempts = info.UpstreamRequestAttempts
	r.UpstreamLocalAddress = info.UpstreamLocalAddress
	r.UpstreamAddress = info.UpstreamAddress
	r.Protocol = info.RequestProtocol
	r.LocalReplyBody = info.LocalReplyBody

	// Prefer finalized trace IDs over the early-phase ones (they may differ
	// if the tracer filter rewrites them).
	if info.TraceID != "" {
		r.TraceID = info.TraceID
	}
	if info.SpanID != "" {
		r.SpanID = info.SpanID
	}

	r.HasError = isError(r)

	// Apply global field filter.
	ff := NewFieldFilter(c.FieldFilter)
	ff.Apply(r)

	return r
}

func isError(r *Record) bool {
	return r.UpstreamFailure != "" ||
		(r.ResponseFlags != "" && containsErrorFlag(r.ResponseFlags)) ||
		r.StatusCode >= 500
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

// metaStr reads a dynamic metadata string and returns it as a Go string.
// Returns "" if not present.
func metaStr(w *up.Writer, ns, key string) string {
	if v, ok := w.GetMetadataString(up.MetadataSourceDynamic, ns, key); ok {
		return v.ToString()
	}
	return ""
}
