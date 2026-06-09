// Package grpc_callout demonstrates using up.GRPCCallout to call a rate-limit
// service (envoy.service.ratelimit.v3.RateLimitService/ShouldRateLimit) from a
// request body handler. The filter extracts a key from the raw request body,
// sends it to the rate-limit service as a descriptor entry, and rejects requests
// that are over the limit with HTTP 429.
//
// Envoy bootstrap must define a cluster named "rls-service" pointing at the
// rate-limit service endpoint with http2_protocol_options enabled (h2c or TLS).
package grpc_callout

import (
	"net/http"

	rlscommonv3 "github.com/envoyproxy/go-control-plane/envoy/extensions/common/ratelimit/v3"
	rlsv3 "github.com/envoyproxy/go-control-plane/envoy/service/ratelimit/v3"
	"google.golang.org/protobuf/proto"

	"github.com/dio/transit/up"
)

const (
	filterName     = "grpc-callout"
	rlsClusterName = "rls-service"
	rlsDomain      = "transit-grpc-callout"
	descriptorKey  = "body-key"
)

func init() {
	up.Register(filterName, headersHandler, up.WithMutableBody(bodyHandler))
}

func headersHandler(_ *up.Writer, _ *up.Request) {}

func bodyHandler(w *up.Writer, chunk *up.BodyChunk) {
	if !chunk.EndStream {
		return
	}

	key := string(chunk.Data)

	req := BuildRateLimitRequest(key)
	reqBytes, err := proto.Marshal(req)
	if err != nil {
		w.SendLocalResponse(http.StatusInternalServerError,
			[]byte("rls marshal error"),
			[2]string{"content-type", "text/plain"})
		return
	}

	_, err = w.GRPCCallout(up.GRPCCalloutRequest{
		Cluster:       rlsClusterName,
		Method:        rlsv3.RateLimitService_ShouldRateLimit_FullMethodName,
		Message:       reqBytes,
		TimeoutMillis: 1000,
	}, func(resp up.GRPCCalloutResponse) {
		if resp.Result != up.HTTPCalloutSuccess || resp.GRPCStatus != 0 {
			// Fail open: if the RLS is unavailable, let the request through.
			return
		}
		var rlsResp rlsv3.RateLimitResponse
		if err := proto.Unmarshal(resp.Body, &rlsResp); err != nil {
			return
		}
		if IsOverLimit(&rlsResp) {
			w.SendLocalResponse(http.StatusTooManyRequests,
				[]byte("rate limit exceeded"),
				[2]string{"content-type", "text/plain"},
				[2]string{"x-rls-status", "over-limit"},
			)
		}
	})
	if err != nil {
		// Fail open: cluster not found or other init error.
		return
	}
}

// BuildRateLimitRequest creates the Envoy RLS v3 request used by the example.
func BuildRateLimitRequest(value string) *rlsv3.RateLimitRequest {
	return &rlsv3.RateLimitRequest{
		Domain: rlsDomain,
		Descriptors: []*rlscommonv3.RateLimitDescriptor{
			{
				Entries: []*rlscommonv3.RateLimitDescriptor_Entry{
					{Key: descriptorKey, Value: value},
				},
			},
		},
		HitsAddend: 1,
	}
}

// IsOverLimit reports whether the RLS response should reject the request.
func IsOverLimit(resp *rlsv3.RateLimitResponse) bool {
	return resp.GetOverallCode() == rlsv3.RateLimitResponse_OVER_LIMIT
}
