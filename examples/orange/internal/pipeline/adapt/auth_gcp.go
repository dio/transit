package adapt

import (
	"context"
	"fmt"
	"strings"

	authlib "cloud.google.com/go/auth"
	"cloud.google.com/go/auth/credentials"

	"github.com/dio/transit/up"
)

const gcpCloudPlatformScope = "https://www.googleapis.com/auth/cloud-platform"

// GCPAuth obtains a short-lived Bearer token and injects it. Token caching
// and refresh are handled internally by *authlib.Credentials.
type GCPAuth struct {
	creds *authlib.Credentials
}

// NewGCPAuth constructs a GCPAuth. The secret argument controls credential
// source and may be:
//
//   - empty — Application Default Credentials: GOOGLE_APPLICATION_CREDENTIALS,
//     gcloud user creds, or Workload Identity.
//   - a file path (secret_ref: file:///abs/path/to/key.json) — path is passed
//     directly to CredentialsFile; the GCP SDK reads and validates the file.
//   - inline JSON (secret_ref: env://GCP_SERVICE_ACCOUNT_JSON, env var holds
//     the key JSON) — JSON bytes are passed to CredentialsJSON.
//
// Both CredentialsFile and CredentialsJSON carry a staticcheck deprecation
// warning about unvalidated credential configs from untrusted sources.
// Orange always sources credentials from operator-controlled env vars or
// file paths, so the warning does not apply here.
func NewGCPAuth(ctx context.Context, secret string) (*GCPAuth, error) {
	opts := &credentials.DetectOptions{Scopes: []string{gcpCloudPlatformScope}}
	switch {
	case strings.HasPrefix(strings.TrimSpace(secret), "{"):
		opts.CredentialsJSON = []byte(secret) //nolint:staticcheck
	case secret != "":
		opts.CredentialsFile = secret //nolint:staticcheck
	}
	creds, err := credentials.DetectDefault(opts)
	if err != nil {
		return nil, fmt.Errorf("orange: GCP credentials not found: %w; "+
			"set GOOGLE_APPLICATION_CREDENTIALS, supply secret_ref with SA JSON (env://), "+
			"or point to a key file (file:///path/to/key.json)", err)
	}
	return &GCPAuth{creds: creds}, nil
}

func (a *GCPAuth) InjectAuth(w *up.Writer) {
	tok, err := a.creds.Token(context.Background())
	if err != nil || !tok.IsValid() {
		return
	}
	w.SetRequestHeader("authorization", "Bearer "+tok.Value)
}
