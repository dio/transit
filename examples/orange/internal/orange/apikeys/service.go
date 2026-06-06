package apikeys

import (
	"context"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/types/known/timestamppb"

	apikeyv1 "github.com/dio/transit/examples/orange/api/orange/apikey/admin/v1"
)

// Service implements APIKeyAdminServiceHandler on top of Store.
type Service struct {
	store *Store
}

// NewService wraps a Store as a Connect-compatible service handler.
func NewService(store *Store) *Service {
	return &Service{store: store}
}

func (s *Service) IssueKey(ctx context.Context, req *connect.Request[apikeyv1.IssueKeyRequest]) (*connect.Response[apikeyv1.IssueKeyResponse], error) {
	scopes := req.Msg.GetScopes()
	if len(scopes) == 0 {
		scopes = DefaultUserScopes
	}
	var desc string
	if d := req.Msg.Description; d != nil {
		desc = *d
	}
	plaintext, rec, err := s.store.Issue(ctx,
		req.Msg.GetOrgId(),
		req.Msg.GetUserId(),
		req.Msg.GetWorkspaceId(),
		scopes,
		desc,
	)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	return connect.NewResponse(&apikeyv1.IssueKeyResponse{
		Key:       recordToProto(rec),
		Plaintext: plaintext,
	}), nil
}

func (s *Service) ListKeys(ctx context.Context, req *connect.Request[apikeyv1.ListKeysRequest]) (*connect.Response[apikeyv1.ListKeysResponse], error) {
	recs, err := s.store.List(ctx, req.Msg.GetOrgId(), req.Msg.GetUserId())
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	keys := make([]*apikeyv1.ApiKey, len(recs))
	for i, r := range recs {
		keys[i] = recordToProto(r)
	}
	return connect.NewResponse(&apikeyv1.ListKeysResponse{Keys: keys}), nil
}

func (s *Service) RevokeKey(ctx context.Context, req *connect.Request[apikeyv1.RevokeKeyRequest]) (*connect.Response[apikeyv1.RevokeKeyResponse], error) {
	if err := s.store.Revoke(ctx, req.Msg.GetKeyId()); err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	return connect.NewResponse(&apikeyv1.RevokeKeyResponse{}), nil
}

func recordToProto(r Record) *apikeyv1.ApiKey {
	k := &apikeyv1.ApiKey{
		KeyId:       r.KeyID,
		KeyPrefix:   r.KeyPrefix,
		OrgId:       r.OrgID,
		UserId:      r.UserID,
		WorkspaceId: r.WorkspaceID,
		Scopes:      r.Scopes,
		CreatedAt:   timestamppb.New(r.CreatedAt),
	}
	if r.Description != "" {
		k.Description = &r.Description
	}
	return k
}
