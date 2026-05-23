package e2e

import (
	"net/http"
	"testing"

	"github.com/stretchr/testify/suite"
)

type AsyncCalloutSuite struct {
	suite.Suite
}

func TestAsyncCallout(t *testing.T) {
	suite.Run(t, new(AsyncCalloutSuite))
}

func (s *AsyncCalloutSuite) TestGet_calloutBodyReturnedAsLocalResponse() {
	req, err := http.NewRequest(http.MethodGet, asyncCalloutAddr+"/checked", nil)
	s.Require().NoError(err)

	resp := mustDo(s.T(), req)
	body := readBody(s.T(), resp)

	s.Require().Equal(http.StatusOK, resp.StatusCode)
	s.Require().Equal("ok", resp.Header.Get("x-async-callout"))
	s.Require().Equal("checked", body)
}
