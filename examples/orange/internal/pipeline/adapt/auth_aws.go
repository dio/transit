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

	"github.com/aws/aws-sdk-go-v2/aws"
	v4 "github.com/aws/aws-sdk-go-v2/aws/signer/v4"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"

	"github.com/dio/transit/up"
)

// awsBedrockService is the SigV4 signing service name for Bedrock Runtime.
// Note: the endpoint hostname uses "bedrock-runtime" but the credential scope
// must say "bedrock".
const awsBedrockService = "bedrock"

// AWSAuth signs requests with AWS Signature Version 4.
// InjectAuth is a no-op; all signing happens in InjectAuthWithBody after
// the translator has produced the final body and :path.
type AWSAuth struct {
	creds  aws.CredentialsProvider
	region string
	signer *v4.Signer
}

// NewAWSAuth constructs an AWSAuth using the default AWS credential chain
// (AWS_ACCESS_KEY_ID + AWS_SECRET_ACCESS_KEY env vars, ~/.aws/credentials,
// IAM instance role, etc.).
func NewAWSAuth(ctx context.Context, region string) (*AWSAuth, error) {
	cfg, err := awsconfig.LoadDefaultConfig(ctx, awsconfig.WithRegion(region))
	if err != nil {
		return nil, fmt.Errorf("orange: AWS credentials not found for region %q: %w; "+
			"set AWS_ACCESS_KEY_ID + AWS_SECRET_ACCESS_KEY or configure an IAM role", region, err)
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
