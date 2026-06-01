package e2e

import (
	"encoding/json"
	"net/http"
	"sync"
	"testing"

	"github.com/stretchr/testify/suite"

	"github.com/dio/transit/e2e/filters"
)

// StreamFinalizedSuite verifies the up.WithOnStreamFinalized API end-to-end:
// the callback must fire exactly once per stream Envoy finalizes (success,
// local reply, upstream failure) and the FinalizedInfo payload must carry the
// fields the access-logger path exposes.
type StreamFinalizedSuite struct {
	suite.Suite
}

func TestStreamFinalized(t *testing.T) {
	suite.Run(t, new(StreamFinalizedSuite))
}

func readStreamFinalizedCounters(t *testing.T) filters.StreamFinalizedCounters {
	t.Helper()
	req, _ := http.NewRequest(http.MethodGet, streamFinalizedLoopbackAddr+"/counters", nil)
	resp := mustDo(t, req)
	defer resp.Body.Close()
	var c filters.StreamFinalizedCounters
	if err := json.NewDecoder(resp.Body).Decode(&c); err != nil {
		t.Fatalf("decode counters: %v", err)
	}
	return c
}

func (s *StreamFinalizedSuite) TestSuccess_firesOnceWithFinalizedFields() {
	before := readStreamFinalizedCounters(s.T())

	req, _ := http.NewRequest(http.MethodGet, streamFinalizedAddr+"/x", nil)
	resp := mustDo(s.T(), req)
	readBody(s.T(), resp)
	s.Equal(http.StatusOK, resp.StatusCode)

	after := readStreamFinalizedCounters(s.T())
	s.Equal(uint64(1), after.Fired-before.Fired, "Fired must increment by exactly 1")
	s.Equal(uint64(1), after.ContextOK-before.ContextOK, "ContextOK must increment by 1")
	s.Equal(uint64(0), after.NilContext-before.NilContext, "NilContext must stay flat")
	s.Equal(uint64(1), after.NonZeroResponseCode-before.NonZeroResponseCode, "ResponseCode must be populated")
	s.Equal(uint64(1), after.NonZeroBytesReceived-before.NonZeroBytesReceived, "BytesInfo must be populated")
}

func (s *StreamFinalizedSuite) TestLocalReply_firesWithResponseCode() {
	before := readStreamFinalizedCounters(s.T())

	// guard returns 401 when x-api-key is missing.
	req, _ := http.NewRequest(http.MethodGet, streamFinalizedLocalAddr+"/", nil)
	resp := mustDo(s.T(), req)
	readBody(s.T(), resp)
	s.Equal(http.StatusUnauthorized, resp.StatusCode)

	after := readStreamFinalizedCounters(s.T())
	s.Equal(uint64(1), after.Fired-before.Fired)
	// ResponseCode must reflect the 401 sent by SendLocalResponse.
	//
	// HasLocalReplyBody is intentionally not asserted. Three paths that
	// should plausibly populate AccessLoggerHandle.GetLocalReplyBody were
	// tried against the Envoy used by this e2e (the envoy-dynamic-modules
	// build pinned by integration/down/envoy):
	//
	//   1. Filter SendLocalResponse with an inline body string (this test's
	//      401 path) — GetLocalReplyBody returns ok=false.
	//   2. Route direct_response with response_body.inline_string — same,
	//      ok=false. (An earlier draft added a TestDirectResponse case for
	//      this; removed once the result was confirmed.)
	//   3. Envoy-synthesised 503 on upstream connection failure (the UF
	//      path in TestUpstreamFailure below) — same, ok=false.
	//
	// The SDK plumbs FinalizedInfo.LocalReplyBody for parity with
	// AccessLoggerHandle.GetLocalReplyBody (which is part of Envoy's
	// dynamic-module ABI), but every code path reachable from this e2e
	// observes the field as empty. The predecessor accesslogger.go in
	// examples/request-ui called GetLocalReplyBody the same way and saw
	// the same empty result; the gap predates this change and is an
	// upstream Envoy behaviour, not an SDK regression. If a future Envoy
	// build starts populating GetLocalReplyBody on any of these paths,
	// add a HasLocalReplyBody assertion here (or restore a direct_response
	// test) — the counter and FinalizedInfo field are already wired.
	//
	// Full investigation notes and Envoy-source TODOs:
	// docs/get-local-reply-body-investigation.md
	s.Equal(uint64(1), after.NonZeroResponseCode-before.NonZeroResponseCode, "ResponseCode must be populated for local reply")
}

func (s *StreamFinalizedSuite) TestNoRequestIDLocalReply_usesFallbackCorrelation() {
	before := readStreamFinalizedCounters(s.T())

	// This listener disables generate_request_id. The SDK must stamp its
	// fallback correlation header immediately so the access logger can find
	// the finalized entry even though guard sends a local reply before queued
	// request-header mutations would normally flush.
	req, _ := http.NewRequest(http.MethodGet, streamFinalizedFallbackAddr+"/", nil)
	resp := mustDo(s.T(), req)
	readBody(s.T(), resp)
	s.Equal(http.StatusUnauthorized, resp.StatusCode)

	after := readStreamFinalizedCounters(s.T())
	s.Equal(uint64(1), after.Fired-before.Fired)
	s.Equal(uint64(1), after.ContextOK-before.ContextOK)
	s.Equal(uint64(1), after.NonZeroResponseCode-before.NonZeroResponseCode)
}

func (s *StreamFinalizedSuite) TestUpstreamFailure_firesWithResponseFlags() {
	before := readStreamFinalizedCounters(s.T())

	req, _ := http.NewRequest(http.MethodGet, streamFinalizedDeadAddr+"/", nil)
	resp := mustDo(s.T(), req)
	readBody(s.T(), resp)
	// 503 from Envoy when upstream connection fails.
	s.Equal(http.StatusServiceUnavailable, resp.StatusCode)

	after := readStreamFinalizedCounters(s.T())
	s.Equal(uint64(1), after.Fired-before.Fired)
	s.Equal(uint64(1), after.HasResponseFlags-before.HasResponseFlags, "ResponseFlags must be set on UF path")
	// See TestLocalReply_firesWithResponseCode for the full explanation of
	// why HasLocalReplyBody is not asserted on the UF path either.
}

// TestConcurrentRequests_firesPerStream verifies behaviour under multi-worker
// load: with ENVOY_CONCURRENCY > 1 the SDK-internal access logger must route
// each stream's FinalizedInfo to the right (fn, ctx) pair without dropping or
// crossing entries.
func (s *StreamFinalizedSuite) TestConcurrentRequests_firesPerStream() {
	const N = 64
	before := readStreamFinalizedCounters(s.T())

	var wg sync.WaitGroup
	wg.Add(N)
	errs := make(chan error, N)
	for i := 0; i < N; i++ {
		go func() {
			defer wg.Done()
			req, _ := http.NewRequest(http.MethodGet, streamFinalizedAddr+"/c", nil)
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

	after := readStreamFinalizedCounters(s.T())
	s.Equal(uint64(N), after.Fired-before.Fired)
	s.Equal(uint64(N), after.ContextOK-before.ContextOK)
}
