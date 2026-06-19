package e2e

import (
	"net/http"
	"testing"

	"github.com/stretchr/testify/suite"
)

// UpstreamFilterSuite verifies that a dynamic module filter can be loaded as
// an upstream HTTP filter (via HttpProtocolOptions.upstream_http_filters on a
// cluster) rather than as a downstream filter on a listener.
//
// The upstream-filter-e2e listener has no HTTP filter of its own — it only has
// the router. The e2e-upstream filter sits in the upstream-filter-upstream
// cluster's upstream_http_filters chain. On every response it stamps
// "x-upstream-filter: ran"; the test asserts that header reaches the client.
type UpstreamFilterSuite struct {
	suite.Suite
}

func TestUpstreamFilter(t *testing.T) {
	suite.Run(t, new(UpstreamFilterSuite))
}

// TestGet_upstreamFilterHeaderPresent verifies the upstream filter runs and
// stamps the response header.
func (s *UpstreamFilterSuite) TestGet_upstreamFilterHeaderPresent() {
	req, _ := http.NewRequest(http.MethodGet, upstreamFilterAddr+"/", nil)
	resp := mustDo(s.T(), req)
	resp.Body.Close()

	s.Require().Equal(http.StatusOK, resp.StatusCode)
	s.Require().Equal("ran", resp.Header.Get("x-upstream-filter"),
		"x-upstream-filter header should be set by the upstream filter")
}

// TestPost_upstreamFilterHeaderPresent verifies the header is set for non-GET
// verbs too (i.e. the filter is not request-method gated).
func (s *UpstreamFilterSuite) TestPost_upstreamFilterHeaderPresent() {
	req, _ := http.NewRequest(http.MethodPost, upstreamFilterAddr+"/", nil)
	resp := mustDo(s.T(), req)
	resp.Body.Close()

	s.Require().Equal(http.StatusOK, resp.StatusCode)
	s.Require().Equal("ran", resp.Header.Get("x-upstream-filter"),
		"x-upstream-filter header should be set by the upstream filter")
}

// TestGet_downstreamFilterNotInvolved verifies the response has no header that
// would only appear if a downstream filter ran (sanity check that the stamp
// comes from the cluster side).
func (s *UpstreamFilterSuite) TestGet_downstreamFilterNotInvolved() {
	req, _ := http.NewRequest(http.MethodGet, upstreamFilterAddr+"/", nil)
	resp := mustDo(s.T(), req)
	resp.Body.Close()

	s.Require().Equal(http.StatusOK, resp.StatusCode)
	// the upstream filter stamps x-upstream-filter; no downstream filter runs
	// on this listener so no other dynamic-module header should appear
	s.Require().Empty(resp.Header.Get("x-body-len"),
		"x-body-len would only appear if the body filter ran downstream")
}

// TestGet_responseHeaderRemovedByUpstreamFilter verifies that RemoveResponseHeader
// works from the upstream filter position: the plain upstream always sets
// "x-upstream-source: plain", but the upstream filter removes it before the
// response reaches the client.
func (s *UpstreamFilterSuite) TestGet_responseHeaderRemovedByUpstreamFilter() {
	req, _ := http.NewRequest(http.MethodGet, upstreamFilterAddr+"/", nil)
	resp := mustDo(s.T(), req)
	resp.Body.Close()

	s.Require().Equal(http.StatusOK, resp.StatusCode)
	s.Require().Empty(resp.Header.Get("x-upstream-source"),
		"x-upstream-source should be removed by the upstream filter before reaching the client")
}

// ── auth-injection use case ──────────────────────────────────────────────────

// TestGet_authHeaderInjected verifies that e2e-upstream-auth (an upstream
// filter on upstream-auth-upstream) injects "Authorization: Bearer test-token"
// into the request before it reaches the upstream server. The upstream server
// echoes it back as "x-received-authorization".
func (s *UpstreamFilterSuite) TestGet_authHeaderInjected() {
	req, _ := http.NewRequest(http.MethodGet, upstreamAuthAddr+"/", nil)
	resp := mustDo(s.T(), req)
	resp.Body.Close()

	s.Require().Equal(http.StatusOK, resp.StatusCode)
	s.Require().Equal("Bearer test-token", resp.Header.Get("x-received-authorization"),
		"upstream echo should reflect the injected Authorization header")
}

