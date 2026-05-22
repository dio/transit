package e2e

import (
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestClusterExtension_schedulerCallback(t *testing.T) {
	var last string
	require.Eventually(t, func() bool {
		resp, err := http.Get(clusterSchedulerAddr + "/") //nolint:noctx
		if err != nil {
			last = fmt.Sprintf("GET scheduler state: %v", err)
			return false
		}
		last = readBody(t, resp)
		if resp.StatusCode != http.StatusOK {
			last = fmt.Sprintf("GET scheduler state: status %d body %s", resp.StatusCode, last)
			return false
		}

		committed, ran, ok := parseSchedulerState(last)
		if !ok {
			return false
		}
		return committed >= 2 && ran == committed
	}, 5*time.Second, 100*time.Millisecond, "scheduled callback did not run; last state: %s", last)
}

func parseSchedulerState(body string) (committed int64, ran int64, ok bool) {
	if _, err := fmt.Sscanf(body, "committed=%d ran=%d", &committed, &ran); err != nil {
		return 0, 0, false
	}
	return committed, ran, true
}
