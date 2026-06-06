package config

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	configv1 "github.com/dio/transit/examples/orange/api/orange/config/v1"
)

// ── formatFromProto ───────────────────────────────────────────────────────────

func TestFormatFromProto_Proto(t *testing.T) {
	f, err := formatFromProto(configv1.PayloadFormat_PAYLOAD_FORMAT_PROTO)
	require.NoError(t, err)
	assert.Equal(t, SnapshotFormatProto, f)
}

func TestFormatFromProto_YAML(t *testing.T) {
	f, err := formatFromProto(configv1.PayloadFormat_PAYLOAD_FORMAT_YAML)
	require.NoError(t, err)
	assert.Equal(t, SnapshotFormatYAML, f)
}

func TestFormatFromProto_JSON(t *testing.T) {
	f, err := formatFromProto(configv1.PayloadFormat_PAYLOAD_FORMAT_JSON)
	require.NoError(t, err)
	assert.Equal(t, SnapshotFormatJSON, f)
}

func TestFormatFromProto_Msgpack(t *testing.T) {
	f, err := formatFromProto(configv1.PayloadFormat_PAYLOAD_FORMAT_MSGPACK)
	require.NoError(t, err)
	assert.Equal(t, SnapshotFormatMsgpack, f)
}

func TestFormatFromProto_Unspecified_IsError(t *testing.T) {
	// UNSPECIFIED must be rejected: the sender is required to declare a format.
	_, err := formatFromProto(configv1.PayloadFormat_PAYLOAD_FORMAT_UNSPECIFIED)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "unrecognized payload format")
}

func TestFormatFromProto_UnknownValue_IsError(t *testing.T) {
	// A value not in the enum (e.g. from a newer sender) must fail loudly.
	_, err := formatFromProto(configv1.PayloadFormat(99))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "unrecognized payload format")
}

// ── compressionFromProto ──────────────────────────────────────────────────────

func TestCompressionFromProto_None(t *testing.T) {
	c, err := compressionFromProto(configv1.Compression_COMPRESSION_NONE)
	require.NoError(t, err)
	assert.Equal(t, CompressionNone, c)
}

func TestCompressionFromProto_Zstd(t *testing.T) {
	c, err := compressionFromProto(configv1.Compression_COMPRESSION_ZSTD)
	require.NoError(t, err)
	assert.Equal(t, CompressionZstd, c)
}

func TestCompressionFromProto_Unspecified_BecomesNone(t *testing.T) {
	// COMPRESSION_UNSPECIFIED is treated as COMPRESSION_NONE per §4a.
	c, err := compressionFromProto(configv1.Compression_COMPRESSION_UNSPECIFIED)
	require.NoError(t, err)
	assert.Equal(t, CompressionNone, c)
}

func TestCompressionFromProto_UnknownValue_IsError(t *testing.T) {
	_, err := compressionFromProto(configv1.Compression(99))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "unrecognized compression")
}

// ── envelopeFromProto ─────────────────────────────────────────────────────────

func TestEnvelopeFromProto_FullProtoZstd(t *testing.T) {
	payload := []byte("compressed-payload-bytes")
	checksum := make([]byte, 32) // synthetic SHA-256
	pb := &configv1.SnapshotEnvelope{
		Version:     7,
		Format:      configv1.PayloadFormat_PAYLOAD_FORMAT_PROTO,
		Compression: configv1.Compression_COMPRESSION_ZSTD,
		Payload:     payload,
		Checksum:    checksum,
	}
	env, err := envelopeFromProto(pb)
	require.NoError(t, err)
	assert.Equal(t, uint64(7), env.Version)
	assert.Equal(t, SnapshotFormatProto, env.Format)
	assert.Equal(t, CompressionZstd, env.Compression)
	assert.Equal(t, payload, env.Payload)
	assert.Equal(t, checksum, env.Checksum)
}

func TestEnvelopeFromProto_YAMLNone(t *testing.T) {
	pb := &configv1.SnapshotEnvelope{
		Version:     1,
		Format:      configv1.PayloadFormat_PAYLOAD_FORMAT_YAML,
		Compression: configv1.Compression_COMPRESSION_NONE,
		Payload:     []byte("llm:\n  providers: {}"),
	}
	env, err := envelopeFromProto(pb)
	require.NoError(t, err)
	assert.Equal(t, SnapshotFormatYAML, env.Format)
	assert.Equal(t, CompressionNone, env.Compression)
}

func TestEnvelopeFromProto_JSONNone(t *testing.T) {
	pb := &configv1.SnapshotEnvelope{
		Version: 2,
		Format:  configv1.PayloadFormat_PAYLOAD_FORMAT_JSON,
		Payload: []byte(`{"llm":{"providers":{}}}`),
	}
	env, err := envelopeFromProto(pb)
	require.NoError(t, err)
	assert.Equal(t, SnapshotFormatJSON, env.Format)
}

