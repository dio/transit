package up

import "github.com/envoyproxy/envoy/source/extensions/dynamic_modules/sdk/go/shared"

// SetMetadata writes a per-stream dynamic metadata value.
// value must be a string, bool, or numeric type (int, int64, float64, etc.).
// Panics for unsupported types rather than silently producing a no-op.
// On the request path (queued mode) this is batched with other mutations and
// applied in flush() before ContinueRequest. On the response path (directWrite=true)
// it applies immediately.
func (w *Writer) SetMetadata(ns, key string, value any) {
	switch value.(type) {
	case string, bool,
		int, int8, int16, int32, int64,
		uint, uint8, uint16, uint32, uint64,
		float32, float64:
	default:
		panic("up: SetMetadata: unsupported value type; use string, bool, or a numeric type")
	}
	if w.queued() {
		w.f.dynamicMetadata = append(w.f.dynamicMetadata, dynamicMetadataMutation{ns: ns, key: key, value: value})
		return
	}
	w.f.handle.SetMetadata(ns, key, value)
}

// GetMetadataString reads a string metadata value from the given source.
// Returns a Buffer (copy the data with buf.String() before the callback returns).
func (w *Writer) GetMetadataString(source MetadataSource, ns, key string) (Buffer, bool) {
	v, ok := w.f.handle.GetMetadataString(shared.MetadataSourceType(source), ns, key)
	if !ok {
		return Buffer{}, false
	}
	return newBuffer(v), true
}

// GetMetadataNumber reads a numeric metadata value from the given source.
func (w *Writer) GetMetadataNumber(source MetadataSource, ns, key string) (float64, bool) {
	return w.f.handle.GetMetadataNumber(shared.MetadataSourceType(source), ns, key)
}

// GetMetadataBool reads a boolean metadata value from the given source.
func (w *Writer) GetMetadataBool(source MetadataSource, ns, key string) (bool, bool) {
	return w.f.handle.GetMetadataBool(shared.MetadataSourceType(source), ns, key)
}
