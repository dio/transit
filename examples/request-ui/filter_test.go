package requestui

import (
	"testing"

	requestuisink "github.com/dio/transit/examples/request-ui/sink"
	"github.com/envoyproxy/envoy/source/extensions/dynamic_modules/sdk/go/shared"
)

// testSink collects records sent by the filter.
type testSink struct {
	records []*requestuisink.Record
}

func (s *testSink) Send(r *requestuisink.Record) { s.records = append(s.records, r) }

var _ interface{ Send(*requestuisink.Record) } = (*testSink)(nil)

func strBuf(str string) shared.UnsafeEnvoyBuffer {
	if str == "" {
		return shared.UnsafeEnvoyBuffer{}
	}
	b := []byte(str)
	return shared.UnsafeEnvoyBuffer{Ptr: &b[0], Len: uint64(len(b))}
}

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
		{"UF", true},
		{"UH", true},
		{"UC", true},
		{"UT", true},
		{"UO", true},
		{"NR", true},
		{"DC", false},
		{"RL", false},
		{"-", false},
		{"", false},
		{"UFDC", true},
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

func TestCopyHeaders(t *testing.T) {
	raw := [][2]shared.UnsafeEnvoyBuffer{
		{strBuf("content-type"), strBuf("application/json")},
		{strBuf(":status"), strBuf("200")},
	}
	got := copyHeaders(raw)
	if len(got) != 2 {
		t.Fatalf("want 2 headers, got %d", len(got))
	}
	if got[0][0] != "content-type" || got[0][1] != "application/json" {
		t.Errorf("header 0: want content-type/application/json, got %v", got[0])
	}
	if got[1][0] != ":status" || got[1][1] != "200" {
		t.Errorf("header 1: want :status/200, got %v", got[1])
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
