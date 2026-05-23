package up

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/dio/transit/up/testutil"
	"github.com/envoyproxy/envoy/source/extensions/dynamic_modules/sdk/go/shared"
	"github.com/stretchr/testify/require"
)

func TestWriterGoDo_parallelCallouts_resumeOnce(t *testing.T) {
	var nextID atomic.Uint64
	handle := testutil.NewFilterHandle(
		testutil.WithHeaders(map[string]string{":method": "GET", ":path": "/fanout"}),
		testutil.WithHTTPCalloutFunc(func(cluster string, _ [][2]string, _ []byte, _ uint64, cb shared.HttpCalloutCallback) (shared.HttpCalloutInitResult, uint64) {
			id := nextID.Add(1)
			go func() {
				time.Sleep(50 * time.Millisecond)
				cb.OnHttpCalloutDone(id, shared.HttpCalloutSuccess, nil, []shared.UnsafeEnvoyBuffer{unsafeBuffer(cluster)})
			}()
			return shared.HttpCalloutInitSuccess, id
		}),
	)
	f := &filter{
		handle: handle,
		handler: func(w *Writer, _ *Request) {
			w.Go(func(ctx context.Context) {
				var wg sync.WaitGroup
				results := make([]string, 4)
				for i := range results {
					wg.Add(1)
					go func(i int) {
						defer wg.Done()
						resp, err := w.Do(ctx, HTTPCalloutRequest{Cluster: fmt.Sprintf("backend-%d", i)})
						require.NoError(t, err)
						require.Len(t, resp.Body, 1)
						results[i] = resp.Body[0].ToString()
					}(i)
				}
				wg.Wait()
				w.SetRequestHeader("x-fanout", fmt.Sprintf("%s,%s,%s,%s", results[0], results[1], results[2], results[3]))
			})
		},
	}

	start := time.Now()
	status := f.OnRequestHeaders(handle.RequestHeaders(), true)
	require.Equal(t, shared.HeadersStatusStop, status)
	requireDone(t, handle.ContinueRequestC)

	require.Less(t, time.Since(start), 160*time.Millisecond)
	require.Equal(t, "backend-0,backend-1,backend-2,backend-3", handle.RequestHeaders().GetOne("x-fanout").ToString())
}

func TestWriterHTTPCallout_pausesAndResumes(t *testing.T) {
	handle := testutil.NewFilterHandle(
		testutil.WithHTTPCalloutFunc(func(_ string, _ [][2]string, _ []byte, _ uint64, cb shared.HttpCalloutCallback) (shared.HttpCalloutInitResult, uint64) {
			go cb.OnHttpCalloutDone(1, shared.HttpCalloutSuccess, nil, []shared.UnsafeEnvoyBuffer{unsafeBuffer("ok")})
			return shared.HttpCalloutInitSuccess, 1
		}),
	)
	f := &filter{
		handle: handle,
		handler: func(w *Writer, _ *Request) {
			init, err := w.HTTPCallout(HTTPCalloutRequest{Cluster: "auth"}, func(_ HTTPCalloutResult, _ [][2]Buffer, body []Buffer) {
				require.Len(t, body, 1)
				w.SetRequestHeader("x-auth", body[0].ToString())
			})
			require.NoError(t, err)
			require.Equal(t, HTTPCalloutInitSuccess, init)
		},
	}

	status := f.OnRequestHeaders(handle.RequestHeaders(), true)
	require.Equal(t, shared.HeadersStatusStop, status)
	requireDone(t, handle.ContinueRequestC)
	require.Equal(t, "ok", handle.RequestHeaders().GetOne("x-auth").ToString())
}

func TestWriterHTTPCallout_synchronousCallbackDoesNotStop(t *testing.T) {
	handle := testutil.NewFilterHandle(
		testutil.WithHTTPCalloutFunc(func(_ string, _ [][2]string, _ []byte, _ uint64, cb shared.HttpCalloutCallback) (shared.HttpCalloutInitResult, uint64) {
			cb.OnHttpCalloutDone(1, shared.HttpCalloutSuccess, nil, []shared.UnsafeEnvoyBuffer{unsafeBuffer("ok")})
			return shared.HttpCalloutInitSuccess, 1
		}),
	)
	f := &filter{
		handle: handle,
		handler: func(w *Writer, _ *Request) {
			init, err := w.HTTPCallout(HTTPCalloutRequest{Cluster: "auth"}, func(_ HTTPCalloutResult, _ [][2]Buffer, body []Buffer) {
				require.Len(t, body, 1)
				w.SetRequestHeader("x-auth", body[0].ToString())
			})
			require.NoError(t, err)
			require.Equal(t, HTTPCalloutInitSuccess, init)
		},
	}

	status := f.OnRequestHeaders(handle.RequestHeaders(), true)
	require.Equal(t, shared.HeadersStatusContinue, status)
	require.Equal(t, 0, handle.ContinuedReq)
	require.Equal(t, "ok", handle.RequestHeaders().GetOne("x-auth").ToString())
}

