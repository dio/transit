package requestui

import (
	"testing"

	"github.com/stretchr/testify/require"

	requestuisink "github.com/dio/transit/examples/request-ui/sink"
	"github.com/dio/transit/up"
)

// testSink collects records sent by the filter.
type testSink struct {
	records []*requestuisink.Record
}

func (s *testSink) Send(r *requestuisink.Record) { s.records = append(s.records, r) }

var _ interface{ Send(*requestuisink.Record) } = (*testSink)(nil)

func TestHasError(t *testing.T) {
	cases := []struct {
		r    requestuisink.Record
		want bool
	}{
		{requestuisink.Record{}, false},
		{requestuisink.Record{ErrorDetails: "upstream_reset"}, true},
		{requestuisink.Record{UpstreamFailure: "tls"}, true},
		{requestuisink.Record{ResponseFlags: "UF"}, true},
		{requestuisink.Record{ResponseFlags: "UT"}, true},
		{requestuisink.Record{ResponseFlags: "DC"}, false},
		{requestuisink.Record{ResponseCode: 500}, true},
		{requestuisink.Record{ResponseCode: 200}, false},
		{requestuisink.Record{ResponseFlags: "-"}, false},
	}
	for _, c := range cases {
		got := hasError(&c.r)
		require.Equal(t, c.want, got, "hasError(%+v)", c.r)
	}
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
		{up.ResponseFlagRateLimited, false},
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

func TestStatusStr(t *testing.T) {
	cases := []struct {
		code int
		want string
	}{
		{0, ""},
		{200, "200"},
		{404, "404"},
		{503, "503"},
	}
	for _, c := range cases {
		got := statusStr(c.code)
		require.Equal(t, c.want, got, "statusStr(%d)", c.code)
	}
}

func TestDefaultConfig(t *testing.T) {
	cfgMu.RLock()
	c := cfg
	cfgMu.RUnlock()

	require.True(t, c.RecordRequestHeaders, "want RecordRequestHeaders=true by default")
	require.True(t, c.RecordResponseHeaders, "want RecordResponseHeaders=true by default")
	require.False(t, c.RecordRequestBody, "want RecordRequestBody=false by default")
	require.Equal(t, defaultMaxBody, c.MaxBodyBytes)
}
