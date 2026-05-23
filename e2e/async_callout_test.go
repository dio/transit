package e2e

import (
	"net/http"
	"strings"
	"testing"
	"time"

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

func (s *AsyncCalloutSuite) TestPost_goDoBodyCallbackProceedsAfterResume() {
	// Verifies that after Go+Do completes (goStarted cleared), the request body
	// handler runs normally and its header mutation reaches upstream.
	//
	// Filter: headers handler calls w.Go → w.Do → sets x-go-result from callout body.
	//         body handler (after resume) sets x-body-len from body length.
	// Both headers are echoed by the forward-echo upstream.
	req, err := http.NewRequest(http.MethodPost, asyncCalloutBodyAddr+"/go-do-body", strings.NewReader("hello"))
	s.Require().NoError(err)
	req.Header.Set("content-type", "text/plain")

	resp := mustDo(s.T(), req)
	readBody(s.T(), resp)

	s.Require().Equal(http.StatusOK, resp.StatusCode)
	s.Require().Equal("go-do-body", resp.Header.Get("x-received-x-go-result"))
	s.Require().Equal("5", resp.Header.Get("x-received-x-body-len"))
}

func (s *AsyncCalloutSuite) TestGet_calloutLocalResponseUpstreamNotReached() {
	// The /checked path calls SendLocalResponse inside the callout callback.
	// The listener for this test routes to the recorder upstream instead of the
	// echo upstream. After the client receives the local response, the recorder
	// must have zero requests — proving SendLocalResponse stops forwarding.
	s.T().Cleanup(mutableBodyRecorder.Reset)

	req, err := http.NewRequest(http.MethodGet, asyncCalloutLocalResponseAddr+"/checked", nil)
	s.Require().NoError(err)

	resp := mustDo(s.T(), req)
	body := readBody(s.T(), resp)

	s.Require().Equal(http.StatusOK, resp.StatusCode)
	s.Require().Equal("ok", resp.Header.Get("x-async-callout"))
	s.Require().Equal("checked", body)

	// Wait 200 ms after the local response to catch any delayed upstream forward.
	// An immediate Len()==0 check would race against asynchronous resume/forward.
	mutableBodyRecorder.WaitForNone(s.T(), 200*time.Millisecond)
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

func (s *AsyncCalloutSuite) TestGet_goDoFanoutBothCalloutsSeen() {
	// The filter issues two w.Do calls concurrently inside a single w.Go goroutine
	// (fan-out). Each callout targets a different path on async-callout-upstream,
	// which returns the path segment as the body. The goroutine merges both results
	// into x-fanout-result; the forward-echo upstream reflects the header back as
	// x-received-x-fanout-result. This proves both callouts completed and their
	// bodies were observable under real Envoy scheduling.
	req, err := http.NewRequest(http.MethodGet, asyncCalloutAddr+"/fanout", nil)
	s.Require().NoError(err)

	resp := mustDo(s.T(), req)
	readBody(s.T(), resp)

	s.Require().Equal(http.StatusOK, resp.StatusCode)
	s.Require().Equal("fanout-a,fanout-b", resp.Header.Get("x-received-x-fanout-result"))
}
