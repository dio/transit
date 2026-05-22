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
	require.Equal(t, "tls upstream ok sni=cluster-tls.local", readBody(t, resp))
}
