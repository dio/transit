package up

import "github.com/envoyproxy/envoy/source/extensions/dynamic_modules/sdk/go/shared"

// Writer provides actions the handler can take on the current request.
type Writer struct {
	handle  shared.HttpFilterHandle
	stopped bool

	// body replacement — set via SetRequestBody/SetResponseBody in buffered mode
	requestBodyReplacement    []byte
	hasRequestBodyReplacement bool
	responseBodyReplacement   []byte
	hasResponseBodyReplacement bool
}

// NewWriter wraps handle in a Writer. Intended for use in tests.
func NewWriter(h shared.HttpFilterHandle) *Writer { return &Writer{handle: h} }

// Log emits a message via Envoy's logging mechanism.
func (w *Writer) Log(level shared.LogLevel, format string, args ...any) {
	w.handle.Log(level, format, args...)
}

// SendLocalResponse sends an immediate response to the client and stops filter chain
// processing. Subsequent filters are not invoked. Optional headers are sent with the
// response (e.g. [2]string{"content-type", "application/json"}).
func (w *Writer) SendLocalResponse(status int, body []byte, headers ...[2]string) {
	w.handle.SendLocalResponse(uint32(status), headers, body, "")
	w.stopped = true
}

// GetAttributeString returns the string stream attribute for the given ID.
func (w *Writer) GetAttributeString(id shared.AttributeID) (shared.UnsafeEnvoyBuffer, bool) {
	return w.handle.GetAttributeString(id)
}

// GetAttributeNumber returns the numeric stream attribute for the given ID.
func (w *Writer) GetAttributeNumber(id shared.AttributeID) (float64, bool) {
	return w.handle.GetAttributeNumber(id)
}

// GetAttributeBool returns the boolean stream attribute for the given ID.
func (w *Writer) GetAttributeBool(id shared.AttributeID) (bool, bool) {
	return w.handle.GetAttributeBool(id)
}

// GetActiveSpan returns the active tracing span for the current stream.
func (w *Writer) GetActiveSpan() shared.Span {
	return w.handle.GetActiveSpan()
}

// SetRequestHeader sets a request header. Valid during request-phase callbacks.
func (w *Writer) SetRequestHeader(name, value string) {
	w.handle.RequestHeaders().Set(name, value)
}

// AddRequestHeader adds a request header without removing existing values.
func (w *Writer) AddRequestHeader(name, value string) {
	w.handle.RequestHeaders().Add(name, value)
}

// RemoveRequestHeader removes a request header.
func (w *Writer) RemoveRequestHeader(name string) {
	w.handle.RequestHeaders().Remove(name)
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
