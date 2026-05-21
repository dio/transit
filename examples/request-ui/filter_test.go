package requestui

import (
	"testing"

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
		if got != c.want {
			t.Errorf("hasError(%+v): want %v, got %v", c.r, c.want, got)
		}
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
		if got != c.want {
			t.Errorf("containsErrorFlag(%q): want %v, got %v", c.flags, c.want, got)
		}
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
		if got != c.want {
			t.Errorf("statusStr(%d): want %q, got %q", c.code, c.want, got)
		}
	}
}

func TestDefaultConfig(t *testing.T) {
	cfgMu.RLock()
	c := cfg
	cfgMu.RUnlock()

	if !c.RecordRequestHeaders {
		t.Error("want RecordRequestHeaders=true by default")
	}
	if !c.RecordResponseHeaders {
		t.Error("want RecordResponseHeaders=true by default")
	}
	if c.RecordRequestBody {
		t.Error("want RecordRequestBody=false by default")
	}
	if c.MaxBodyBytes != defaultMaxBody {
		t.Errorf("want MaxBodyBytes=%d, got %d", defaultMaxBody, c.MaxBodyBytes)
	}
}

func TestStatePool(t *testing.T) {
	st := statePool.Get().(*reqState)
	st.requestID = "test"
	st.method = "GET"
	*st = reqState{}
	statePool.Put(st)

	st2 := statePool.Get().(*reqState)
	if st2.requestID != "" || st2.method != "" {
		t.Error("pool returned dirty state")
	}
	statePool.Put(st2)
}
