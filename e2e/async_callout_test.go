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

func (s *AsyncCalloutSuite) TestGet_calloutMutatesAndForwards() {
	// The filter issues a callout; the callback sets x-callout-result and does
	// NOT send a local response. The request is forwarded to the echo upstream
	// which reflects the mutated header back as x-received-x-callout-result.
	req, err := http.NewRequest(http.MethodGet, asyncCalloutAddr+"/forward", nil)
	s.Require().NoError(err)

	resp := mustDo(s.T(), req)
	readBody(s.T(), resp)

	s.Require().Equal(http.StatusOK, resp.StatusCode)
	s.Require().Equal("forwarded", resp.Header.Get("x-received-x-callout-result"))
}

func (s *AsyncCalloutSuite) TestGet_calloutMissingHostFails() {
	// The filter omits host from the callout request. Envoy rejects it at init
	// time. The filter catches the error and sends a local 503.
	req, err := http.NewRequest(http.MethodGet, asyncCalloutAddr+"/missing-host", nil)
	s.Require().NoError(err)

	resp := mustDo(s.T(), req)
	body := readBody(s.T(), resp)

	s.Require().Equal(http.StatusServiceUnavailable, resp.StatusCode)
	s.Require().Contains(body, "missing required headers")
}

func (s *AsyncCalloutSuite) TestGet_goDoSetsHeaderAndForwards() {
	// The filter calls w.Go; inside the goroutine w.Do issues an outbound callout
	// and sets x-go-result from the callout response body. The request forwards to
	// the echo upstream which reflects it as x-received-x-go-result.
	req, err := http.NewRequest(http.MethodGet, asyncCalloutAddr+"/go-do", nil)
	s.Require().NoError(err)

	resp := mustDo(s.T(), req)
	readBody(s.T(), resp)

	s.Require().Equal(http.StatusOK, resp.StatusCode)
	s.Require().Equal("go-do", resp.Header.Get("x-received-x-go-result"))
}
