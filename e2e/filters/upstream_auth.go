// e2e-upstream-auth is an upstream HTTP filter that injects a synthetic
// Authorization header on every request before it reaches the upstream server.
// This demonstrates the auth-injection use case: credentials are added at the
// cluster boundary so individual filters on the listener side do not need to
// know about them.
//
// The header value "Bearer test-token" is hard-coded for test purposes; the
// upstream echo server reflects it back as "x-received-authorization" so
// UpstreamFilterSuite can assert the injection happened.
package filters

import "github.com/dio/transit/up"

func init() {
	up.Register("e2e-upstream-auth", upstreamAuthOnRequest)
}

func upstreamAuthOnRequest(w *up.Writer, _ *up.Request) {
	w.SetRequestHeader("authorization", "Bearer test-token")
}
