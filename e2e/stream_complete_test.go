package e2e

import (
	"encoding/json"
	"net/http"
	"sync"
	"testing"

	"github.com/stretchr/testify/suite"

	"github.com/dio/transit/e2e/filters"
)

// StreamCompleteSuite verifies the up.WithOnStreamComplete API end-to-end:
// the callback must fire exactly once per stream Envoy terminates, must see
// the per-stream context the request handler populated, and must do so under
// concurrent load (especially with ENVOY_CONCURRENCY > 1).
//
// Each test reads counters via the loopback HTTP server (direct, bypassing
// Envoy) so the read does not itself drive a stream completion.
type StreamCompleteSuite struct {
	suite.Suite
}

func TestStreamComplete(t *testing.T) {
	suite.Run(t, new(StreamCompleteSuite))
}

func readStreamCompleteCounters(t *testing.T) filters.StreamCompleteCounters {
	t.Helper()
	req, _ := http.NewRequest(http.MethodGet, streamCompleteLoopbackAddr+"/counters", nil)
	resp := mustDo(t, req)
	defer resp.Body.Close()
	var c filters.StreamCompleteCounters
	if err := json.NewDecoder(resp.Body).Decode(&c); err != nil {
		t.Fatalf("decode counters: %v", err)
	}
	return c
}

func (s *StreamCompleteSuite) TestSingleRequest_firesOnce() {
	before := readStreamCompleteCounters(s.T())

	req, _ := http.NewRequest(http.MethodGet, streamCompleteAddr+"/x", nil)
	resp := mustDo(s.T(), req)
	body := readBody(s.T(), resp)
	s.Equal(http.StatusOK, resp.StatusCode)
	s.Equal("ok\n", body)

	after := readStreamCompleteCounters(s.T())
	s.Equal(uint64(1), after.Fired-before.Fired, "Fired must increment by exactly 1")
	s.Equal(uint64(1), after.ContextOK-before.ContextOK, "ContextOK must increment by 1")
	s.Equal(uint64(0), after.NilContext-before.NilContext, "NilContext must stay flat")
}

func (s *StreamCompleteSuite) TestSequentialRequests_firesPerRequest() {
	const N = 10
	before := readStreamCompleteCounters(s.T())

	for i := 0; i < N; i++ {
		req, _ := http.NewRequest(http.MethodGet, streamCompleteAddr+"/", nil)
		resp := mustDo(s.T(), req)
		readBody(s.T(), resp)
		s.Equal(http.StatusOK, resp.StatusCode)
	}

	after := readStreamCompleteCounters(s.T())
	s.Equal(uint64(N), after.Fired-before.Fired)
	s.Equal(uint64(N), after.ContextOK-before.ContextOK)
}

// TestConcurrentRequests_firesPerStream is the test that matters under
// ENVOY_CONCURRENCY > 1: multiple workers process streams in parallel, and
// the OnStreamComplete callback must still fire exactly N times for N
// requests without dropping or double-counting.
func (s *StreamCompleteSuite) TestConcurrentRequests_firesPerStream() {
	const N = 64
	before := readStreamCompleteCounters(s.T())

	var wg sync.WaitGroup
	wg.Add(N)
	errs := make(chan error, N)
	for i := 0; i < N; i++ {
		go func() {
			defer wg.Done()
			req, _ := http.NewRequest(http.MethodGet, streamCompleteAddr+"/c", nil)
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				errs <- err
				return
			}
			defer resp.Body.Close()
			if resp.StatusCode != http.StatusOK {
				errs <- &httpStatusError{got: resp.StatusCode}
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		s.NoError(err)
	}

	after := readStreamCompleteCounters(s.T())
	s.Equal(uint64(N), after.Fired-before.Fired)
	s.Equal(uint64(N), after.ContextOK-before.ContextOK)
}

type httpStatusError struct{ got int }

func (e *httpStatusError) Error() string {
	return http.StatusText(e.got)
}
