package config

import (
	"fmt"

	configv1 "github.com/dio/transit/examples/orange/api/orange/config/v1"
)

// ── Envelope types ────────────────────────────────────────────────────────────

// SnapshotEnvelope is the Go representation of the outermost delivery frame.
// It is produced by envelopeFromProto() from the wire proto and is the entry
// point for decodeRawConfig() in Phase 7.
//
// Version must be monotonically increasing. The receiver discards any envelope
// whose Version is not strictly greater than the last accepted version — this
// is the SoTW stale-rejection contract described in §4 of the design doc.
//
// Payload holds the (possibly compressed) config bytes. Checksum is the
// SHA-256 of the decompressed payload and is verified before decoding.
type SnapshotEnvelope struct {
	Version     uint64
	Format      SnapshotFormat
	Compression CompressionKind
	Payload     []byte
	Checksum    []byte // SHA-256 of decompressed Payload bytes
}

// SnapshotFormat identifies the encoding of SnapshotEnvelope.Payload.
// Only the values defined as constants below are accepted by decodeRawConfig.
type SnapshotFormat string

const (
	// SnapshotFormatProto is the recommended production format. Payload is a
	// proto-encoded ConfigPayload, decompressed and decoded by protoToRaw().
	SnapshotFormatProto SnapshotFormat = "proto"

	// SnapshotFormatYAML is valid for local development and seed files.
	// Payload is decoded directly into RawConfig by yaml.Unmarshal.
	SnapshotFormatYAML SnapshotFormat = "yaml"

	// SnapshotFormatJSON is valid for local development and seed files.
	// Payload is decoded directly into RawConfig by json.Unmarshal.
	SnapshotFormatJSON SnapshotFormat = "json"

	// SnapshotFormatMsgpack is defined for optional future use.
	// decodeRawConfig returns an error for this format until support is added.
	SnapshotFormatMsgpack SnapshotFormat = "msgpack"
)

// CompressionKind identifies the compression applied to SnapshotEnvelope.Payload.
type CompressionKind string

const (
	// CompressionNone means the payload bytes are stored verbatim.
	// COMPRESSION_UNSPECIFIED in the proto is treated as CompressionNone.
	CompressionNone CompressionKind = "none"

	// CompressionZstd means the payload is zstd-compressed.
	// decompress() in Phase 7 handles decompression before checksum verification.
	CompressionZstd CompressionKind = "zstd"
)

// ── Proto conversion ──────────────────────────────────────────────────────────

// envelopeFromProto converts a wire-level configv1.SnapshotEnvelope into the
// Go SnapshotEnvelope used by decodeRawConfig. It maps proto enum values to
// their string constants and propagates the version, payload, and checksum
// fields verbatim.
//
// COMPRESSION_UNSPECIFIED is silently promoted to CompressionNone (no-op path).
// PAYLOAD_FORMAT_UNSPECIFIED is rejected — the sender must always specify a format.
func envelopeFromProto(pb *configv1.SnapshotEnvelope) (SnapshotEnvelope, error) {
	if pb == nil {
		return SnapshotEnvelope{}, fmt.Errorf("envelope: nil proto envelope")
	}
	format, err := formatFromProto(pb.Format)
	if err != nil {
		return SnapshotEnvelope{}, err
	}
	compression, err := compressionFromProto(pb.Compression)
	if err != nil {
		return SnapshotEnvelope{}, err
	}
	return SnapshotEnvelope{
		Version:     pb.Version,
		Format:      format,
		Compression: compression,
		Payload:     pb.Payload,
		Checksum:    pb.Checksum,
	}, nil
}

// formatFromProto maps a PayloadFormat proto enum to a SnapshotFormat constant.
// PAYLOAD_FORMAT_UNSPECIFIED is an error: the sender must declare the format.
func formatFromProto(f configv1.PayloadFormat) (SnapshotFormat, error) {
	switch f {
	case configv1.PayloadFormat_PAYLOAD_FORMAT_PROTO:
		return SnapshotFormatProto, nil
	case configv1.PayloadFormat_PAYLOAD_FORMAT_YAML:
		return SnapshotFormatYAML, nil
	case configv1.PayloadFormat_PAYLOAD_FORMAT_JSON:
		return SnapshotFormatJSON, nil
	case configv1.PayloadFormat_PAYLOAD_FORMAT_MSGPACK:
		return SnapshotFormatMsgpack, nil
	default:
		// Covers PAYLOAD_FORMAT_UNSPECIFIED (0) and any future values the
		// receiver does not yet know about. Both are treated as errors so that
		// a mis-configured sender fails loudly rather than silently decoding
		// garbage.
		return "", fmt.Errorf("envelope: unrecognized payload format %v (%d)", f, int32(f))
	}
}

// compressionFromProto maps a Compression proto enum to a CompressionKind constant.
// COMPRESSION_UNSPECIFIED is treated as CompressionNone per the proto comment in §4a.
func compressionFromProto(c configv1.Compression) (CompressionKind, error) {
	switch c {
	case configv1.Compression_COMPRESSION_UNSPECIFIED, configv1.Compression_COMPRESSION_NONE:
		return CompressionNone, nil
	case configv1.Compression_COMPRESSION_ZSTD:
		return CompressionZstd, nil
	default:
		return "", fmt.Errorf("envelope: unrecognized compression %v (%d)", c, int32(c))
	}
}
