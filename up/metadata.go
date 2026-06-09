package up

import (
	"strconv"

	"github.com/envoyproxy/envoy/source/extensions/dynamic_modules/sdk/go/shared"
)

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

// SetTypedMetadata is like SetMetadata but numeric values are automatically
// formatted as decimal strings before being written. This satisfies the Envoy
// dynamic modules ABI constraint (metadata values must be strings) while
// letting callers pass typed values without manual strconv calls.
func (w *Writer) SetTypedMetadata(ns, key string, value any) {
	w.SetMetadata(ns, key, numericToString(value))
}

// numericToString converts numeric types to their decimal string
// representation. Strings and bools are returned unchanged.
func numericToString(v any) any {
	switch n := v.(type) {
	case int:
		return strconv.FormatInt(int64(n), 10)
	case int8:
		return strconv.FormatInt(int64(n), 10)
	case int16:
		return strconv.FormatInt(int64(n), 10)
	case int32:
		return strconv.FormatInt(int64(n), 10)
	case int64:
		return strconv.FormatInt(n, 10)
	case uint:
		return strconv.FormatUint(uint64(n), 10)
	case uint8:
		return strconv.FormatUint(uint64(n), 10)
	case uint16:
		return strconv.FormatUint(uint64(n), 10)
	case uint32:
		return strconv.FormatUint(uint64(n), 10)
	case uint64:
		return strconv.FormatUint(n, 10)
	case float32:
		return strconv.FormatFloat(float64(n), 'f', -1, 32)
	case float64:
		return strconv.FormatFloat(n, 'f', -1, 64)
	default:
		return v
	}
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
