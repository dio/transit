package ratelimit

import (
	"fmt"
	"net/http"
	"testing"

	rlscommonv3 "github.com/envoyproxy/go-control-plane/envoy/extensions/common/ratelimit/v3"
	rlsv3 "github.com/envoyproxy/go-control-plane/envoy/service/ratelimit/v3"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"

	"github.com/dio/transit/up"
	"github.com/dio/transit/up/testutil"
)

func TestCodeBuilderReadsTrustedMetadata(t *testing.T) {
	c := validConfig()
	c.Descriptors = nil
	c.BuildDescriptors = func(w *up.Writer, _ *up.Request) ([]*rlscommonv3.RateLimitDescriptor, error) {
		id, ok := w.GetMetadataString(up.MetadataSourceDynamic, "tars", "router_id")
		if !ok {
			return nil, fmt.Errorf("missing trusted router identity")
		}
		return []*rlscommonv3.RateLimitDescriptor{{Entries: []*rlscommonv3.RateLimitDescriptor_Entry{
			{Key: "key_id", Value: id.String()}, {Key: "dim", Value: "rpm"},
		}}}, nil
	}
	p, err := Compile(c)
	require.NoError(t, err)
	h := testutil.NewFilterHandle(testutil.WithHeaders(map[string]string{"x-tars-id": "spoofed"}))
	h.SetMetadata("tars", "router_id", "trusted")
	req, err := p.requestFor(up.NewWriter(h), up.NewRequest(h.RequestHeaders(), "ratelimit"))
	require.NoError(t, err)
	require.Equal(t, "trusted", req.Descriptors[0].Entries[0].Value)
	require.Equal(t, "orange", req.Domain)
	require.EqualValues(t, 1, req.HitsAddend)
	missing := testutil.NewFilterHandle()
	_, err = p.requestFor(up.NewWriter(missing), up.NewRequest(missing.RequestHeaders(), "ratelimit"))
	require.Error(t, err)
	c.Descriptors = validConfig().Descriptors
	_, err = Compile(c)
	require.Error(t, err, "must not silently combine code and configured descriptors")
}

func TestCodeBuilderCannotSilentlySkipAdmission(t *testing.T) {
	for _, result := range [][]*rlscommonv3.RateLimitDescriptor{
		nil, {nil}, {{}}, {{Entries: []*rlscommonv3.RateLimitDescriptor_Entry{nil}}},
		{{Entries: []*rlscommonv3.RateLimitDescriptor_Entry{{Key: "key", Value: ""}}}},
	} {
		c := validConfig()
		c.Descriptors = nil
		c.FailureMode = "open"
		c.BuildDescriptors = func(*up.Writer, *up.Request) ([]*rlscommonv3.RateLimitDescriptor, error) { return result, nil }
		p, err := Compile(c)
		require.NoError(t, err)
		h := testutil.NewFilterHandle()
		NewHandler(up.NewStaticConfig(p))(up.NewWriter(h), up.NewRequest(h.RequestHeaders(), "ratelimit"))
		require.Len(t, h.LocalResponses, 1)
		require.EqualValues(t, 400, h.LocalResponses[0].Status)
	}
}

func TestDescriptorSources(t *testing.T) {
	c := validConfig()
	c.Descriptors = append(c.Descriptors, []Entry{{Key: "tenant", FilterState: "auth.tenant"}})
	p, err := Compile(c)
	require.NoError(t, err)
	req, err := p.request(func(name string) string {
		require.Equal(t, "x-tars-id", name)
		return "alice"
	}, func(name string) (string, bool) {
		require.Equal(t, "auth.tenant", name)
		return "tenant-a", true
	})
	require.NoError(t, err)
	require.Equal(t, "orange", req.Domain)
	require.EqualValues(t, 1, req.HitsAddend)
	require.Len(t, req.Descriptors, 2)
	require.Equal(t, "alice", req.Descriptors[0].Entries[0].Value)
	require.Equal(t, "rpm", req.Descriptors[0].Entries[1].Value)
	require.Equal(t, "tenant-a", req.Descriptors[1].Entries[0].Value)
	_, err = p.request(func(string) string { return "alice" }, func(string) (string, bool) { return "", false })
	require.Error(t, err)
}

func TestResponseDecisions(t *testing.T) {
	ok, err := proto.Marshal(&rlsv3.RateLimitResponse{OverallCode: rlsv3.RateLimitResponse_OK})
	require.NoError(t, err)
	denied, err := proto.Marshal(&rlsv3.RateLimitResponse{OverallCode: rlsv3.RateLimitResponse_OVER_LIMIT})
	require.NoError(t, err)
	for _, mode := range []string{"closed", "open"} {
		c := validConfig()
		c.FailureMode = mode
		p, err := Compile(c)
		require.NoError(t, err)
		failure := http.StatusServiceUnavailable
		if mode == "open" {
			failure = 0
		}
		for _, tc := range []struct {
			name     string
			response up.GRPCCalloutResponse
			want     int
		}{
			{"allow", up.GRPCCalloutResponse{Body: ok}, 0},
			{"deny", up.GRPCCalloutResponse{Body: denied}, http.StatusTooManyRequests},
			{"reset", up.GRPCCalloutResponse{Result: up.HTTPCalloutReset}, failure},
			{"grpc error", up.GRPCCalloutResponse{GRPCStatus: 14, Body: ok}, failure},
			{"malformed", up.GRPCCalloutResponse{Body: []byte{0xff}}, failure},
			{"empty unknown", up.GRPCCalloutResponse{}, failure},
		} {
			t.Run(mode+"/"+tc.name, func(t *testing.T) { require.Equal(t, tc.want, p.responseStatus(tc.response)) })
		}
	}
}

func TestMissingConfigAndDescriptorRejectBeforeCallout(t *testing.T) {
	p, err := Compile(validConfig())
	require.NoError(t, err)
	for _, tc := range []struct {
		name   string
		pc     *up.PipelineConfig[*Policy]
		status int
	}{
		{"missing config", nil, 503},
		{"zero policy", up.NewStaticConfig(&Policy{}), 503},
		{"missing descriptor", up.NewStaticConfig(p), 400},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := testutil.NewFilterHandle()
			NewHandler(tc.pc)(up.NewWriter(h), up.NewRequest(h.RequestHeaders(), "ratelimit"))
			require.Len(t, h.LocalResponses, 1)
			require.EqualValues(t, tc.status, h.LocalResponses[0].Status)
		})
	}
}
