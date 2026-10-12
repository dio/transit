package llmdecider

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/dio/libsygo"
)

func TestRequestFromBody(t *testing.T) {
	cases := []struct {
		name    string
		body    string
		want    string
		wantErr string
	}{
		{
			name: "user message",
			body: `{"model":"auto","messages":[{"role":"user","content":"refactor the payment module"}]}`,
			want: "refactor the payment module",
		},
		{
			name: "extra fields ignored",
			body: `{"model":"auto","stream":true,"temperature":0.2,"messages":[{"role":"user","content":"hi"}]}`,
			want: "hi",
		},
		{
			// System content is an instruction, not a conversation turn, so it must
			// not be mistaken for one.
			name: "instructions are not messages",
			body: `{"messages":[{"role":"system","content":"be terse"},{"role":"user","content":"hi"}]}`,
			want: "hi",
		},
		{
			// A follow-up is a second user turn, not a replacement for the first.
			// Which of the two the judge is shown is libsygo's decision, pinned by
			// its own conformance corpus.
			name: "follow-up",
			body: `{"messages":[{"role":"user","content":"first"},{"role":"user","content":"second"}]}`,
			want: "first",
		},
		{name: "empty body", body: ``, wantErr: "no body"},
		{name: "malformed", body: `{"messages":`, wantErr: "normalizing request body"},
		{name: "no messages", body: `{"model":"auto"}`, wantErr: "non-empty"},
		{name: "system only", body: `{"messages":[{"role":"system","content":"be terse"}]}`, wantErr: "non-empty"},
		// A request this dataplane cannot represent is refused rather than routed
		// as though it were something else.
		{name: "unknown role", body: `{"messages":[{"role":"wizard","content":"hi"}]}`, wantErr: "normalizing request body"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			request, err := requestFromBody([]byte(tc.body))
			if tc.wantErr != "" {
				require.Error(t, err)
				require.Contains(t, err.Error(), tc.wantErr)
				return
			}
			require.NoError(t, err)
			require.NotEmpty(t, request.Messages)

			text, ok := request.Messages[0].TextContent(" ")
			require.True(t, ok, "the first message has no text")
			require.Equal(t, tc.want, text)
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
