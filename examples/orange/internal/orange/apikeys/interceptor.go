package apikeys

import (
	"context"
	"errors"
	"strings"

	"connectrpc.com/connect"
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

// Interceptor returns a Connect unary interceptor that validates admin API keys.
// Every request must carry Authorization: Bearer sk-org-… (or sk-… for other scopes).
func Interceptor(store *Store) connect.UnaryInterceptorFunc {
	return func(next connect.UnaryFunc) connect.UnaryFunc {
		return func(ctx context.Context, req connect.AnyRequest) (connect.AnyResponse, error) {
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
			return next(WithRecord(ctx, rec), req)
		}
	}
}
