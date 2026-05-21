package up

import "github.com/envoyproxy/envoy/source/extensions/dynamic_modules/sdk/go/shared"

// MetadataSourceType identifies which metadata store to read from.
type MetadataSourceType = shared.MetadataSourceType

const (
	MetadataSourceTypeDynamic      MetadataSourceType = shared.MetadataSourceTypeDynamic
	MetadataSourceTypeRoute        MetadataSourceType = shared.MetadataSourceTypeRoute
	MetadataSourceTypeCluster      MetadataSourceType = shared.MetadataSourceTypeCluster
	MetadataSourceTypeHost         MetadataSourceType = shared.MetadataSourceTypeHost
	MetadataSourceTypeHostLocality MetadataSourceType = shared.MetadataSourceTypeHostLocality
)

// SetMetadata writes a dynamic metadata value for the current stream.
// value may be a string, float64, int, int64, or bool.
func (w *Writer) SetMetadata(namespace, key string, value any) {
	w.handle.SetMetadata(namespace, key, value)
}

// GetMetadataString reads a string metadata value from the given source.
func (w *Writer) GetMetadataString(source MetadataSourceType, namespace, key string) (shared.UnsafeEnvoyBuffer, bool) {
	return w.handle.GetMetadataString(source, namespace, key)
}

// GetMetadataNumber reads a numeric metadata value from the given source.
func (w *Writer) GetMetadataNumber(source MetadataSourceType, namespace, key string) (float64, bool) {
	return w.handle.GetMetadataNumber(source, namespace, key)
}

// GetMetadataBool reads a boolean metadata value from the given source.
func (w *Writer) GetMetadataBool(source MetadataSourceType, namespace, key string) (bool, bool) {
	return w.handle.GetMetadataBool(source, namespace, key)
}