func TestWriterHTTPCallout_localResponseDoesNotContinue(t *testing.T) {
	handle := testutil.NewFilterHandle(
		testutil.WithHTTPCalloutFunc(func(_ string, _ [][2]string, _ []byte, _ uint64, cb shared.HttpCalloutCallback) (shared.HttpCalloutInitResult, uint64) {
			go cb.OnHttpCalloutDone(1, shared.HttpCalloutSuccess, nil, nil)
			return shared.HttpCalloutInitSuccess, 1
		}),
	)
	f := &filter{
		handle: handle,
		handler: func(w *Writer, _ *Request) {
			init, err := w.HTTPCallout(HTTPCalloutRequest{Cluster: "auth"}, func(_ HTTPCalloutResult, _ [][2]Buffer, _ []Buffer) {
				w.SendLocalResponse(503, []byte(`{"error":"unavailable"}`), [2]string{"content-type", "application/json"})
			})
			require.NoError(t, err)
			require.Equal(t, HTTPCalloutInitSuccess, init)
		},
	}

	status := f.OnRequestHeaders(handle.RequestHeaders(), true)
	require.Equal(t, shared.HeadersStatusStop, status)
	requireDone(t, handle.LocalResponseC)
	require.Equal(t, 0, handle.ContinuedReq)
	require.Equal(t, uint32(503), handle.LocalResponses[0].Status)
}

func TestWriterHTTPCallout_synchronousLocalResponseStopsWithoutContinue(t *testing.T) {
	handle := testutil.NewFilterHandle(
		testutil.WithHTTPCalloutFunc(func(_ string, _ [][2]string, _ []byte, _ uint64, cb shared.HttpCalloutCallback) (shared.HttpCalloutInitResult, uint64) {
			cb.OnHttpCalloutDone(1, shared.HttpCalloutSuccess, nil, nil)
			return shared.HttpCalloutInitSuccess, 1
		}),
	)
	f := &filter{
		handle: handle,
		handler: func(w *Writer, _ *Request) {
			init, err := w.HTTPCallout(HTTPCalloutRequest{Cluster: "auth"}, func(_ HTTPCalloutResult, _ [][2]Buffer, _ []Buffer) {
				w.SendLocalResponse(503, []byte(`{"error":"unavailable"}`), [2]string{"content-type", "application/json"})
			})
			require.NoError(t, err)
			require.Equal(t, HTTPCalloutInitSuccess, init)
		},
	}

	status := f.OnRequestHeaders(handle.RequestHeaders(), true)
	require.Equal(t, shared.HeadersStatusStop, status)
	requireDone(t, handle.LocalResponseC)
	require.Equal(t, 0, handle.ContinuedReq)
	require.Equal(t, uint32(503), handle.LocalResponses[0].Status)
}

func TestWriterGo_streamCompleteCancelsWithoutResume(t *testing.T) {
	done := make(chan struct{})
	handle := testutil.NewFilterHandle()
	f := &filter{
		handle: handle,
		handler: func(w *Writer, _ *Request) {
			w.Go(func(ctx context.Context) {
				<-ctx.Done()
				close(done)
			})
		},
	}

	status := f.OnRequestHeaders(handle.RequestHeaders(), true)
	require.Equal(t, shared.HeadersStatusStop, status)
	f.OnStreamComplete()

	require.Eventually(t, func() bool {
		select {
		case <-done:
			return true
		default:
			return false
		}
	}, 2*time.Second, 10*time.Millisecond)
	require.Equal(t, 0, handle.ContinuedReq)
}

func TestMetricIDPreservesSharedWidth(t *testing.T) {
	id := MetricID(1<<40 + 7)
	require.Equal(t, shared.MetricID(1<<40+7), shared.MetricID(id))
}

func unsafeBuffer(s string) shared.UnsafeEnvoyBuffer {
	b := []byte(s)
	return shared.UnsafeEnvoyBuffer{Ptr: &b[0], Len: uint64(len(b))}
}

func requireDone(t *testing.T, ch <-chan struct{}) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for async completion")
	}
}
