package e2e

import (
	"context"
	"net/http"
	"testing"
	"time"

	otlptrace "go.opentelemetry.io/proto/otlp/trace/v1"

	"github.com/stretchr/testify/suite"
)

// OtelTracesSuite tests the e2e-tracer filter together with Envoy's built-in
// OpenTelemetry tracer (envoy.tracers.opentelemetry).
//
// The filter calls w.GetActiveSpan().SetTag(...) to annotate the active span
// with two attributes. The tracer-e2e listener is configured with 100%
// sampling so every request produces a span exported to the in-memory OTLP sink.
type OtelTracesSuite struct {
	suite.Suite
}

func TestOtelTraces(t *testing.T) {
	suite.Run(t, new(OtelTracesSuite))
}

// TestGet_spanExported verifies that a span is exported for the request and
// that the custom attribute set by the filter appears in the OTLP span.
func (s *OtelTracesSuite) TestGet_spanExported() {
	req, _ := http.NewRequest(http.MethodGet, tracerAddr+"/", nil)
	resp := mustDo(s.T(), req)
	resp.Body.Close()
	s.Require().Equal(http.StatusOK, resp.StatusCode)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	_, ok := otelSink.WaitForSpan(ctx, func(sp *otlptrace.Span) bool {
		return spanHasAttr(sp, "e2e.custom", "hello-from-tracer")
	})
	s.Require().True(ok, "timed out waiting for span with e2e.custom=hello-from-tracer")
}

// TestGet_methodAttributeSet verifies that the filter encodes the HTTP method
// as the e2e.method span attribute.
func (s *OtelTracesSuite) TestGet_methodAttributeSet() {
	req, _ := http.NewRequest(http.MethodGet, tracerAddr+"/", nil)
	resp := mustDo(s.T(), req)
	resp.Body.Close()
	s.Require().Equal(http.StatusOK, resp.StatusCode)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	_, ok := otelSink.WaitForSpan(ctx, func(sp *otlptrace.Span) bool {
		return spanHasAttr(sp, "e2e.method", "GET")
	})
	s.Require().True(ok, "timed out waiting for span with e2e.method=GET")
}

// TestPost_methodAttributeReflectsVerb verifies the method attribute tracks
// the actual verb, not just GET.
func (s *OtelTracesSuite) TestPost_methodAttributeReflectsVerb() {
	req, _ := http.NewRequest(http.MethodPost, tracerAddr+"/", nil)
	resp := mustDo(s.T(), req)
	resp.Body.Close()
	s.Require().Equal(http.StatusOK, resp.StatusCode)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	_, ok := otelSink.WaitForSpan(ctx, func(sp *otlptrace.Span) bool {
		return spanHasAttr(sp, "e2e.method", "POST")
	})
	s.Require().True(ok, "timed out waiting for span with e2e.method=POST")
}

// spanHasAttr reports whether span sp carries an attribute with the given key
// and string value.
func spanHasAttr(sp *otlptrace.Span, key, val string) bool {
	for _, a := range sp.Attributes {
		if a.Key == key && a.Value.GetStringValue() == val {
			return true
		}
	}
	return false
}
