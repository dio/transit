package reqlog

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/dio/transit/up"
)

func TestDefaultConfig(t *testing.T) {
	cfgMu.RLock()
	c := cfg
	cfgMu.RUnlock()

	require.True(t, c.RecordRequestHeaders, "want RecordRequestHeaders=true by default")
	require.True(t, c.RecordResponseHeaders, "want RecordResponseHeaders=true by default")
	require.False(t, c.RecordRequestBody, "want RecordRequestBody=false by default")
	require.False(t, c.RecordResponseBody, "want RecordResponseBody=false by default")
	require.Equal(t, defaultMaxBody, c.MaxBodyBytes)
}

func TestFieldFilter_AllowList(t *testing.T) {
	f := NewFieldFilter(FieldFilterConfig{
		AllowRequestHeaders: []string{"content-type", "authorization"},
	})
	hdrs := [][2]string{
		{"content-type", "application/json"},
		{"authorization", "Bearer tok"},
		{"x-custom", "value"},
	}
	got := applyHeaderFilter(hdrs, f.allowReqHdrs, f.denyReqHdrs)
	require.Len(t, got, 2)
	assert.Equal(t, "content-type", got[0][0])
	assert.Equal(t, "authorization", got[1][0])
}

func TestFieldFilter_DenyList(t *testing.T) {
	f := NewFieldFilter(FieldFilterConfig{
		DenyRequestHeaders: []string{"authorization", "x-api-key"},
	})
	hdrs := [][2]string{
		{"content-type", "application/json"},
		{"authorization", "Bearer tok"},
		{"x-api-key", "sk-123"},
	}
	got := applyHeaderFilter(hdrs, f.allowReqHdrs, f.denyReqHdrs)
	require.Len(t, got, 1)
	assert.Equal(t, "content-type", got[0][0])
}

func TestFieldFilter_AllowThenDeny(t *testing.T) {
	f := NewFieldFilter(FieldFilterConfig{
		AllowRequestHeaders: []string{"content-type", "authorization"},
		DenyRequestHeaders:  []string{"authorization"},
	})
	hdrs := [][2]string{
		{"content-type", "application/json"},
		{"authorization", "Bearer tok"},
		{"x-custom", "dropped"},
	}
	got := applyHeaderFilter(hdrs, f.allowReqHdrs, f.denyReqHdrs)
	require.Len(t, got, 1)
	assert.Equal(t, "content-type", got[0][0])
}

func TestFieldFilter_CaseInsensitive(t *testing.T) {
	f := NewFieldFilter(FieldFilterConfig{
		DenyRequestHeaders: []string{"Authorization"},
	})
	hdrs := [][2]string{
		{"authorization", "Bearer tok"},
		{"AUTHORIZATION", "Bearer tok2"},
	}
	got := applyHeaderFilter(hdrs, f.allowReqHdrs, f.denyReqHdrs)
	assert.Empty(t, got)
}

func TestFieldFilter_NoRules(t *testing.T) {
	f := NewFieldFilter(FieldFilterConfig{})
	hdrs := [][2]string{
		{"content-type", "application/json"},
		{"authorization", "Bearer tok"},
	}
	got := applyHeaderFilter(hdrs, f.allowReqHdrs, f.denyReqHdrs)
	assert.Equal(t, hdrs, got)
}

func TestApplyBodyFilter_RemovePath(t *testing.T) {
	body := `{"model":"gpt-4","stream":true,"messages":[{"role":"user","content":"hello"}]}`
	got := applyBodyFilter(body, nil, []string{"stream"})
	assert.NotContains(t, got, `"stream"`)
	assert.Contains(t, got, `"model"`)
}

func TestApplyBodyFilter_RedactPath(t *testing.T) {
	body := `{"model":"gpt-4","messages":[{"role":"user","content":"secret prompt"}]}`
	got := applyBodyFilter(body, []string{"messages.0.content"}, nil)
	assert.Contains(t, got, "[REDACTED]")
	assert.NotContains(t, got, "secret prompt")
}

func TestApplyBodyFilter_NonJSON(t *testing.T) {
	body := "not json at all"
	got := applyBodyFilter(body, []string{"foo"}, []string{"bar"})
	assert.Equal(t, body, got)
}

func TestApplyBodyFilter_Empty(t *testing.T) {
	assert.Equal(t, "", applyBodyFilter("", []string{"foo"}, nil))
}

func TestIsError_UpstreamFailure(t *testing.T) {
	r := &Record{UpstreamFailure: "connection refused"}
	assert.True(t, isError(r))
}

func TestIsError_5xx(t *testing.T) {
	r := &Record{StatusCode: 503}
	assert.True(t, isError(r))
}

func TestIsError_4xx(t *testing.T) {
	r := &Record{StatusCode: 404}
	assert.False(t, isError(r))
}

func TestIsError_OK(t *testing.T) {
	r := &Record{StatusCode: 200}
	assert.False(t, isError(r))
}

func TestContainsErrorFlag(t *testing.T) {
	cases := []struct {
		flags string
		want  bool
	}{
		{up.ResponseFlagUpstreamConnectionFailure, true},
		{up.ResponseFlagNoHealthyUpstream, true},
		{up.ResponseFlagUpstreamConnectionTermination, true},
		{up.ResponseFlagUpstreamRequestTimeout, true},
		{up.ResponseFlagUpstreamOverflow, true},
		{up.ResponseFlagNoRouteFound, true},
		{up.ResponseFlagDownstreamConnectionTermination, false},
		{"-", false},
		{"", false},
		// multi-flag: comma-separated
		{up.ResponseFlagUpstreamConnectionFailure + "," + up.ResponseFlagDownstreamConnectionTermination, true},
	}
	for _, c := range cases {
		got := containsErrorFlag(c.flags)
		require.Equal(t, c.want, got, "containsErrorFlag(%q)", c.flags)
	}
}

func TestFieldFilter_Apply_Record(t *testing.T) {
	f := NewFieldFilter(FieldFilterConfig{
		DenyRequestHeaders:  []string{"authorization"},
		DenyResponseHeaders: []string{"set-cookie"},
	})
	r := &Record{
		RequestHeaders: [][2]string{
			{"content-type", "application/json"},
			{"authorization", "Bearer secret"},
		},
		ResponseHeaders: [][2]string{
			{"content-type", "application/json"},
			{"set-cookie", "session=abc"},
		},
	}
	f.Apply(r)
	require.Len(t, r.RequestHeaders, 1)
	assert.Equal(t, "content-type", r.RequestHeaders[0][0])
	require.Len(t, r.ResponseHeaders, 1)
	assert.Equal(t, "content-type", r.ResponseHeaders[0][0])
}
