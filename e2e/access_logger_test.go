package e2e

import (
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/dio/transit/e2e/accessloggersink"
	"github.com/stretchr/testify/suite"
)

// AccessLoggerSuite tests the e2e-logger access logger on port 10002.
// The listener has no HTTP filter (just router) and logs every request via
// the dynamic module access logger wired to the in-process sink.
type AccessLoggerSuite struct {
	suite.Suite
}

func TestAccessLogger(t *testing.T) {
	suite.Run(t, new(AccessLoggerSuite))
}

func (s *AccessLoggerSuite) SetupTest() {
	accessloggersink.Reset()
}

func (s *AccessLoggerSuite) TearDownTest() {
	accessloggersink.Reset()
}

// TestDownstreamEnd_receivedOnGet verifies that a simple GET triggers an
// OnLog(DownstreamEnd) call with sensible field values.
func (s *AccessLoggerSuite) TestDownstreamEnd_receivedOnGet() {
	req, _ := http.NewRequest(http.MethodGet, accessLoggerAddr+"/", nil)
	resp := mustDo(s.T(), req)
	readBody(s.T(), resp)
	s.Equal(http.StatusOK, resp.StatusCode)

	entries := accessloggersink.Drain(5 * time.Second)
	s.Require().Len(entries, 1, "expected exactly one access log entry")

	e := entries[0]
	s.Equal(6, e.LogType, "expected DownstreamEnd (6)")
	s.GreaterOrEqual(e.DurationMs, int64(0), "duration must be non-negative")
	s.Greater(e.BytesSent, uint64(0), "bytes_sent must be positive for a non-empty response body")
	s.Equal(uint32(200), e.ResponseCode)
	s.NotEmpty(e.CodeDetails, "code_details should identify the route action")
}

// TestDownstreamEnd_responseCodePreserved verifies that a 404 (no matching
// route prefix altered) produces the correct response code in the log entry.
// The route_config uses prefix "/" so all paths match; send to a listener path
// that still matches but confirm code == 200.
func (s *AccessLoggerSuite) TestDownstreamEnd_multipleRequests() {
	const n = 3
	for i := 0; i < n; i++ {
		req, _ := http.NewRequest(http.MethodGet, fmt.Sprintf("%s/req%d", accessLoggerAddr, i), nil)
		resp := mustDo(s.T(), req)
		readBody(s.T(), resp)
		s.Equal(http.StatusOK, resp.StatusCode)
	}

	entries := accessloggersink.Drain(5 * time.Second)
	s.Require().Len(entries, n, "expected one entry per request")
	for _, e := range entries {
		s.Equal(6, e.LogType)
		s.Equal(uint32(200), e.ResponseCode)
	}
}

// TestDownstreamEnd_flagsField verifies that the flags field is present
// (may be empty for a clean 200) and does not contain garbage.
func (s *AccessLoggerSuite) TestDownstreamEnd_flagsField() {
	req, _ := http.NewRequest(http.MethodGet, accessLoggerAddr+"/flags-check", nil)
	resp := mustDo(s.T(), req)
	readBody(s.T(), resp)
	s.Equal(http.StatusOK, resp.StatusCode)

	entries := accessloggersink.Drain(5 * time.Second)
	s.Require().NotEmpty(entries)

	// A clean direct_response 200 should have no error flags set.
	s.Empty(entries[0].Flags, "expected no error flags for a clean 200 direct_response")
}
