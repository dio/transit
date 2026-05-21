package e2e

import (
	"net/http"
	"testing"

	"github.com/stretchr/testify/suite"
)

// CodecSuite tests the e2e-codec filter.
//
// The upstream always returns "hello codec" compressed with gzip, regardless
// of Accept-Encoding. The filter calls NegotiateIdentity (which the upstream
// ignores), then decodes the gzip response body with codec.Decode and replaces
// it with the plain text via SetResponseBody. The test client therefore sees
// the uncompressed string.
type CodecSuite struct {
	suite.Suite
}

func TestCodec(t *testing.T) {
	suite.Run(t, new(CodecSuite))
}

// TestGet_decodesGzipResponse verifies the full pipeline: gzip upstream →
// codec filter (NegotiateIdentity + Decode) → plain-text response body.
func (s *CodecSuite) TestGet_decodesGzipResponse() {
	req, _ := http.NewRequest(http.MethodGet, codecAddr+"/", nil)
	resp := mustDo(s.T(), req)
	body := readBody(s.T(), resp)
	s.Equal(http.StatusOK, resp.StatusCode)
	s.Equal("hello codec", body)
}

// TestPost_decodesGzipResponse verifies that POST requests also go through
// the decode path correctly.
func (s *CodecSuite) TestPost_decodesGzipResponse() {
	req, _ := http.NewRequest(http.MethodPost, codecAddr+"/", nil)
	resp := mustDo(s.T(), req)
	body := readBody(s.T(), resp)
	s.Equal(http.StatusOK, resp.StatusCode)
	s.Equal("hello codec", body)
}

// TestGet_contentEncodingStripped verifies that the filter removes the
// Content-Encoding header before forwarding the response, so clients do not
// attempt to decompress an already-decoded body.
func (s *CodecSuite) TestGet_contentEncodingStripped() {
	req, _ := http.NewRequest(http.MethodGet, codecAddr+"/", nil)
	resp := mustDo(s.T(), req)
	resp.Body.Close()
	s.Equal(http.StatusOK, resp.StatusCode)
	s.Empty(resp.Header.Get("Content-Encoding"))
}
