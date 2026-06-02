package up

// StreamKey[T] is a typed handle for one slot in the per-stream object bag
// (Primitive A). The zero value is unusable; construct with NewStreamKey.
//
// StreamKey is a thin, allocation-free wrapper around a string key. All type
// safety is enforced at compile time via the generic parameter T; no reflect
// or interface{} type switches are needed at the call site.
type StreamKey[T any] struct{ key string }

// NewStreamKey returns a StreamKey that uses key as its bag slot name.
// key must be unique within a pipeline; conventionally use a dotted reverse-
// domain form: "orange.decision", "mcp.session", etc.
func NewStreamKey[T any](key string) StreamKey[T] {
	return StreamKey[T]{key: key}
}

// Key returns the underlying string key.
func (k StreamKey[T]) Key() string { return k.key }

// Set stores v in the per-stream bag via w.
// Must be called on the worker thread (same constraint as Writer.SetStreamObject).
func (k StreamKey[T]) Set(w *Writer, v T) {
	w.SetStreamObject(k.key, v)
}

// Get retrieves the value from the per-stream bag via w.
// Returns (zero, false) if the slot has not been set or the stored value
// cannot be asserted to T (which should not happen in correct usage).
// Must be called on the worker thread.
func (k StreamKey[T]) Get(w *Writer) (T, bool) {
	raw, ok := w.GetStreamObject(k.key)
	if !ok {
		var zero T
		return zero, false
	}
	v, ok := raw.(T)
	if !ok {
		var zero T
		return zero, false
	}
	return v, true
}

// GetFromCtx retrieves the value from the per-stream bag via a ClusterLBContext.
// Returns (zero, false) if the slot has not been set, the context has no bag,
// or the stored value cannot be asserted to T.
// Safe to call from the cluster main thread (same constraint as
// ClusterLBContext.GetStreamObject).
func (k StreamKey[T]) GetFromCtx(ctx ClusterLBContext) (T, bool) {
	raw, ok := ctx.GetStreamObject(k.key)
	if !ok {
		var zero T
		return zero, false
	}
	v, ok := raw.(T)
	if !ok {
		var zero T
		return zero, false
	}
	return v, true
}
