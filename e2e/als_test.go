package e2e

import (
	"context"
	"net/http"
	"testing"
	"time"

	accesslogdatav3 "github.com/envoyproxy/go-control-plane/envoy/data/accesslog/v3"
	"github.com/stretchr/testify/suite"
)

// AlsSuite tests that dynamic metadata set by an HTTP filter appears in ALS
// (envoy.access_loggers.http_grpc) log entries received by alssink.
//
// The als-e2e listener runs the e2e-metadata filter, which calls
// w.SetMetadata("e2e", "custom_field", "hello-from-filter") and
// w.SetMetadata("e2e", "method", <HTTP-method>) before each response. Those
// values land in AccessLogCommon.metadata.filter_metadata["e2e"] of the ALS
// entry.
type AlsSuite struct {
	suite.Suite
}

func TestAls(t *testing.T) {
	suite.Run(t, new(AlsSuite))
}

func (s *AlsSuite) SetupTest() {
	alsSink.Reset()
}

// TestGet_customFieldInMetadata verifies that the custom_field set by the
// e2e-metadata filter appears in the ALS entry's filter metadata.
func (s *AlsSuite) TestGet_customFieldInMetadata() {
	req, _ := http.NewRequest(http.MethodGet, alsAddr+"/", nil)
	resp := mustDo(s.T(), req)
	resp.Body.Close()
	s.Require().Equal(http.StatusOK, resp.StatusCode)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	_, ok := alsSink.WaitForHTTPEntry(ctx, func(e *accesslogdatav3.HTTPAccessLogEntry) bool {
		return alsMetadataString(e, "e2e", "custom_field") == "hello-from-filter"
	})
	s.Require().True(ok, "timed out waiting for ALS entry with e2e:custom_field=hello-from-filter")
}

// TestGet_methodInMetadata verifies that the method metadata key tracks the
// HTTP verb.
func (s *AlsSuite) TestGet_methodInMetadata() {
	req, _ := http.NewRequest(http.MethodGet, alsAddr+"/", nil)
	resp := mustDo(s.T(), req)
	resp.Body.Close()
	s.Require().Equal(http.StatusOK, resp.StatusCode)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	_, ok := alsSink.WaitForHTTPEntry(ctx, func(e *accesslogdatav3.HTTPAccessLogEntry) bool {
		return alsMetadataString(e, "e2e", "method") == "GET"
	})
	s.Require().True(ok, "timed out waiting for ALS entry with e2e:method=GET")
}

// TestPost_methodReflectsVerb verifies the method metadata reflects POST.
func (s *AlsSuite) TestPost_methodReflectsVerb() {
	req, _ := http.NewRequest(http.MethodPost, alsAddr+"/", nil)
	resp := mustDo(s.T(), req)
	resp.Body.Close()
	s.Require().Equal(http.StatusOK, resp.StatusCode)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	_, ok := alsSink.WaitForHTTPEntry(ctx, func(e *accesslogdatav3.HTTPAccessLogEntry) bool {
		return alsMetadataString(e, "e2e", "method") == "POST"
	})
	s.Require().True(ok, "timed out waiting for ALS entry with e2e:method=POST")
}

// alsMetadataString returns the string value at filter_metadata[ns][key] in
// the ALS entry, or "" if absent.
func alsMetadataString(e *accesslogdatav3.HTTPAccessLogEntry, ns, key string) string {
	fm := e.GetCommonProperties().GetMetadata().GetFilterMetadata()
	if fm == nil {
		return ""
	}
	ns_struct, ok := fm[ns]
	if !ok {
		return ""
	}
	v, ok := ns_struct.GetFields()[key]
	if !ok {
		return ""
	}
	return v.GetStringValue()
}
