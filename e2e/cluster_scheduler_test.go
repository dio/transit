package e2e

import (
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestClusterExtension_schedulerCallback(t *testing.T) {
	deadline := time.Now().Add(5 * time.Second)
	var last string
	for time.Now().Before(deadline) {
		resp, err := http.Get(clusterSchedulerAddr + "/") //nolint:noctx
		if err != nil {
			t.Fatalf("GET scheduler state: %v", err)
		}
		last = readBody(t, resp)
		if strings.Contains(last, "committed=2") && strings.Contains(last, "ran=2") {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("scheduled callback did not run; last state: %s", last)
}
