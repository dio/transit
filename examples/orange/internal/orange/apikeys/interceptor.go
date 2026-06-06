package apikeys

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/reflect/protoregistry"

	authv1 "github.com/dio/transit/examples/orange/api/orange/auth/v1"
)

type contextKey struct{}

// WithRecord attaches a validated Record to ctx.
func WithRecord(ctx context.Context, rec Record) context.Context {
	return context.WithValue(ctx, contextKey{}, rec)
}

// RecordFromContext retrieves the validated Record. Returns zero value if absent.
func RecordFromContext(ctx context.Context) (Record, bool) {
	rec, ok := ctx.Value(contextKey{}).(Record)
	return rec, ok
}

// policyMap maps Connect procedure path (e.g. "/orange.org.admin.v1.OrgAdminService/CreateOrg")
// to the auth options declared on that method via (orange.auth.v1.auth).
// Methods without the annotation are absent from the map (public).
type policyMap map[string]*authv1.AuthOptions

// buildPolicyMap walks all globally registered proto files once and extracts
// every (orange.auth.v1.auth) annotation. Called once at interceptor creation.
func buildPolicyMap() policyMap {
	m := make(policyMap)
	protoregistry.GlobalFiles.RangeFiles(func(fd protoreflect.FileDescriptor) bool {
		svcs := fd.Services()
		for i := range svcs.Len() {
			svc := svcs.Get(i)
			methods := svc.Methods()
			for j := range methods.Len() {
				method := methods.Get(j)
				if method.Options() == nil {
					continue
				}
				opts, _ := proto.GetExtension(method.Options(), authv1.E_Auth).(*authv1.AuthOptions)
				if opts == nil {
					continue
				}
				procedure := "/" + string(svc.FullName()) + "/" + string(method.Name())
				m[procedure] = opts
			}
		}
		return true
	})
	return m
}

// Interceptor returns a Connect unary interceptor that validates API keys and
// enforces the scopes declared via (orange.auth.v1.auth) on each RPC method.
//
// The policy map is built once from the global proto registry at interceptor
// creation (static, not lazy). Methods without an auth annotation are public.
func Interceptor(store *Store) connect.UnaryInterceptorFunc {
	policy := buildPolicyMap()

	return func(next connect.UnaryFunc) connect.UnaryFunc {
		return func(ctx context.Context, req connect.AnyRequest) (connect.AnyResponse, error) {
			opts, protected := policy[req.Spec().Procedure]

			// No annotation → public endpoint.
			if !protected {
				return next(ctx, req)
			}

			// Validate credential.
			raw := req.Header().Get("Authorization")
			token, ok := strings.CutPrefix(raw, "Bearer ")
			if !ok || !strings.HasPrefix(token, "sk-") {
				return nil, connect.NewError(connect.CodeUnauthenticated,
					errors.New("missing or malformed Authorization header (expected Bearer sk-…)"))
			}
			rec, err := store.Validate(ctx, token)
			if err != nil {
				if errors.Is(err, ErrInvalidKey) {
					return nil, connect.NewError(connect.CodeUnauthenticated, err)
				}
				return nil, connect.NewError(connect.CodeInternal, err)
			}

			// Check auth_type: the presented API key must be an accepted credential type.
			if !acceptsAPIKey(opts) {
				return nil, connect.NewError(connect.CodeUnauthenticated,
					fmt.Errorf("API key authentication not accepted for %s", req.Spec().Procedure))
			}

			// Check all required scopes.
			for _, required := range opts.GetScopes() {
				if !rec.HasScope(required) {
					return nil, connect.NewError(connect.CodePermissionDenied,
						fmt.Errorf("scope %q required for %s", required, req.Spec().Procedure))
				}
			}

			return next(WithRecord(ctx, rec), req)
		}
	}
}

// acceptsAPIKey returns true when AUTH_TYPE_API_KEY is listed, or auth_types is
// empty (meaning any credential type is accepted).
func acceptsAPIKey(opts *authv1.AuthOptions) bool {
	if len(opts.GetAuthTypes()) == 0 {
		return true
	}
	for _, t := range opts.GetAuthTypes() {
		if t == authv1.AuthType_AUTH_TYPE_API_KEY {
			return true
		}
	}
	return false
}