func TestEnvelopeFromProto_UnspecifiedCompression_BecomesNone(t *testing.T) {
	// Wire sends COMPRESSION_UNSPECIFIED (zero value) — receiver normalises to none.
	pb := &configv1.SnapshotEnvelope{
		Version:     3,
		Format:      configv1.PayloadFormat_PAYLOAD_FORMAT_PROTO,
		Compression: configv1.Compression_COMPRESSION_UNSPECIFIED,
		Payload:     []byte("raw-proto-bytes"),
	}
	env, err := envelopeFromProto(pb)
	require.NoError(t, err)
	assert.Equal(t, CompressionNone, env.Compression)
}

func TestEnvelopeFromProto_UnspecifiedFormat_IsError(t *testing.T) {
	pb := &configv1.SnapshotEnvelope{
		Version: 1,
		// Format left at zero value = PAYLOAD_FORMAT_UNSPECIFIED
		Payload: []byte("some-bytes"),
	}
	_, err := envelopeFromProto(pb)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "unrecognized payload format")
}

func TestEnvelopeFromProto_NilProto_IsError(t *testing.T) {
	_, err := envelopeFromProto(nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "nil proto envelope")
}

func TestEnvelopeFromProto_VersionZero_OK(t *testing.T) {
	// Version 0 is technically valid on the wire (bootstrap / first send).
	// The SoTW stale-rejection check happens in AppState, not in envelopeFromProto.
	pb := &configv1.SnapshotEnvelope{
		Version: 0,
		Format:  configv1.PayloadFormat_PAYLOAD_FORMAT_PROTO,
		Payload: []byte("payload"),
	}
	env, err := envelopeFromProto(pb)
	require.NoError(t, err)
	assert.Equal(t, uint64(0), env.Version)
}

func TestEnvelopeFromProto_EmptyPayload_OK(t *testing.T) {
	// An empty payload is valid at this layer; decodeRawConfig will handle
	// the resulting error if the bytes are not decodable.
	pb := &configv1.SnapshotEnvelope{
		Version: 5,
		Format:  configv1.PayloadFormat_PAYLOAD_FORMAT_PROTO,
		Payload: nil,
	}
	env, err := envelopeFromProto(pb)
	require.NoError(t, err)
	assert.Nil(t, env.Payload)
	assert.Nil(t, env.Checksum)
}

func TestEnvelopeFromProto_MsgpackFormat(t *testing.T) {
	// Msgpack is a valid format constant; envelopeFromProto accepts it even
	// though decodeRawConfig will return an error until support is implemented.
	pb := &configv1.SnapshotEnvelope{
		Version: 1,
		Format:  configv1.PayloadFormat_PAYLOAD_FORMAT_MSGPACK,
		Payload: []byte("msgpack-bytes"),
	}
	env, err := envelopeFromProto(pb)
	require.NoError(t, err)
	assert.Equal(t, SnapshotFormatMsgpack, env.Format)
}

func TestEnvelopeFromProto_UnknownFormat_IsError(t *testing.T) {
	pb := &configv1.SnapshotEnvelope{
		Version: 1,
		Format:  configv1.PayloadFormat(99),
		Payload: []byte("bytes"),
	}
	_, err := envelopeFromProto(pb)
	require.Error(t, err)
}

func TestEnvelopeFromProto_UnknownCompression_IsError(t *testing.T) {
	pb := &configv1.SnapshotEnvelope{
		Version:     1,
		Format:      configv1.PayloadFormat_PAYLOAD_FORMAT_PROTO,
		Compression: configv1.Compression(99),
		Payload:     []byte("bytes"),
	}
	_, err := envelopeFromProto(pb)
	require.Error(t, err)
}

// ── Constant value assertions ─────────────────────────────────────────────────
// These pin the string values so a rename of the constant is caught by the
// test suite before it breaks a YAML or JSON seed file that uses these strings.

func TestSnapshotFormatConstants(t *testing.T) {
	assert.Equal(t, SnapshotFormat("proto"), SnapshotFormatProto)
	assert.Equal(t, SnapshotFormat("yaml"), SnapshotFormatYAML)
	assert.Equal(t, SnapshotFormat("json"), SnapshotFormatJSON)
	assert.Equal(t, SnapshotFormat("msgpack"), SnapshotFormatMsgpack)
}

func TestCompressionKindConstants(t *testing.T) {
	assert.Equal(t, CompressionKind("none"), CompressionNone)
	assert.Equal(t, CompressionKind("zstd"), CompressionZstd)
}
