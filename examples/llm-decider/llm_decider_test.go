package llmdecider

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/dio/libsygo"
)

func TestPromptFromBody(t *testing.T) {
	cases := []struct {
		name    string
		body    string
		want    string
		wantErr string
	}{
		{name: "prompt", body: `{"prompt":"refactor the payment module"}`, want: "refactor the payment module"},
		{name: "extra fields ignored", body: `{"model":"auto","prompt":"hi","stream":true}`, want: "hi"},
		{name: "empty body", body: ``, wantErr: "no body"},
		{name: "malformed", body: `{"prompt":`, wantErr: "parsing request body"},
		{name: "missing prompt", body: `{"model":"auto"}`, wantErr: "non-empty"},
		{name: "empty prompt", body: `{"prompt":""}`, wantErr: "non-empty"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := promptFromBody([]byte(tc.body))
			if tc.wantErr != "" {
				require.Error(t, err)
				require.Contains(t, err.Error(), tc.wantErr)
				return
			}
			require.NoError(t, err)
			require.Equal(t, tc.want, got)
		})
	}
}

// decisionSource names the rule behind a decision, so the surfaces downstream of
// the filter can attribute it.
func TestDecisionSource(t *testing.T) {
	cases := []struct {
		name    string
		outcome libsygo.Outcome
		want    string
	}{
		{
			name:    "judged",
			outcome: libsygo.Outcome{Evidence: json.RawMessage(`{"source":"decision_classifier","score":0.77}`)},
			want:    "decision_classifier",
		},
		{
			name:    "fail open",
			outcome: libsygo.Outcome{Evidence: json.RawMessage(`{"source":"fail_open","reason_code":"call_error"}`)},
			want:    "fail_open",
		},
		{
			name:    "latched",
			outcome: libsygo.Outcome{Latched: true, Evidence: json.RawMessage(`{"source":"retained"}`)},
			want:    "retained",
		},
		{
			name:    "latched without evidence",
			outcome: libsygo.Outcome{Latched: true},
			want:    "retained",
		},
		{
			name:    "no evidence",
			outcome: libsygo.Outcome{},
			want:    "",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			require.Equal(t, tc.want, decisionSource(tc.outcome))
		})
	}
}
