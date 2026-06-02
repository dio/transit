package hostpick

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestSplitEndpoint(t *testing.T) {
	cases := []struct {
		in, host, port string
		wantErr        bool
	}{
		{"https://api.openai.com", "api.openai.com", "443", false},
		{"http://localhost:8081", "localhost", "8081", false},
		{"https://us-east5-aiplatform.googleapis.com", "us-east5-aiplatform.googleapis.com", "443", false},
		{"http://example.com", "example.com", "80", false},
		{"", "", "", true},
		{"ftp://x", "", "", true},
	}
	for _, tc := range cases {
		h, p, err := splitEndpoint(tc.in)
		if tc.wantErr {
			require.Error(t, err, "splitEndpoint(%q) want err", tc.in)
			continue
		}
		require.NoError(t, err, "splitEndpoint(%q)", tc.in)
		require.Equal(t, tc.host, h, "splitEndpoint(%q) host", tc.in)
		require.Equal(t, tc.port, p, "splitEndpoint(%q) port", tc.in)
	}
}
