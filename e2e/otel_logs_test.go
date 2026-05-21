package e2e

import (
	"context"
	"net/http"
	"testing"
	"time"

	otlplogs "go.opentelemetry.io/proto/otlp/logs/v1"

	"github.com/stretchr/testify/suite"
)

// OtelMetadataSuite tests the e2e-metadata filter together with Envoy's built-in
// OpenTelemetry access logger.
//
// The filter writes two dynamic metadata values (namespace "e2e"):
//   - "custom_field" = "hello-from-filter"  → appears in the OTLP log body
//   - "method"       = HTTP method           → appears as the "method" attribute
//
// Envoy renders the body and attribute values via %DYNAMIC_METADATA(e2e:...)%
// command operators and exports them to the in-memory OTLP sink.
type OtelMetadataSuite struct {
	suite.Suite
}

func TestOtelMetadata(t *testing.T) {
	suite.Run(t, new(OtelMetadataSuite))
}

// TestGet_customFieldInLogBody verifies that the dynamic metadata value set by
// the filter appears verbatim as the OTLP log record body.
func (s *OtelMetadataSuite) TestGet_customFieldInLogBody() {
	req, _ := http.NewRequest(http.MethodGet, metadataAddr+"/", nil)
	resp := mustDo(s.T(), req)
	resp.Body.Close()
	s.Require().Equal(http.StatusOK, resp.StatusCode)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	_, ok := otelSink.WaitForRecord(ctx, func(r *otlplogs.LogRecord) bool {
		return r.Body != nil && r.Body.GetStringValue() == "hello-from-filter"
	})
	s.Require().True(ok, "timed out waiting for OTel log record with body 'hello-from-filter'")
}

// TestGet_methodAttribute verifies that the dynamic metadata "method" value set
// by the filter appears as the "method" attribute in the OTLP log record.
func (s *OtelMetadataSuite) TestGet_methodAttribute() {
	req, _ := http.NewRequest(http.MethodGet, metadataAddr+"/", nil)
	resp := mustDo(s.T(), req)
	resp.Body.Close()
	s.Require().Equal(http.StatusOK, resp.StatusCode)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	_, ok := otelSink.WaitForRecord(ctx, func(r *otlplogs.LogRecord) bool {
		for _, attr := range r.Attributes {
			if attr.Key == "method" && attr.Value.GetStringValue() == "GET" {
				return true
			}
		}
		return false
	})
	s.Require().True(ok, "timed out waiting for OTel log record with method=GET attribute")
}

// TestPost_methodAttributeReflectsVerb verifies the method attribute tracks the
// actual HTTP verb, not just GET.
func (s *OtelMetadataSuite) TestPost_methodAttributeReflectsVerb() {
	req, _ := http.NewRequest(http.MethodPost, metadataAddr+"/", nil)
	resp := mustDo(s.T(), req)
	resp.Body.Close()
	s.Require().Equal(http.StatusOK, resp.StatusCode)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	_, ok := otelSink.WaitForRecord(ctx, func(r *otlplogs.LogRecord) bool {
		for _, attr := range r.Attributes {
			if attr.Key == "method" && attr.Value.GetStringValue() == "POST" {
				return true
			}
		}
		return false
	})
	s.Require().True(ok, "timed out waiting for OTel log record with method=POST attribute")
}
