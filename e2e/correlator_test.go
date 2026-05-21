package e2e

import (
	"net/http"
	"testing"
	"time"

	"github.com/dio/transit/e2e/accessloggersink"
	"github.com/stretchr/testify/suite"
)

// CorrelatorSuite tests the HTTP filter ↔ access logger correlation pattern on
// port correlatorAddr. The e2e-correlator filter deposits the response status
// code (seen during OnResponseHeaders) into a sync.Map keyed by x-request-id.
// The e2e-correlator-logger access logger pops it on DownstreamEnd, enriches it
// with finalized fields, and posts a CorrelatedEntry to the sink.
type CorrelatorSuite struct {
	suite.Suite
}

func TestCorrelator(t *testing.T) {
	suite.Run(t, new(CorrelatorSuite))
}

func (s *CorrelatorSuite) SetupTest() {
	accessloggersink.ResetCorrelated()
}

func (s *CorrelatorSuite) TearDownTest() {
	accessloggersink.ResetCorrelated()
}

// TestStatusCode_matchesBothPhases is the core assertion: the status code seen
// by the HTTP filter (status_filter) must equal the code confirmed by the access
// logger (response_code). This validates end-to-end correlation via x-request-id.
func (s *CorrelatorSuite) TestStatusCode_matchesBothPhases() {
	req, _ := http.NewRequest(http.MethodGet, correlatorAddr+"/", nil)
	resp := mustDo(s.T(), req)
	readBody(s.T(), resp)
	s.Equal(http.StatusOK, resp.StatusCode)

	entries := accessloggersink.DrainCorrelated(5 * time.Second)
	s.Require().Len(entries, 1)

	e := entries[0]
	s.NotEmpty(e.RequestID, "x-request-id must be non-empty")
	s.Equal(200, e.StatusFilter, "HTTP filter saw 200 on response headers")
	s.Equal(uint32(200), e.ResponseCode, "access logger confirms 200")
	s.GreaterOrEqual(e.DurationMs, int64(0))
	s.Greater(e.BytesSent, uint64(0))
}

// TestCorrelation_uniquePerRequest verifies that concurrent requests each get
// their own correlation record and no cross-contamination occurs.
func (s *CorrelatorSuite) TestCorrelation_uniquePerRequest() {
	const n = 3
	for i := 0; i < n; i++ {
		req, _ := http.NewRequest(http.MethodGet, correlatorAddr+"/", nil)
		resp := mustDo(s.T(), req)
		readBody(s.T(), resp)
		s.Equal(http.StatusOK, resp.StatusCode)
	}

	entries := accessloggersink.DrainCorrelated(5 * time.Second)
	s.Require().Len(entries, n)

	seen := map[string]bool{}
	for _, e := range entries {
		s.Equal(200, e.StatusFilter)
		s.Equal(uint32(200), e.ResponseCode)
		s.False(seen[e.RequestID], "request IDs must be unique: %s", e.RequestID)
		seen[e.RequestID] = true
	}
}