// TestGet_authHeaderInjectedH2C verifies the same upstream request header
// mutation works when Envoy speaks HTTP/2 to the upstream.
func (s *UpstreamFilterSuite) TestGet_authHeaderInjectedH2C() {
	req, _ := http.NewRequest(http.MethodGet, upstreamAuthH2CAddr+"/", nil)
	resp := mustDo(s.T(), req)
	resp.Body.Close()

	s.Require().Equal(http.StatusOK, resp.StatusCode)
	s.Require().Equal("HTTP/2.0", resp.Header.Get("x-upstream-proto"),
		"upstream should receive the request over HTTP/2")
	s.Require().Equal("Bearer test-token", resp.Header.Get("x-received-authorization"),
		"upstream echo should reflect the injected Authorization header")
}

// TestGet_authHeaderInjectedH2TLS verifies the upstream request header mutation
// works when Envoy speaks HTTP/2 over TLS to the upstream.
func (s *UpstreamFilterSuite) TestGet_authHeaderInjectedH2TLS() {
	req, _ := http.NewRequest(http.MethodGet, upstreamAuthH2TLSAddr+"/", nil)
	resp := mustDo(s.T(), req)
	resp.Body.Close()

	s.Require().Equal(http.StatusOK, resp.StatusCode)
	s.Require().Equal("https", resp.Header.Get("x-upstream-scheme"),
		"upstream should receive an HTTPS scheme when Envoy uses TLS upstream transport")
	s.Require().Equal("HTTP/2.0", resp.Header.Get("x-upstream-proto"),
		"upstream should receive the request over HTTP/2")
	s.Require().Equal("Bearer test-token", resp.Header.Get("x-received-authorization"),
		"upstream echo should reflect the injected Authorization header")
}

// TestGet_clientAuthNotForwarded verifies that without the auth-injecting
// upstream filter the upstream server does NOT see an Authorization header —
// confirming the header comes from the filter, not from the client.
func (s *UpstreamFilterSuite) TestGet_clientAuthNotForwarded() {
	req, _ := http.NewRequest(http.MethodGet, upstreamFilterAddr+"/", nil)
	resp := mustDo(s.T(), req)
	resp.Body.Close()

	s.Require().Equal(http.StatusOK, resp.StatusCode)
	s.Require().Empty(resp.Header.Get("x-received-authorization"),
		"upstream-filter cluster has no auth filter so no Authorization should be injected")
}

// ── group-based auth injection ───────────────────────────────────────────────

// TestGet_groupAuthHeaderInjected verifies that e2e-upstream-auth-group, which
// uses up.Register + up.WithGroup, injects a header set by a background goroutine.
// It confirms that group-owned state is visible to the request handler without
// any package-level variables.
func (s *UpstreamFilterSuite) TestGet_groupAuthHeaderInjected() {
	req, _ := http.NewRequest(http.MethodGet, upstreamAuthGroupAddr+"/", nil)
	resp := mustDo(s.T(), req)
	resp.Body.Close()

	s.Require().Equal(http.StatusOK, resp.StatusCode)
	s.Require().Equal("Bearer group-token", resp.Header.Get("x-received-authorization"),
		"upstream echo should reflect the header injected by the group-based filter")
}

// TestPost_groupAuthHeaderInjected verifies the group-based injection works for
// non-GET requests too.
func (s *UpstreamFilterSuite) TestPost_groupAuthHeaderInjected() {
	req, _ := http.NewRequest(http.MethodPost, upstreamAuthGroupAddr+"/", nil)
	resp := mustDo(s.T(), req)
	resp.Body.Close()

	s.Require().Equal(http.StatusOK, resp.StatusCode)
	s.Require().Equal("Bearer group-token", resp.Header.Get("x-received-authorization"),
		"upstream echo should reflect the header injected by the group-based filter")
}
