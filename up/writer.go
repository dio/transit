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
