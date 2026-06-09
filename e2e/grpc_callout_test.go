package e2e

import (
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/suite"
)

type GRPCCalloutSuite struct {
	suite.Suite
}

func TestGRPCCallout(t *testing.T) {
	suite.Run(t, new(GRPCCalloutSuite))
}

func (s *GRPCCalloutSuite) TestPost_echoesBodyViaGRPC() {
	// The filter reads the request body, encodes it as EchoRequest, issues a
	// GRPCCallout to the e2e gRPC upstream, decodes the EchoResponse, and
	// returns the echoed payload as the local response.
	req, err := http.NewRequest(http.MethodPost, grpcCalloutAddr+"/echo", strings.NewReader("hello grpc"))
	s.Require().NoError(err)
	req.Header.Set("content-type", "text/plain")

	resp := mustDo(s.T(), req)
	body := readBody(s.T(), resp)

	s.Require().Equal(http.StatusOK, resp.StatusCode, "body: %s", body)
	s.Require().Equal("0", resp.Header.Get("x-grpc-status"))
	s.Require().Equal("11", resp.Header.Get("x-grpc-sequence"))
	s.Require().Equal("hello grpc", body)
}

func (s *GRPCCalloutSuite) TestPost_emptyBody() {
	req, err := http.NewRequest(http.MethodPost, grpcCalloutAddr+"/echo", strings.NewReader(""))
	s.Require().NoError(err)
	req.Header.Set("content-type", "text/plain")

	resp := mustDo(s.T(), req)
	body := readBody(s.T(), resp)

	s.Require().Equal(http.StatusOK, resp.StatusCode)
	s.Require().Equal("0", resp.Header.Get("x-grpc-status"))
	s.Require().Equal("1", resp.Header.Get("x-grpc-sequence"))
	s.Require().Equal("", body)
}

func (s *GRPCCalloutSuite) TestPost_largeBody() {
	payload := strings.Repeat("x", 1024)
	req, err := http.NewRequest(http.MethodPost, grpcCalloutAddr+"/echo", strings.NewReader(payload))
	s.Require().NoError(err)
	req.Header.Set("content-type", "text/plain")

	resp := mustDo(s.T(), req)
	body := readBody(s.T(), resp)

	s.Require().Equal(http.StatusOK, resp.StatusCode)
	s.Require().Equal("1025", resp.Header.Get("x-grpc-sequence"))
	s.Require().Equal(payload, body)
}
