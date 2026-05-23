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

// TestLBPolicy_firstHostAlwaysSelected proves that the "first-host" policy
// consistently routes to index=0 rather than falling back to round-robin.
// The two-host cluster has host-0 (returns "lb-host-0") at index=0 and
// host-1 (returns "lb-host-1") at index=1. Every response must be "lb-host-0".
func TestLBPolicy_firstHostAlwaysSelected(t *testing.T) {
	for i := 0; i < 5; i++ {
		resp, err := http.Get(lbPolicySelectionAddr + "/") //nolint:noctx
		if err != nil {
			t.Fatalf("request %d: %v", i, err)
		}
		body := readBody(t, resp)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("request %d: want 200, got %d", i, resp.StatusCode)
		}
		if body != "lb-host-0" {
			t.Fatalf("request %d: want body %q, got %q (wrong host selected)", i, "lb-host-0", body)
		}
	}
}
