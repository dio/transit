package egressauth

import (
	"context"
	"crypto/ed25519"
	"crypto/x509"
	"encoding/base64"
	"encoding/pem"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/proto"

	authv1 "github.com/dio/transit/examples/orange/api/orange/auth/v1"
	"github.com/dio/transit/examples/orange/internal/orange/auth"
)

// KeyLookup can fetch the active Ed25519 public key PEM for an egress.
type KeyLookup interface {
	ActivePublicKeyPEM(ctx context.Context, egressID string) (string, error)
}

type contextKey struct{}

// EgressIdentity holds the authenticated egress identity extracted from the assertion.
type EgressIdentity struct {
	EgressID    string
	WorkspaceID string
}

// WithEgressIdentity attaches an EgressIdentity to ctx.
func WithEgressIdentity(ctx context.Context, id EgressIdentity) context.Context {
	return context.WithValue(ctx, contextKey{}, id)
}

// EgressIdentityFromContext retrieves the EgressIdentity. Returns zero value if absent.
func EgressIdentityFromContext(ctx context.Context) (EgressIdentity, bool) {
	id, ok := ctx.Value(contextKey{}).(EgressIdentity)
	return id, ok
}

// acceptsEgressAssertion returns true when AUTH_TYPE_EGRESS_ASSERTION is listed
// in the auth_types, or auth_types is empty (meaning any credential type is accepted).
func acceptsEgressAssertion(opts *authv1.AuthOptions) bool {
	if len(opts.GetAuthTypes()) == 0 {
		return true
	}
	return slices.Contains(opts.GetAuthTypes(), authv1.AuthType_AUTH_TYPE_EGRESS_ASSERTION)
}

// Interceptor returns a Connect unary interceptor that validates egress assertions
// and enforces the scopes declared via (orange.auth.v1.auth) on each RPC method.
//
// The assertion format is:
//
//	X-Egress-Assertion: <egress_id>.<base64url(sig)>.<unix_ts>
//
// The signed message is:
//
//	"egress:<egress_id>:<workspace_id>:<unix_ts>"
//
// The signature is Ed25519 over the egress's active signing keypair public key
// (stored in egress_keypairs.public_key_pem).
//
// Methods without an auth annotation are public. Methods with an annotation
// that does not include AUTH_TYPE_EGRESS_ASSERTION are passed through
// (other interceptors handle their types).
//
// EXAMPLE: When an RPC endpoint is annotated with:
//
//	rpc Heartbeat(HeartbeatRequest) returns (HeartbeatResponse) {
//	  option (orange.auth.v1.auth) = {
//	    auth_types: [AUTH_TYPE_EGRESS_ASSERTION]
//	  };
//	}
//
// This interceptor will:
// 1. Extract the X-Egress-Assertion header from the request
// 2. Parse it into: egress_id, signature (base64url), and unix timestamp
// 3. Verify the timestamp is within ±30 seconds (replay prevention)
// 4. Look up the egress's active Ed25519 public key from the database
// 5. Extract workspace_id from the request body (HeartbeatRequest.workspace_id)
// 6. Reconstruct the signed message: "egress:egress-id:workspace-id:timestamp"
// 7. Verify the Ed25519 signature using the public key
// 8. Attach EgressIdentity to context for downstream handlers
// 9. Pass the request to the next handler (no "egress" scope check is performed;
//    signature verification IS the authorization)
func Interceptor(store KeyLookup) connect.UnaryInterceptorFunc {
	// Build the policy map once: maps procedure paths (e.g., "/orange.egress.v1.EgressService/Heartbeat")
	// to their auth options extracted from proto annotations.
	policy := auth.BuildPolicyMap()

	return func(next connect.UnaryFunc) connect.UnaryFunc {
		return func(ctx context.Context, req connect.AnyRequest) (connect.AnyResponse, error) {
			// Look up the procedure's auth options from the policy map.
			// If the procedure is not in the map, it has no auth annotation (public).
			opts, protected := policy[req.Spec().Procedure]

			// STEP 1: Check if endpoint has an auth annotation.
			// If not annotated, this is a public endpoint — skip auth.
			if !protected {
				return next(ctx, req)
			}

			// STEP 2: Check if the annotation includes AUTH_TYPE_EGRESS_ASSERTION.
			// If the endpoint requires a different auth type (e.g., AUTH_TYPE_API_KEY),
			// pass through — other interceptors will handle it.
			if !acceptsEgressAssertion(opts) {
				// Other auth types (e.g., apikeys.Interceptor) handle this endpoint.
				return next(ctx, req)
			}

			// STEP 3: Parse and validate the X-Egress-Assertion header.
			// Format: <egress_id>.<base64url(signature)>.<unix_timestamp>
			assertion := req.Header().Get("X-Egress-Assertion")
			if assertion == "" {
				return nil, connect.NewError(connect.CodeUnauthenticated,
					errors.New("missing X-Egress-Assertion header"))
			}

			// STEP 4: Split the assertion into its three components.
			parts := strings.SplitN(assertion, ".", 3)
			if len(parts) != 3 {
				return nil, connect.NewError(connect.CodeUnauthenticated,
					errors.New("malformed X-Egress-Assertion header (expected <egress_id>.<sig>.<ts>)"))
			}
			egressID, sigB64url, tsStr := parts[0], parts[1], parts[2]

			// STEP 5: Parse timestamp and enforce clock window (±30 seconds).
			// This is the primary replay prevention mechanism: assertions older than
			// 30 seconds are rejected, preventing indefinite replay if the header is captured.
			ts, err := strconv.ParseInt(tsStr, 10, 64)
			if err != nil {
				return nil, connect.NewError(connect.CodeUnauthenticated,
					fmt.Errorf("invalid timestamp in assertion: %w", err))
			}
			now := time.Now().Unix()
			if absDiff(now, ts) > 30 {
				return nil, connect.NewError(connect.CodeUnauthenticated,
					fmt.Errorf("assertion timestamp outside acceptance window (diff=%d seconds)", absDiff(now, ts)))
			}

			// STEP 6: Fetch the active Ed25519 public key for this egress from the database.
			// The query joins egress_keypairs with egresses to ensure we use the currently
			// active keypair (the one referenced by egresses.keypair_id).
			pubKeyPEM, err := store.ActivePublicKeyPEM(ctx, egressID)
			if err != nil {
				if errors.Is(err, ErrNotFound) {
					return nil, connect.NewError(connect.CodeUnauthenticated,
						fmt.Errorf("egress %q not found or has no active keypair", egressID))
				}
				return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("lookup public key: %w", err))
			}

			// STEP 7: Parse the public key from PEM format.
			// PEM-encoded Ed25519 public keys are in PKIX format.
			pubKey, err := parseEd25519PublicKeyPEM(pubKeyPEM)
			if err != nil {
				return nil, connect.NewError(connect.CodeInternal,
					fmt.Errorf("parse public key: %w", err))
			}

			// STEP 8: Extract workspace_id from the request body.
			// All egress-facing RPC request messages have a workspace_id field.
			// The emulator sends it so the signed message can include it for workspace binding.
			msg := req.Any().(proto.Message)
			workspaceID, err := extractWorkspaceID(msg)
			if err != nil {
				return nil, connect.NewError(connect.CodeUnauthenticated,
					fmt.Errorf("missing workspace_id in request: %w", err))
			}

			// STEP 9: Reconstruct the signed message.
			// The client (emulator) signed: "egress:<egress_id>:<workspace_id>:<unix_ts>"
			// We reconstruct it identically to verify the signature.
			// Workspace binding prevents a compromised egress key from being used to
			// fetch another workspace's config.
			signedMsg := fmt.Sprintf("egress:%s:%s:%s", egressID, workspaceID, tsStr)

			// STEP 10: Decode the base64url-encoded signature.
			sig, err := base64.RawURLEncoding.DecodeString(sigB64url)
			if err != nil {
				return nil, connect.NewError(connect.CodeUnauthenticated,
					fmt.Errorf("invalid base64 signature: %w", err))
			}

			// STEP 11: Verify the Ed25519 signature.
			// If the signature is invalid, the request is rejected with CodeUnauthenticated.
			// This is the core authorization check: only the egress holding the private key
			// can create valid signatures.
			if !ed25519.Verify(pubKey, []byte(signedMsg), sig) {
				return nil, connect.NewError(connect.CodeUnauthenticated,
					errors.New("invalid signature"))
			}

			// STEP 12: Attach the authenticated egress identity to the context.
			// Downstream handlers can retrieve this via EgressIdentityFromContext(ctx)
			// to access the authenticated egress_id and workspace_id if needed.
			ctx = WithEgressIdentity(ctx, EgressIdentity{
				EgressID:    egressID,
				WorkspaceID: workspaceID,
			})

			// STEP 13: Invoke the handler with the authenticated context.
			// The request is authorized and the authenticated identity is available.
			return next(ctx, req)
		}
	}
}

