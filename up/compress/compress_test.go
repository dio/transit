package compress_test

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/dio/transit/up/compress"
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
			compressed, err := compress.Encode(enc.name, plaintext)
			require.NoError(t, err)
			require.NotEmpty(t, compressed)

			decoded, err := compress.Decode(enc.name, compressed)
			require.NoError(t, err)
			require.Equal(t, plaintext, decoded)
		})
	}
}

func TestEncodeDecode_normalisation(t *testing.T) {
	for _, enc := range encodings {
		t.Run(enc.name+"/"+strings.TrimSpace(enc.variant), func(t *testing.T) {
			compressed, err := compress.Encode(enc.name, plaintext)
			require.NoError(t, err)

			// Decode with alternate spelling — must still work.
			decoded, err := compress.Decode(enc.variant, compressed)
			require.NoError(t, err)
			require.Equal(t, plaintext, decoded)
		})
	}
}

func TestEncodeDecode_identity(t *testing.T) {
	for _, enc := range []string{"identity", "", "Identity", "IDENTITY"} {
		t.Run(enc, func(t *testing.T) {
			out, err := compress.Encode(enc, plaintext)
			require.NoError(t, err)
			require.Equal(t, plaintext, out)

			out, err = compress.Decode(enc, plaintext)
			require.NoError(t, err)
			require.Equal(t, plaintext, out)
		})
	}
}

func TestEncodeDecode_emptyPayload(t *testing.T) {
	for _, enc := range encodings {
		t.Run(enc.name, func(t *testing.T) {
			compressed, err := compress.Encode(enc.name, []byte{})
			require.NoError(t, err)

			decoded, err := compress.Decode(enc.name, compressed)
			require.NoError(t, err)
			require.Equal(t, []byte{}, decoded)
		})
	}
}

func TestDecode_corruptData(t *testing.T) {
	garbage := []byte("this is not compressed data ~~~~")
	for _, enc := range encodings {
		t.Run(enc.name, func(t *testing.T) {
			_, err := compress.Decode(enc.name, garbage)
			require.Error(t, err)
		})
	}
}

func TestEncode_unsupportedEncoding(t *testing.T) {
	_, err := compress.Encode("compress", plaintext)
	require.Error(t, err)
	require.Contains(t, err.Error(), "compress")
}

func TestDecode_unsupportedEncoding(t *testing.T) {
	_, err := compress.Decode("compress", plaintext)
	require.Error(t, err)
	require.Contains(t, err.Error(), "compress")
}

func TestRequestIdentity(t *testing.T) {
	var h fakeHeaderSetter
	compress.RequestIdentity(&h)
	require.Len(t, h.calls, 1)
	require.Equal(t, "accept-encoding", h.calls[0][0])
	require.Equal(t, "identity", h.calls[0][1])
}

func TestAcceptEncodingAllSupported(t *testing.T) {
	tests := []struct {
		header string
		want   bool
	}{
		{"", true},
		{"identity", true},
		{"gzip", true},
		{"deflate", true},
		{"br", true},
		{"zstd", true},
		{"GZIP", true},
		{"gzip, deflate, br", true},
		{"gzip;q=0.9, deflate;q=0.8, identity;q=0.1", true},
		{"*", false},
		{"compress", false},
		{"gzip, compress", false},
		{"gzip, *", false},
	}
	for _, tt := range tests {
		t.Run(tt.header, func(t *testing.T) {
			require.Equal(t, tt.want, compress.AcceptEncodingAllSupported(tt.header))
		})
	}
}

type fakeHeaderSetter struct {
	calls [][2]string
}

func (f *fakeHeaderSetter) SetRequestHeader(name, value string) {
	f.calls = append(f.calls, [2]string{name, value})
}
