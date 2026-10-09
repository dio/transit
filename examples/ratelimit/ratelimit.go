// Package ratelimit implements request admission through Envoy's RLS v3 API.
// Configuration delivery is supplied by the embedding application; the request
// path reads one immutable snapshot and sends gRPC only through an Envoy cluster.
package ratelimit

import (
	"fmt"
	"net/http"

	rlscommonv3 "github.com/envoyproxy/go-control-plane/envoy/extensions/common/ratelimit/v3"
	rlsv3 "github.com/envoyproxy/go-control-plane/envoy/service/ratelimit/v3"
	"google.golang.org/protobuf/proto"

	"github.com/dio/transit/up"
)

// DescriptorBuilder assembles request-scoped descriptors from trusted context.
// Return owned protobuf values; do not mutate them after returning. Empty or
// invalid results and errors reject admission, even in service fail-open mode.
type DescriptorBuilder func(*up.Writer, *up.Request) ([]*rlscommonv3.RateLimitDescriptor, error)

// NewHandler creates a header-phase admission filter. The caller owns source
// refresh and shutdown. No config, including a zero Policy, fails closed.
func NewHandler(config *up.PipelineConfig[*Policy]) up.HandlerFunc {
	return func(w *up.Writer, r *up.Request) {
		var policy *Policy
		if config != nil {
			policy = config.Snapshot()
		}
		if policy == nil || policy.config.Cluster == "" {
			reject(w, http.StatusServiceUnavailable, "rate limit config unavailable")
			return
		}
		req, err := policy.requestFor(w, r)
		if err != nil {
			reject(w, http.StatusBadRequest, "rate limit descriptor unavailable")
			return
		}
		body, err := proto.Marshal(req)
		if err != nil {
			reject(w, http.StatusInternalServerError, "rate limit request encoding failed")
			return
		}
		_, err = w.GRPCCallout(up.GRPCCalloutRequest{
			Cluster:       policy.config.Cluster,
			Authority:     policy.config.Authority,
			Method:        rlsv3.RateLimitService_ShouldRateLimit_FullMethodName,
			Message:       body,
			TimeoutMillis: policy.config.TimeoutMillis,
		}, func(response up.GRPCCalloutResponse) {
			// Keep the captured policy, including failure mode, across refreshes.
			switch code := policy.responseStatus(response); code {
			case http.StatusTooManyRequests:
				reject(w, code, "rate limit exceeded")
			case http.StatusServiceUnavailable:
				reject(w, code, "rate limit service unavailable")
			}
		})
		if err != nil && policy.config.FailureMode == "closed" {
			reject(w, http.StatusServiceUnavailable, "rate limit service unavailable")
		}
	}
}

func (p *Policy) requestFor(w *up.Writer, r *up.Request) (*rlsv3.RateLimitRequest, error) {
	if p.config.BuildDescriptors == nil {
		return p.request(r.Header, w.GetFilterState)
	}
	descriptors, err := p.config.BuildDescriptors(w, r)
	if err != nil {
		return nil, err
	}
	if len(descriptors) == 0 {
		return nil, fmt.Errorf("descriptor builder returned no descriptors")
	}
	for _, descriptor := range descriptors {
		if len(descriptor.GetEntries()) == 0 {
			return nil, fmt.Errorf("descriptor builder returned an empty descriptor")
		}
		for _, entry := range descriptor.GetEntries() {
			if entry.GetKey() == "" || entry.GetValue() == "" {
				return nil, fmt.Errorf("descriptor builder returned an invalid entry")
			}
		}
	}
	return &rlsv3.RateLimitRequest{Domain: p.config.Domain, HitsAddend: 1, Descriptors: descriptors}, nil
}

func (p *Policy) request(header func(string) string, state func(string) (string, bool)) (*rlsv3.RateLimitRequest, error) {
	req := &rlsv3.RateLimitRequest{Domain: p.config.Domain, HitsAddend: 1}
	for _, entries := range p.config.Descriptors {
		descriptor := &rlscommonv3.RateLimitDescriptor{}
		for _, entry := range entries {
			value := entry.Value
			if entry.Header != "" {
				value = header(entry.Header)
			}
			if entry.FilterState != "" {
				value, _ = state(entry.FilterState)
			}
			if value == "" {
				return nil, fmt.Errorf("descriptor %q is unavailable", entry.Key)
			}
			descriptor.Entries = append(descriptor.Entries, &rlscommonv3.RateLimitDescriptor_Entry{Key: entry.Key, Value: value})
		}
		req.Descriptors = append(req.Descriptors, descriptor)
	}
	return req, nil
}

func (p *Policy) responseStatus(response up.GRPCCalloutResponse) int {
	if response.Result == up.HTTPCalloutSuccess && response.GRPCStatus == 0 {
		var result rlsv3.RateLimitResponse
		if err := proto.Unmarshal(response.Body, &result); err == nil {
			switch result.GetOverallCode() {
			case rlsv3.RateLimitResponse_OK:
				return 0
			case rlsv3.RateLimitResponse_OVER_LIMIT:
				return http.StatusTooManyRequests
			}
		}
	}
	if p.config.FailureMode == "open" {
		return 0
	}
	return http.StatusServiceUnavailable
}

func reject(w *up.Writer, code int, message string) {
	w.SendLocalResponse(code, []byte(message), [2]string{"content-type", "text/plain"})
}
