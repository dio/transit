package e2e

import (
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestClusterExtension_staticHosts verifies that the Cluster Extension owns the
// host set, completes initialization, and routes through its ClusterLB.
func TestClusterExtension_staticHosts(t *testing.T) {
	resp, err := http.Get(clusterExtensionAddr + "/") //nolint:noctx
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	require.Contains(t, readBody(t, resp), "upstream ok")
}

// TestClusterExtension_tlsHosts verifies that the same static-hosts Cluster
// Extension works unchanged when Envoy is configured with a TLS transport socket
// pointing at a local HTTPS server. TLS validation is an Envoy cluster-level
// concern; the Go Cluster Extension code in cluster.go requires no modification.
func TestClusterExtension_tlsHosts(t *testing.T) {
	resp, err := http.Get(clusterExtensionTLSAddr + "/") //nolint:noctx
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	require.Equal(t, "tls upstream ok sni=cluster-tls.local client=", readBody(t, resp))
}

// TestClusterExtension_h2TLSWithUpstreamFilter verifies the dynamic cluster
// path works with upstream HTTP/2 filters before the upstream codec.
func TestClusterExtension_h2TLSWithUpstreamFilter(t *testing.T) {
	resp, err := http.Get(clusterExtensionH2TLSAuthAddr + "/") //nolint:noctx
	require.NoError(t, err)
	defer resp.Body.Close()

	require.Equal(t, http.StatusOK, resp.StatusCode)
	require.Equal(t, "HTTP/2.0", resp.Header.Get("x-upstream-proto"))
	require.Equal(t, "Bearer test-token", resp.Header.Get("x-received-authorization"))
}

// TestClusterExtension_mTLSHosts verifies that Envoy can add a client
// certificate on top of the same Cluster Extension host discovery path. The
// local HTTPS upstream requires and verifies that client certificate before it
// responds.
func TestClusterExtension_mTLSHosts(t *testing.T) {
	resp, err := http.Get(clusterExtensionMTLSAddr + "/") //nolint:noctx
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	require.Equal(t, "tls upstream ok sni=cluster-mtls.local client=transit-e2e-client", readBody(t, resp))
}
