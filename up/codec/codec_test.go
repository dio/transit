package codec_test

import (
	"strings"
	"testing"

	"github.com/dio/transit/up/codec"
	"github.com/stretchr/testify/require"
)

var plaintext = []byte("the quick brown fox jumps over the lazy dog")

// encodings lists every supported content-encoding name alongside a canonical
// spelling used for the round-trip and an alternate spelling to exercise
// normalisation.
var encodings = []struct {
	name    string
	variant string // alternate capitalisation / whitespace
}{
	{"gzip", "Gzip"},
	{"deflate", " deflate "},
	{"br", "BR"},
	{"zstd", "ZSTD"},
}

func TestEncodeDecode_roundTrip(t *testing.T) {
	for _, enc := range encodings {
		t.Run(enc.name, func(t *testing.T) {
			compressed, err := codec.Encode(enc.name, plaintext)
			require.NoError(t, err)
			require.NotEmpty(t, compressed)

			decoded, err := codec.Decode(enc.name, compressed)
			require.NoError(t, err)
			require.Equal(t, plaintext, decoded)
		})
	}
}

func TestEncodeDecode_normalisation(t *testing.T) {
	for _, enc := range encodings {
		t.Run(enc.name+"/"+strings.TrimSpace(enc.variant), func(t *testing.T) {
			compressed, err := codec.Encode(enc.name, plaintext)
			require.NoError(t, err)

			// Decode with alternate spelling — must still work.
			decoded, err := codec.Decode(enc.variant, compressed)
			require.NoError(t, err)
			require.Equal(t, plaintext, decoded)
		})
	}
}

func TestEncodeDecode_identity(t *testing.T) {
	for _, enc := range []string{"identity", "", "Identity", "IDENTITY"} {
		t.Run(enc, func(t *testing.T) {
			out, err := codec.Encode(enc, plaintext)
			require.NoError(t, err)
			require.Equal(t, plaintext, out)

			out, err = codec.Decode(enc, plaintext)
			require.NoError(t, err)
			require.Equal(t, plaintext, out)
		})
	}
}

func TestEncodeDecode_emptyPayload(t *testing.T) {
	for _, enc := range encodings {
		t.Run(enc.name, func(t *testing.T) {
			compressed, err := codec.Encode(enc.name, []byte{})
			require.NoError(t, err)

			decoded, err := codec.Decode(enc.name, compressed)
			require.NoError(t, err)
			require.Equal(t, []byte{}, decoded)
		})
	}
}

func TestDecode_corruptData(t *testing.T) {
	garbage := []byte("this is not compressed data ~~~~")
	for _, enc := range encodings {
		t.Run(enc.name, func(t *testing.T) {
			_, err := codec.Decode(enc.name, garbage)
			require.Error(t, err)
		})
	}
}

func TestEncode_unsupportedEncoding(t *testing.T) {
	_, err := codec.Encode("compress", plaintext)
	require.Error(t, err)
	require.Contains(t, err.Error(), "compress")
}

func TestDecode_unsupportedEncoding(t *testing.T) {
	_, err := codec.Decode("compress", plaintext)
	require.Error(t, err)
	require.Contains(t, err.Error(), "compress")
}

func TestNegotiateIdentity(t *testing.T) {
	var h fakeHeaderSetter
	codec.NegotiateIdentity(&h)
	require.Len(t, h.calls, 1)
	require.Equal(t, "accept-encoding", h.calls[0][0])
	require.Equal(t, "identity", h.calls[0][1])
}

type fakeHeaderSetter struct {
	calls [][2]string
}

func (f *fakeHeaderSetter) SetRequestHeader(name, value string) {
	f.calls = append(f.calls, [2]string{name, value})
}
