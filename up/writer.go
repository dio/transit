package up

import "github.com/envoyproxy/envoy/source/extensions/dynamic_modules/sdk/go/shared"

// Writer provides actions the handler can take on the current request.
type Writer struct {
	handle  shared.HttpFilterHandle
	stopped bool
}

// NewWriter wraps handle in a Writer. Intended for use in tests.
func NewWriter(h shared.HttpFilterHandle) *Writer { return &Writer{handle: h} }

// Log emits a message via Envoy's logging mechanism.
func (w *Writer) Log(level shared.LogLevel, format string, args ...any) {
	w.handle.Log(level, format, args...)
}

// SendLocalResponse sends an immediate response to the client and stops filter chain
// processing. Subsequent filters are not invoked.
func (w *Writer) SendLocalResponse(status int, body []byte) {
	w.handle.SendLocalResponse(uint32(status), nil, body, "")
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
