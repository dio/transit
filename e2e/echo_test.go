package e2e

import (
	"net/http"
	"testing"

	"github.com/stretchr/testify/suite"
)

// EchoSuite tests the echo filter on port 10000.
// The echo filter logs method+path and passes every request through.
type EchoSuite struct {
	suite.Suite
}

func TestEcho(t *testing.T) {
	suite.Run(t, new(EchoSuite))
}

func (s *EchoSuite) TestGet_passesThrough() {
	req, _ := http.NewRequest(http.MethodGet, echoAddr+"/", nil)
	resp := mustDo(s.T(), req)
	body := readBody(s.T(), resp)
	s.Equal(http.StatusOK, resp.StatusCode)
	s.Equal("echo ok", body)
}

func (s *EchoSuite) TestPost_passesThrough() {
	req, _ := http.NewRequest(http.MethodPost, echoAddr+"/api/v1", nil)
	resp := mustDo(s.T(), req)
	body := readBody(s.T(), resp)
	s.Equal(http.StatusOK, resp.StatusCode)
	s.Equal("echo ok", body)
}

func (s *EchoSuite) TestArbitraryPath_passesThrough() {
	req, _ := http.NewRequest(http.MethodGet, echoAddr+"/some/deep/path?q=1", nil)
	resp := mustDo(s.T(), req)
	body := readBody(s.T(), resp)
	s.Equal(http.StatusOK, resp.StatusCode)
	s.Equal("echo ok", body)
}

func (s *EchoSuite) TestExtraHeaders_passesThrough() {
	req, _ := http.NewRequest(http.MethodGet, echoAddr+"/", nil)
	req.Header.Set("x-custom-header", "value")
	resp := mustDo(s.T(), req)
	body := readBody(s.T(), resp)
	s.Equal(http.StatusOK, resp.StatusCode)
	s.Equal("echo ok", body)
}
