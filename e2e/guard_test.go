package e2e

import (
	"net/http"
	"testing"

	"github.com/stretchr/testify/suite"
)

// GuardSuite tests the guard filter on port 10001.
// The guard filter requires a non-empty x-api-key header and returns 401 if missing.
type GuardSuite struct {
	suite.Suite
}

func TestGuard(t *testing.T) {
	suite.Run(t, new(GuardSuite))
}

func (s *GuardSuite) TestMissingKey_returns401() {
	req, _ := http.NewRequest(http.MethodGet, guardAddr+"/", nil)
	resp := mustDo(s.T(), req)
	body := readBody(s.T(), resp)
	s.Equal(http.StatusUnauthorized, resp.StatusCode)
	s.Equal(`{"error":"missing x-api-key"}`, body)
}

func (s *GuardSuite) TestEmptyKey_returns401() {
	req, _ := http.NewRequest(http.MethodGet, guardAddr+"/", nil)
	req.Header.Set("x-api-key", "")
	resp := mustDo(s.T(), req)
	body := readBody(s.T(), resp)
	s.Equal(http.StatusUnauthorized, resp.StatusCode)
	s.Equal(`{"error":"missing x-api-key"}`, body)
}

func (s *GuardSuite) TestValidKey_passesThrough() {
	req, _ := http.NewRequest(http.MethodGet, guardAddr+"/", nil)
	req.Header.Set("x-api-key", "secret-key-abc")
	resp := mustDo(s.T(), req)
	body := readBody(s.T(), resp)
	s.Equal(http.StatusOK, resp.StatusCode)
	s.Equal("guard ok", body)
}

func (s *GuardSuite) TestValidKey_anyNonEmpty() {
	req, _ := http.NewRequest(http.MethodGet, guardAddr+"/", nil)
	req.Header.Set("x-api-key", "x")
	resp := mustDo(s.T(), req)
	defer resp.Body.Close()
	s.Equal(http.StatusOK, resp.StatusCode)
}

func (s *GuardSuite) TestPost_missingKey_returns401() {
	req, _ := http.NewRequest(http.MethodPost, guardAddr+"/api", nil)
	resp := mustDo(s.T(), req)
	body := readBody(s.T(), resp)
	s.Equal(http.StatusUnauthorized, resp.StatusCode)
	s.Equal(`{"error":"missing x-api-key"}`, body)
}