// absDiff returns the absolute difference between two int64 values.
func absDiff(a, b int64) int64 {
	if a > b {
		return a - b
	}
	return b - a
}

// extractWorkspaceID extracts the workspace_id from the request message.
// Both WatchRequest and FetchRequest (config service) and HeartbeatRequest (egress service)
// have a workspace_id field as their second field.
func extractWorkspaceID(msg proto.Message) (string, error) {
	// Use reflection to get the field value.
	if msg == nil {
		return "", errors.New("request message is nil")
	}
	fields := msg.ProtoReflect().Descriptor().Fields()
	for i := range fields.Len() {
		field := fields.Get(i)
		if field.Name() == "workspace_id" {
			val := msg.ProtoReflect().Get(field)
			return val.String(), nil
		}
	}
	return "", errors.New("workspace_id field not found in request")
}

// parseEd25519PublicKeyPEM decodes an Ed25519 public key from PEM format.
func parseEd25519PublicKeyPEM(pemStr string) (ed25519.PublicKey, error) {
	if pemStr == "" {
		return nil, errors.New("empty public key PEM")
	}
	block, _ := pem.Decode([]byte(pemStr))
	if block == nil {
		return nil, errors.New("no PEM block found")
	}
	if block.Type != "PUBLIC KEY" {
		return nil, fmt.Errorf("expected PUBLIC KEY block, got %s", block.Type)
	}
	// Parse PKIX (X.509) public key format.
	pubInterface, err := x509.ParsePKIXPublicKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("parse PKIX: %w", err)
	}
	pubKey, ok := pubInterface.(ed25519.PublicKey)
	if !ok {
		return nil, fmt.Errorf("expected Ed25519 public key, got %T", pubInterface)
	}
	return pubKey, nil
}
