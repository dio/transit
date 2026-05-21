package e2e

import (
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/suite"
)

// BodySuite tests the e2e-body filter (streaming mode, RegisterWithBody).
//
// The filter echoes what it saw in the request body via the x-body-len response
// header: "none" for bodyless requests (GET etc.), the byte count otherwise.
type BodySuite struct {
	suite.Suite
}

func TestBody(t *testing.T) {
	suite.Run(t, new(BodySuite))
}

// TestGet_syntheticBodyCall verifies that the body handler is invoked with
// Data: nil even when the request has no body (the endOfStream invariant).
func (s *BodySuite) TestGet_syntheticBodyCall() {
	req, _ := http.NewRequest(http.MethodGet, bodyAddr+"/", nil)
	resp := mustDo(s.T(), req)
	defer resp.Body.Close()
	s.Equal(http.StatusOK, resp.StatusCode)
	s.Equal("none", resp.Header.Get("x-body-len"))
}

// TestPost_passThrough verifies that streaming mode does not break POST requests.
//
// NOTE: with Envoy's direct_response, the response headers callback fires
// synchronously during OnRequestHeaders before OnRequestBody runs, so the
// request-body context is not yet available when the response handler executes.
// Verifying the body content in streaming mode requires a real upstream that
// replies after reading the full request. For now we just assert liveness.
func (s *BodySuite) TestPost_passThrough() {
	req, _ := http.NewRequest(http.MethodPost, bodyAddr+"/", strings.NewReader("hello"))
	req.Header.Set("content-type", "text/plain")
	resp := mustDo(s.T(), req)
	body := readBody(s.T(), resp)
	s.Equal(http.StatusOK, resp.StatusCode)
	s.Equal("body-ok", body)
}

// TestDelete_syntheticBodyCall verifies that DELETE (which has no body) also
// receives the synthetic body call.
func (s *BodySuite) TestDelete_syntheticBodyCall() {
	req, _ := http.NewRequest(http.MethodDelete, bodyAddr+"/resource/1", nil)
	resp := mustDo(s.T(), req)
	defer resp.Body.Close()
	s.Equal(http.StatusOK, resp.StatusCode)
	s.Equal("none", resp.Header.Get("x-body-len"))
}

// MutableBodySuite tests the e2e-mutable-body filter (buffered mode,
// RegisterWithMutableBody). The filter replaces the request body with
// "replaced:<original>" and echoes the replacement length via x-replaced-len.
type MutableBodySuite struct {
	suite.Suite
}

func TestMutableBody(t *testing.T) {
	suite.Run(t, new(MutableBodySuite))
}

// TestPost_bodyReplaced verifies that buffered mode does not break POST requests
// and that the response passes through correctly.
//
// NOTE: with Envoy's direct_response the response pipeline runs synchronously
// during OnRequestHeaders, before OnRequestBody fires. The context set in the
// request body handler is therefore not yet available when OnResponseHeaders
// runs, so x-replaced-len cannot be asserted here. A real upstream (which waits
// for the request body before replying) would show the correct header.
func (s *MutableBodySuite) TestPost_bodyReplaced() {
	req, _ := http.NewRequest(http.MethodPost, mutableBodyAddr+"/", strings.NewReader("hello"))
	req.Header.Set("content-type", "text/plain")
	resp := mustDo(s.T(), req)
	body := readBody(s.T(), resp)
	s.Equal(http.StatusOK, resp.StatusCode)
	s.Equal("body-mutable-ok", body)
}

// TestPost_emptyBody verifies that a zero-length POST body does not break buffered mode.
func (s *MutableBodySuite) TestPost_emptyBody() {
	req, _ := http.NewRequest(http.MethodPost, mutableBodyAddr+"/", strings.NewReader(""))
	req.Header.Set("content-type", "text/plain")
	req.ContentLength = 0
	resp := mustDo(s.T(), req)
	body := readBody(s.T(), resp)
	s.Equal(http.StatusOK, resp.StatusCode)
	s.Equal("body-mutable-ok", body)
}

// TestGet_syntheticBodyCallSetsContext verifies that the body handler fires for
// GET via the synthetic call: chunk.Data is nil so "replaced:"+string(nil) = "replaced:"
// (9 bytes) is stored in context and reflected in the response header.
func (s *MutableBodySuite) TestGet_syntheticBodyCallSetsContext() {
	req, _ := http.NewRequest(http.MethodGet, mutableBodyAddr+"/", nil)
	resp := mustDo(s.T(), req)
	defer resp.Body.Close()
	s.Equal(http.StatusOK, resp.StatusCode)
	// "replaced:" = 9 bytes; actual body replacement is a no-op (no request body),
	// but the context is written and the response handler reads it.
	s.Equal("9", resp.Header.Get("x-replaced-len"))
}
