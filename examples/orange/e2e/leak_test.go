// Tests for the post-Phase-3 cleanup contract: pending.registry must return
// to its baseline size after every stream Envoy terminates — successful,
// aborted, or never-bodied. Without OnStreamComplete wiring these would
// accumulate entries the goroutine-based design used to leak.
package e2e

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func readPendingSize(t *testing.T) int {
	t.Helper()
	resp, err := http.Get(debugURL + "/pending/size")
	require.NoError(t, err)
	defer resp.Body.Close()
	var got struct {
		Size int `json:"size"`
	}
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&got))
	return got.Size
}

// waitForPendingSize polls the debug endpoint until pending.Size() returns
// to want, giving Envoy a brief window to invoke OnStreamComplete on the
// disconnect path. Returns the last observed size.
func waitForPendingSize(t *testing.T, want int, timeout time.Duration) int {
	t.Helper()
	deadline := time.Now().Add(timeout)
	var last int
	for time.Now().Before(deadline) {
		last = readPendingSize(t)
		if last == want {
			return last
		}
		time.Sleep(50 * time.Millisecond)
	}
	return last
}

// TestRegistry_baselineAfterSuccess: a normal completed request must not
// leave anything behind in the registry.
func TestRegistry_baselineAfterSuccess(t *testing.T) {
	baseline := readPendingSize(t)

	resp := chatCompletion(t, `{"model":"gpt-4o-mini","messages":[{"role":"user","content":"Say hi."}],"max_tokens":8}`)
	resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)

	got := waitForPendingSize(t, baseline, 2*time.Second)
	require.Equalf(t, baseline, got, "registry size did not return to baseline %d (got %d)", baseline, got)
}

// TestRegistry_baselineAfterUnknownModel: classify's local-reply path
// (sendError → SendLocalResponse) is one of the teardown cases the original
// goroutine model could leak under. After Phase 2 it must clean up.
func TestRegistry_baselineAfterUnknownModel(t *testing.T) {
	baseline := readPendingSize(t)

	resp := chatCompletion(t, `{"model":"definitely-not-a-real-model","messages":[]}`)
	resp.Body.Close()
	require.Equal(t, http.StatusNotFound, resp.StatusCode)

	got := waitForPendingSize(t, baseline, 2*time.Second)
	require.Equalf(t, baseline, got, "registry size did not return to baseline %d (got %d)", baseline, got)
}

// TestRegistry_baselineAfterClientAbort: the scenario Phase 2 was designed
// for — the client drops the connection between headers and body so
// classify.bodyHandler never runs. OnStreamComplete must still fire and the
// registry must drain.
func TestRegistry_baselineAfterClientAbort(t *testing.T) {
	baseline := readPendingSize(t)

	const N = 5
	for i := 0; i < N; i++ {
		ctx, cancel := context.WithCancel(context.Background())

		// Streaming body that delivers only a fragment then waits — Envoy
		// receives headers, suspends ChooseHost on the pending, never gets
		// EndStream because we cancel before the body completes.
		pr, pw := io.Pipe()
		go func() {
			_, _ = pw.Write([]byte(`{"model":"gpt-4o-mini","messages":[`))
			time.Sleep(50 * time.Millisecond)
			_ = pw.CloseWithError(context.Canceled)
		}()

		req, err := http.NewRequestWithContext(ctx, http.MethodPost,
			proxyURL+"/v1/chat/completions", pr)
		require.NoError(t, err)
		req.Header.Set("content-type", "application/json")
		req.ContentLength = -1

		go func() {
			time.Sleep(20 * time.Millisecond)
			cancel()
		}()

		// We expect this to fail (context canceled / connection reset);
		// the assertion is on the registry, not the response.
		resp, err := http.DefaultClient.Do(req)
		if err == nil {
			resp.Body.Close()
		}
	}

	got := waitForPendingSize(t, baseline, 5*time.Second)
	require.Equalf(t, baseline, got, "registry size did not return to baseline %d (got %d) after %d aborted requests", baseline, got, N)
}

// TestRegistry_baselineUnderConcurrentLoad: stress the cleanup contract
// under N concurrent in-flight streams, mixing successful + aborted +
// unknown-model paths. Reuses Envoy's --concurrency setting from the
// surrounding harness; the assertion is that the registry drains
// regardless of teardown order.
func TestRegistry_baselineUnderConcurrentLoad(t *testing.T) {
	baseline := readPendingSize(t)

	const N = 12
	var wg sync.WaitGroup
	wg.Add(N)
	for i := 0; i < N; i++ {
		i := i
		go func() {
			defer wg.Done()
			switch i % 3 {
			case 0:
				resp := chatCompletion(t, `{"model":"gpt-4o-mini","messages":[{"role":"user","content":"hi"}],"max_tokens":4}`)
				_, _ = io.Copy(io.Discard, resp.Body)
				resp.Body.Close()
			case 1:
				resp := chatCompletion(t, `{"model":"nope","messages":[]}`)
				resp.Body.Close()
			case 2:
				ctx, cancel := context.WithTimeout(context.Background(), 25*time.Millisecond)
				defer cancel()
				req, _ := http.NewRequestWithContext(ctx, http.MethodPost,
					proxyURL+"/v1/chat/completions",
					strings.NewReader(`{"model":"gpt-4o-mini","messages":[`))
				req.Header.Set("content-type", "application/json")
				req.ContentLength = -1
				resp, err := http.DefaultClient.Do(req)
				if err == nil {
					resp.Body.Close()
				}
			}
		}()
	}
	wg.Wait()

	got := waitForPendingSize(t, baseline, 5*time.Second)
	require.Equalf(t, baseline, got,
		fmt.Sprintf("registry size did not drain after concurrent mixed load (baseline=%d got=%d)", baseline, got))
}
