package rls

import (
	"context"
	"log/slog"

	pb "github.com/envoyproxy/go-control-plane/envoy/service/ratelimit/v3"
	rlsconfig "github.com/envoyproxy/ratelimit/src/config"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// Service implements pb.RateLimitServiceServer.
// Wire it with NewPollProvider and NewRateLimiter, register on a grpc.Server,
// and start the provider loop before serving traffic.
type Service struct {
	pb.UnimplementedRateLimitServiceServer
	provider *PollProvider
	limiter  *RateLimiter
}

// NewService creates a Service.
func NewService(p *PollProvider, l *RateLimiter) *Service {
	return &Service{provider: p, limiter: l}
}

// ShouldRateLimit is the hot path: look up limits for each descriptor then
// check and increment Redis counters in a single pipeline.
func (s *Service) ShouldRateLimit(
	ctx context.Context,
	req *pb.RateLimitRequest,
) (*pb.RateLimitResponse, error) {
	cfg := s.provider.Current()
	if cfg == nil {
		return nil, status.Error(codes.Unavailable, "rls: no config loaded")
	}

	limits := make([]*rlsconfig.RateLimit, len(req.Descriptors))
	for i, d := range req.Descriptors {
		limits[i] = cfg.GetLimit(ctx, req.Domain, d)
	}

	statuses := s.limiter.DoLimit(ctx, req, limits)

	overall := pb.RateLimitResponse_OK
	for _, st := range statuses {
		if st.Code == pb.RateLimitResponse_OVER_LIMIT {
			overall = pb.RateLimitResponse_OVER_LIMIT
			break
		}
	}

	slog.Debug("rls: ShouldRateLimit",
		"domain", req.Domain,
		"descriptors", len(req.Descriptors),
		"overall", overall.String(),
	)

	return &pb.RateLimitResponse{
		OverallCode: overall,
		Statuses:    statuses,
	}, nil
}
