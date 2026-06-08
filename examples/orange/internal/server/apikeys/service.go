package apikeys

import (
	"context"
	"fmt"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/types/known/timestamppb"

	apikeyv1 "github.com/dio/transit/examples/orange/api/orange/apikey/admin/v1"
	"github.com/dio/transit/examples/orange/internal/server/scopes"
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

	// A token:issue key must be workspace-scoped and the user must already be a
	// workspace member. Enforce this at issue time so an admin cannot accidentally
	// grant the scope to a user who has no access to the workspace.
	wsID := req.Msg.GetWorkspaceId()
	uID := req.Msg.GetUserId()
	if containsScope(scopes, ScopeTokenIssue) {
		if wsID == "" || uID == "" {
			return nil, connect.NewError(connect.CodeInvalidArgument,
				fmt.Errorf("token:issue keys must have both workspace_id and user_id set"))
		}
		member, err := s.store.IsMember(ctx, wsID, uID)
		if err != nil {
			return nil, connect.NewError(connect.CodeInternal, err)
		}
		if !member {
			return nil, connect.NewError(connect.CodeFailedPrecondition,
				fmt.Errorf("user %q is not a member of workspace %q — add them first with: orange admin member add --workspace-id=%s --user-id=%s", uID, wsID, wsID, uID))
		}
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

func (s *Service) GetKey(ctx context.Context, req *connect.Request[apikeyv1.GetKeyRequest]) (*connect.Response[apikeyv1.GetKeyResponse], error) {
	rec, err := s.store.Get(ctx, req.Msg.GetKeyId())
	if err != nil {
		if err == ErrKeyNotFound {
			return nil, connect.NewError(connect.CodeNotFound, err)
		}
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	return connect.NewResponse(&apikeyv1.GetKeyResponse{Key: recordToProto(rec)}), nil
}

func (s *Service) UpdateKeyScopes(ctx context.Context, req *connect.Request[apikeyv1.UpdateKeyScopesRequest]) (*connect.Response[apikeyv1.UpdateKeyScopesResponse], error) {
	add := req.Msg.GetAddScopes()

	switch req.Msg.GetTemplate() {
	case "ws-member":
		wsID := req.Msg.GetWorkspaceId()
		if wsID == "" {
			return nil, connect.NewError(connect.CodeInvalidArgument,
				fmt.Errorf("workspace_id is required for template=ws-member"))
		}
		// Fetch the key to resolve the user_id so the rl-policy:write scope
		// is scoped to the correct user (ws-id/user-id).
		key, err := s.store.Get(ctx, req.Msg.GetKeyId())
		if err != nil {
			if err == ErrKeyNotFound {
				return nil, connect.NewError(connect.CodeNotFound, err)
			}
			return nil, connect.NewError(connect.CodeInternal, err)
		}
		add = append(add, scopes.WorkspaceMemberScopes(wsID, key.UserID)...)
	case "":
		// no template
	default:
		return nil, connect.NewError(connect.CodeInvalidArgument,
			fmt.Errorf("unknown template %q — supported: ws-member", req.Msg.GetTemplate()))
	}

	if len(add) == 0 {
		return nil, connect.NewError(connect.CodeInvalidArgument,
			fmt.Errorf("no scopes to add: provide --scope or --template"))
	}

	rec, err := s.store.AppendScopes(ctx, req.Msg.GetKeyId(), add)
	if err != nil {
		if err == ErrKeyNotFound {
			return nil, connect.NewError(connect.CodeNotFound, err)
		}
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	return connect.NewResponse(&apikeyv1.UpdateKeyScopesResponse{Key: recordToProto(rec)}), nil
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

func containsScope(scopes []string, target string) bool {
	for _, s := range scopes {
		if s == target {
			return true
		}
	}
	return false
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
