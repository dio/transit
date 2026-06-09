package grpc_callout

import (
	"testing"

	rlsv3 "github.com/envoyproxy/go-control-plane/envoy/service/ratelimit/v3"
	"github.com/stretchr/testify/require"
)

func TestBuildRateLimitRequest(t *testing.T) {
	req := BuildRateLimitRequest("alice")

	require.Equal(t, rlsDomain, req.GetDomain())
	require.EqualValues(t, 1, req.GetHitsAddend())
	require.Len(t, req.GetDescriptors(), 1)
	require.Len(t, req.GetDescriptors()[0].GetEntries(), 1)
	require.Equal(t, descriptorKey, req.GetDescriptors()[0].GetEntries()[0].GetKey())
	require.Equal(t, "alice", req.GetDescriptors()[0].GetEntries()[0].GetValue())
}

func TestIsOverLimit(t *testing.T) {
	require.False(t, IsOverLimit(&rlsv3.RateLimitResponse{OverallCode: rlsv3.RateLimitResponse_OK}))
	require.True(t, IsOverLimit(&rlsv3.RateLimitResponse{OverallCode: rlsv3.RateLimitResponse_OVER_LIMIT}))
}
