package hostpick

import "testing"

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
			if err == nil {
				t.Errorf("splitEndpoint(%q) want err, got %q:%q", tc.in, h, p)
			}
			continue
		}
		if err != nil {
			t.Errorf("splitEndpoint(%q) err: %v", tc.in, err)
			continue
		}
		if h != tc.host || p != tc.port {
			t.Errorf("splitEndpoint(%q) = %q:%q, want %q:%q", tc.in, h, p, tc.host, tc.port)
		}
	}
}
