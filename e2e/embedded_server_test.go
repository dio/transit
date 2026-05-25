package e2e

import (
	"net/http"
	"testing"

	"github.com/stretchr/testify/suite"
)

// EmbeddedServerSuite verifies the up.RegisterWithGroup + net.Listen embedded
// server pattern in the main e2e suite. The e2e-embedded-server filter starts a
// plain net/http server on a loopback port; Envoy routes requests to it via a
// STATIC cluster. The filter itself is a no-op — its only job is to start the
// server via the Group goroutine.
type EmbeddedServerSuite struct {
	suite.Suite
}

func TestEmbeddedServer(t *testing.T) {
	suite.Run(t, new(EmbeddedServerSuite))
}

// TestGet_serverResponds verifies the embedded server is reachable through
// Envoy and stamps x-embedded-server: ran on every response.
func (s *EmbeddedServerSuite) TestGet_serverResponds() {
	req, _ := http.NewRequest(http.MethodGet, embeddedServerAddr+"/", nil)
	resp := mustDo(s.T(), req)
	resp.Body.Close()

	s.Require().Equal(http.StatusOK, resp.StatusCode)
	s.Require().Equal("ran", resp.Header.Get("x-embedded-server"),
		"embedded server must stamp x-embedded-server on every response")
}

// TestPost_serverResponds verifies the embedded server handles non-GET verbs.
func (s *EmbeddedServerSuite) TestPost_serverResponds() {
	req, _ := http.NewRequest(http.MethodPost, embeddedServerAddr+"/", nil)
	resp := mustDo(s.T(), req)
	resp.Body.Close()

	s.Require().Equal(http.StatusOK, resp.StatusCode)
	s.Require().Equal("ran", resp.Header.Get("x-embedded-server"))
}

// TestGet_pathForwarded verifies that arbitrary sub-paths are forwarded to the
// embedded server intact.
func (s *EmbeddedServerSuite) TestGet_pathForwarded() {
	req, _ := http.NewRequest(http.MethodGet, embeddedServerAddr+"/some/path", nil)
	resp := mustDo(s.T(), req)
	resp.Body.Close()

	s.Require().Equal(http.StatusOK, resp.StatusCode)
	s.Require().Equal("ran", resp.Header.Get("x-embedded-server"))
}
