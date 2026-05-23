package up

import "github.com/envoyproxy/envoy/source/extensions/dynamic_modules/sdk/go/shared"

// MetadataSourceType identifies which metadata store to read from.
type MetadataSourceType uint32

const (
	MetadataSourceTypeDynamic      MetadataSourceType = MetadataSourceType(shared.MetadataSourceTypeDynamic)
	MetadataSourceTypeRoute        MetadataSourceType = MetadataSourceType(shared.MetadataSourceTypeRoute)
	MetadataSourceTypeCluster      MetadataSourceType = MetadataSourceType(shared.MetadataSourceTypeCluster)
	MetadataSourceTypeHost         MetadataSourceType = MetadataSourceType(shared.MetadataSourceTypeHost)
	MetadataSourceTypeHostLocality MetadataSourceType = MetadataSourceType(shared.MetadataSourceTypeHostLocality)
)

// SetMetadata writes a dynamic metadata value for the current stream.
// value may be a string, float64, int, int64, or bool.
func (w *Writer) SetMetadata(namespace, key string, value any) {
	w.handle.SetMetadata(namespace, key, value)
}

// GetMetadataString reads a string metadata value from the given source.
func (w *Writer) GetMetadataString(source MetadataSourceType, namespace, key string) (Buffer, bool) {
	v, ok := w.handle.GetMetadataString(shared.MetadataSourceType(source), namespace, key)
	if !ok {
		return Buffer{}, false
	}
	return newBuffer(v), true
}

// GetMetadataNumber reads a numeric metadata value from the given source.
func (w *Writer) GetMetadataNumber(source MetadataSourceType, namespace, key string) (float64, bool) {
	return w.handle.GetMetadataNumber(shared.MetadataSourceType(source), namespace, key)
}

// GetMetadataBool reads a boolean metadata value from the given source.
func (w *Writer) GetMetadataBool(source MetadataSourceType, namespace, key string) (bool, bool) {
	return w.handle.GetMetadataBool(shared.MetadataSourceType(source), namespace, key)
}
