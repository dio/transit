package adapt

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/url"
	"time"

	authlib "cloud.google.com/go/auth"
	"cloud.google.com/go/auth/credentials"
	"github.com/aws/aws-sdk-go-v2/aws"
	v4 "github.com/aws/aws-sdk-go-v2/aws/signer/v4"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"

	"github.com/dio/transit/up"
)

// backendAuthHandler injects provider credentials into an outgoing request.
type backendAuthHandler interface {
	InjectAuth(w *up.Writer)
}

// BodyAwareAuthHandler is implemented by auth handlers that need the final
// translated body to compute their credentials (e.g., AWS SigV4).
// Called in the RequestBody phase, after the translator has produced the
// final body and :path.
type BodyAwareAuthHandler interface {
	InjectAuthWithBody(w *up.Writer, req SigningRequest) error
}

// SigningRequest carries the data a body-aware auth handler needs to sign.
type SigningRequest struct {
	Method string // always "POST" for LLM inference APIs
	Path   string // final upstream :path after translator rewrite
	Host   string // upstream hostname (no scheme)
	Body   []byte // final translated body; nil means original passes through
}

type noAuth struct{}

func (noAuth) InjectAuth(_ *up.Writer) {}

// BearerAuth sets Authorization: Bearer <Token>.
type BearerAuth struct{ Token string }

func (a BearerAuth) InjectAuth(w *up.Writer) {
	if a.Token != "" {
		w.SetRequestHeader("authorization", "Bearer "+a.Token)
	}
}

// APIKeyAuth sets a custom header to the given key value.
type APIKeyAuth struct {
	Header string
	Key    string
}

func (a APIKeyAuth) InjectAuth(w *up.Writer) {
	if a.Header != "" && a.Key != "" {
		w.SetRequestHeader(a.Header, a.Key)
	}
}

// AnthropicAuth sets x-api-key and anthropic-version, the two headers required
// by the Anthropic Messages API.
type AnthropicAuth struct {
	APIKey  string
	Version string
}

func (a AnthropicAuth) InjectAuth(w *up.Writer) {
	if a.APIKey != "" {
		w.SetRequestHeader("x-api-key", a.APIKey)
	}
	if a.Version != "" {
		w.SetRequestHeader("anthropic-version", a.Version)
	}
}

// GCPAuth obtains a short-lived Bearer token via Application Default
// Credentials and injects it. Token caching and refresh are handled
// internally by *authlib.Credentials.
type GCPAuth struct {
	creds *authlib.Credentials
}

const gcpCloudPlatformScope = "https://www.googleapis.com/auth/cloud-platform"

// NewGCPAuth constructs a GCPAuth using ADC for the cloud-platform scope.
func NewGCPAuth(ctx context.Context) (*GCPAuth, error) {
	creds, err := credentials.DetectDefault(&credentials.DetectOptions{
		Scopes: []string{gcpCloudPlatformScope},
	})
	if err != nil {
		return nil, fmt.Errorf("orange: GCP Application Default Credentials not found: %w\n"+
			"Run: gcloud auth application-default login\n"+
			"Or set GOOGLE_APPLICATION_CREDENTIALS to a service account key file.", err)
	}
	return &GCPAuth{creds: creds}, nil
}

// newGCPAuthFromCreds constructs a GCPAuth from an already-resolved *authlib.Credentials.
// Used by tests to inject a fake credential source without ADC.
func newGCPAuthFromCreds(creds *authlib.Credentials) *GCPAuth {
	return &GCPAuth{creds: creds}
}

func (a *GCPAuth) InjectAuth(w *up.Writer) {
	tok, err := a.creds.Token(context.Background())
	if err != nil || !tok.IsValid() {
		return
	}
	w.SetRequestHeader("authorization", "Bearer "+tok.Value)
}

// AWSAuth signs requests with AWS Signature Version 4.
// InjectAuth is a no-op; all signing happens in InjectAuthWithBody after
// the translator has produced the final body and :path.
type AWSAuth struct {
	creds  aws.CredentialsProvider
	region string
	signer *v4.Signer
}

const awsBedrockService = "bedrock-runtime"

// NewAWSAuth constructs an AWSAuth using the default AWS credential chain.
func NewAWSAuth(ctx context.Context, region string) (*AWSAuth, error) {
	cfg, err := awsconfig.LoadDefaultConfig(ctx, awsconfig.WithRegion(region))
	if err != nil {
		return nil, fmt.Errorf("orange: AWS credentials not found for region %q: %w\n"+
			"Set AWS_ACCESS_KEY_ID + AWS_SECRET_ACCESS_KEY, or configure an IAM role.", region, err)
	}
	return &AWSAuth{creds: cfg.Credentials, region: region, signer: v4.NewSigner()}, nil
}

func (a *AWSAuth) InjectAuth(_ *up.Writer) {} // no-op; signing needs the body hash

func (a *AWSAuth) InjectAuthWithBody(w *up.Writer, req SigningRequest) error {
	creds, err := a.creds.Retrieve(context.Background())
	if err != nil {
		return fmt.Errorf("orange: AWSAuth: retrieve credentials: %w", err)
	}
	body := req.Body
	if body == nil {
		body = []byte{}
	}
	u := &url.URL{Scheme: "https", Host: req.Host, Path: req.Path}
	hr, err := http.NewRequest(req.Method, u.String(), bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("orange: AWSAuth: build request: %w", err)
	}
	if err := a.signer.SignHTTP(context.Background(), creds, hr, hexSHA256(body),
		awsBedrockService, a.region, time.Now()); err != nil {
		return fmt.Errorf("orange: AWSAuth: sign: %w", err)
	}
	w.SetRequestHeader("authorization", hr.Header.Get("Authorization"))
	w.SetRequestHeader("x-amz-date", hr.Header.Get("X-Amz-Date"))
	if tok := hr.Header.Get("X-Amz-Security-Token"); tok != "" {
		w.SetRequestHeader("x-amz-security-token", tok)
	}
	return nil
}

func hexSHA256(b []byte) string {
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}
