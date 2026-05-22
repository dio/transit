package e2e

import (
	"net/http"
	"strings"
	"testing"
)

// TestClusterExtension_staticHosts verifies that the Cluster Extension owns the
// host set, completes initialization, and routes through its ClusterLB.
func TestClusterExtension_staticHosts(t *testing.T) {
	resp, err := http.Get(clusterExtensionAddr + "/") //nolint:noctx
	if err != nil {
		t.Fatalf("GET /: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("want 200, got %d", resp.StatusCode)
	}
	body := readBody(t, resp)
	if !strings.Contains(body, "upstream ok") {
		t.Fatalf("body %q does not contain 'upstream ok'", body)
	}
}
