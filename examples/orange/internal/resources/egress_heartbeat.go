package resources

import (
	"context"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/types/known/timestamppb"

	egressv1 "github.com/dio/transit/examples/orange/api/orange/egress/v1"
	egressv1connect "github.com/dio/transit/examples/orange/api/orange/egress/v1/egressv1connect"
)

// HeartbeatService implements egressv1connect.EgressServiceHandler.
// It delegates liveness tracking to HeartbeatRegistry (no DB I/O per call).
type HeartbeatService struct {
	egressv1connect.UnimplementedEgressServiceHandler
	registry *HeartbeatRegistry
}

// NewHeartbeatService wraps a HeartbeatRegistry as a Connect service handler.
func NewHeartbeatService(registry *HeartbeatRegistry) *HeartbeatService {
	return &HeartbeatService{registry: registry}
}

func (s *HeartbeatService) Heartbeat(ctx context.Context, req *connect.Request[egressv1.HeartbeatRequest]) (*connect.Response[egressv1.HeartbeatResponse], error) {
	if err := s.registry.Record(ctx, req.Msg.GetEgressId()); err != nil {
		return nil, err
	}
	return connect.NewResponse(&egressv1.HeartbeatResponse{
		ServerTime: timestamppb.New(time.Now().UTC()),
	}), nil
}
