package secret

// service_resolve.go — SecretResolverService RPC handlers.
//
// Resolve is the hot path: liborange.so calls it to fetch plaintext for
// orange://<workspace_id>/<realm>/<secret_id> references at request time.
// Watch and Fetch are stubbed; use Resolve for now.

import (
	"context"
	"errors"

	"connectrpc.com/connect"

	secretv1 "github.com/dio/transit/examples/orange/api/orange/secret/v1"
	"github.com/dio/transit/examples/orange/api/orange/secret/v1/secretv1connect"
	"github.com/dio/transit/examples/orange/internal/server/egressauth"
)

// Compile-time assertion.
var _ secretv1connect.SecretResolverServiceHandler = (*Service)(nil)

// Resolve returns plaintext material for (realm, secret_id).
// workspace_id is derived from the caller's egress assertion; realm is the
// short purpose string (e.g. "api-keys") and is expanded to the canonical
// "ws/<uuid>/api-keys" form before lookup.
func (s *Service) Resolve(ctx context.Context, req *connect.Request[secretv1.ResolveRequest]) (*connect.Response[secretv1.ResolveResponse], error) {
	identity, ok := egressauth.EgressIdentityFromContext(ctx)
	if !ok || identity.WorkspaceID == "" {
		return nil, connect.NewError(connect.CodeUnauthenticated, errors.New("missing egress identity in context"))
	}

	realm := req.Msg.GetRealm()
	secretID := req.Msg.GetSecretId()
	if realm == "" || secretID == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("realm and secret_id are required"))
	}

	canonicalRealm := "ws/" + identity.WorkspaceID + "/" + realm

	material, _, checksum, err := s.ResolveSecret(ctx, canonicalRealm, secretID, identity.WorkspaceID, identity.ProjectID, identity.OrgID)
	if err != nil {
		return nil, connect.NewError(connect.CodeNotFound, err)
	}

	if cs := req.Msg.GetIfChecksum(); len(cs) > 0 && string(cs) == checksum {
		return connect.NewResponse(&secretv1.ResolveResponse{
			Result: &secretv1.ResolveResponse_Unchanged{Unchanged: &secretv1.Unchanged{}},
		}), nil
	}

	return connect.NewResponse(&secretv1.ResolveResponse{
		Result: &secretv1.ResolveResponse_Payload{
			Payload: &secretv1.SecretPayload{Material: material},
		},
	}), nil
}

// Watch is not yet implemented.
func (s *Service) Watch(_ context.Context, _ *connect.Request[secretv1.WatchRequest], _ *connect.ServerStream[secretv1.WatchResponse]) error {
	return connect.NewError(connect.CodeUnimplemented, errors.New("Watch is not yet implemented; use Resolve directly"))
}

// Fetch is not yet implemented.
func (s *Service) Fetch(_ context.Context, _ *connect.Request[secretv1.FetchRequest]) (*connect.Response[secretv1.FetchResponse], error) {
	return nil, connect.NewError(connect.CodeUnimplemented, errors.New("Fetch is not yet implemented"))
}
