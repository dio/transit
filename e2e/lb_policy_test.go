package e2e

import (
	"net/http"
	"testing"
)

// TestLBPolicy_firstHost verifies that the "first-host" custom LB policy
// routes requests to the upstream and returns 200.
func TestLBPolicy_firstHost(t *testing.T) {
	resp, err := http.Get(lbPolicyAddr + "/") //nolint:noctx
	if err != nil {
		t.Fatalf("GET /: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("want 200, got %d", resp.StatusCode)
	}
}
