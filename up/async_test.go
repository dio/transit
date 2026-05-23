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
	"github.com/envoyproxy/envoy/source/extensions/dynamic_modules/sdk/go/shared/fake"
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

func TestWriterGoDo_copiesCalloutBuffersBeforeReturn(t *testing.T) {
	handle := testutil.NewFilterHandle(
		testutil.WithHeaders(map[string]string{":method": "GET", ":path": "/copy"}),
		testutil.WithHTTPCalloutFunc(func(_ string, _ [][2]string, _ []byte, _ uint64, cb shared.HttpCalloutCallback) (shared.HttpCalloutInitResult, uint64) {
			headerName := []byte("x-copy")
			headerValue := []byte("original-header")
			body := []byte("original-body")
			cb.OnHttpCalloutDone(1, shared.HttpCalloutSuccess, [][2]shared.UnsafeEnvoyBuffer{
				{
					{Ptr: &headerName[0], Len: uint64(len(headerName))},
					{Ptr: &headerValue[0], Len: uint64(len(headerValue))},
				},
			}, []shared.UnsafeEnvoyBuffer{{Ptr: &body[0], Len: uint64(len(body))}})
			copy(headerValue, "mutated-header!")
			copy(body, "mutated-body!")
			return shared.HttpCalloutInitSuccess, 1
		}),
	)
	f := &filter{
		handle: handle,
		handler: func(w *Writer, _ *Request) {
			w.Go(func(ctx context.Context) {
				resp, err := w.Do(ctx, HTTPCalloutRequest{Cluster: "backend"})
				require.NoError(t, err)
				require.Len(t, resp.Headers, 1)
				require.Len(t, resp.Body, 1)
				w.SetRequestHeader("x-header-copy", resp.Headers[0][1].ToString())
				w.SetRequestHeader("x-body-copy", resp.Body[0].ToString())
			})
		},
	}

	status := f.OnRequestHeaders(handle.RequestHeaders(), true)
	require.Equal(t, shared.HeadersStatusStop, status)
	requireDone(t, handle.ContinueRequestC)
	require.Equal(t, "original-header", handle.RequestHeaders().GetOne("x-header-copy").ToString())
	require.Equal(t, "original-body", handle.RequestHeaders().GetOne("x-body-copy").ToString())
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
			init, err := w.HTTPCallout(HTTPCalloutRequest{Cluster: "auth"}, func(_ HTTPCalloutResult, _ [][2]shared.UnsafeEnvoyBuffer, body []shared.UnsafeEnvoyBuffer) {
				require.Len(t, body, 1)
				w.SetRequestHeader("x-auth", body[0].ToString())
			})
			require.NoError(t, err)
			require.Equal(t, HTTPCalloutInitSuccess, init)
		},
	}

	status := f.OnRequestHeaders(handle.RequestHeaders(), true)
	switch status {
	case shared.HeadersStatusStop:
		requireDone(t, handle.ContinueRequestC)
	case shared.HeadersStatusContinue:
		require.Equal(t, 0, handle.ContinuedReq)
	default:
		t.Fatalf("unexpected status: %v", status)
	}
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
			init, err := w.HTTPCallout(HTTPCalloutRequest{Cluster: "auth"}, func(_ HTTPCalloutResult, _ [][2]shared.UnsafeEnvoyBuffer, body []shared.UnsafeEnvoyBuffer) {
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
	var cb shared.HttpCalloutCallback
	handle := testutil.NewFilterHandle(
		testutil.WithHTTPCalloutFunc(func(_ string, _ [][2]string, _ []byte, _ uint64, calloutCB shared.HttpCalloutCallback) (shared.HttpCalloutInitResult, uint64) {
			cb = calloutCB
			return shared.HttpCalloutInitSuccess, 1
		}),
	)
	f := &filter{
		handle: handle,
		handler: func(w *Writer, _ *Request) {
			init, err := w.HTTPCallout(HTTPCalloutRequest{Cluster: "auth"}, func(_ HTTPCalloutResult, _ [][2]shared.UnsafeEnvoyBuffer, _ []shared.UnsafeEnvoyBuffer) {
				w.SendLocalResponse(503, []byte(`{"error":"unavailable"}`), [2]string{"content-type", "application/json"})
			})
			require.NoError(t, err)
			require.Equal(t, HTTPCalloutInitSuccess, init)
		},
	}

	status := f.OnRequestHeaders(handle.RequestHeaders(), true)
	require.Equal(t, shared.HeadersStatusStop, status)
	require.NotNil(t, cb)
	cb.OnHttpCalloutDone(1, shared.HttpCalloutSuccess, nil, nil)
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
			init, err := w.HTTPCallout(HTTPCalloutRequest{Cluster: "auth"}, func(_ HTTPCalloutResult, _ [][2]shared.UnsafeEnvoyBuffer, _ []shared.UnsafeEnvoyBuffer) {
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

func TestWriterHTTPCallout_fromRequestBodyLocalResponseDoesNotContinue(t *testing.T) {
	var cb shared.HttpCalloutCallback
	handle := testutil.NewFilterHandle(
		testutil.WithHTTPCalloutFunc(func(_ string, _ [][2]string, _ []byte, _ uint64, calloutCB shared.HttpCalloutCallback) (shared.HttpCalloutInitResult, uint64) {
			cb = calloutCB
			return shared.HttpCalloutInitSuccess, 1
		}),
	)
	f := &filter{
		handle:  handle,
		handler: noopHandler,
		requestBodyHandler: func(w *Writer, _ *BodyChunk) {
			init, err := w.HTTPCallout(HTTPCalloutRequest{Cluster: "body-auth"}, func(_ HTTPCalloutResult, _ [][2]shared.UnsafeEnvoyBuffer, _ []shared.UnsafeEnvoyBuffer) {
				w.SendLocalResponse(403, []byte("blocked"), [2]string{"content-type", "text/plain"})
			})
			require.NoError(t, err)
			require.Equal(t, HTTPCalloutInitSuccess, init)
		},
	}

	status := f.OnRequestBody(fake.NewFakeBodyBuffer([]byte("hello")), true)
	require.Equal(t, shared.BodyStatusStopAndBuffer, status)
	require.NotNil(t, cb)

	cb.OnHttpCalloutDone(1, shared.HttpCalloutSuccess, nil, nil)
	requireDone(t, handle.LocalResponseC)
	require.Equal(t, 0, handle.ContinuedReq)
	require.Equal(t, uint32(403), handle.LocalResponses[0].Status)
	require.Equal(t, "blocked", string(handle.LocalResponses[0].Body))
}

func TestWriterHTTPCallout_fromRequestBodyReplacesBodyAndContinues(t *testing.T) {
	var cb shared.HttpCalloutCallback
	handle := testutil.NewFilterHandle(
		testutil.WithHTTPCalloutFunc(func(_ string, _ [][2]string, _ []byte, _ uint64, calloutCB shared.HttpCalloutCallback) (shared.HttpCalloutInitResult, uint64) {
			cb = calloutCB
			return shared.HttpCalloutInitSuccess, 1
		}),
	)
	f := &filter{
		handle:     handle,
		handler:    noopHandler,
		bufferBody: true,
		requestBodyHandler: func(w *Writer, _ *BodyChunk) {
			init, err := w.HTTPCallout(HTTPCalloutRequest{Cluster: "body-auth"}, func(_ HTTPCalloutResult, _ [][2]shared.UnsafeEnvoyBuffer, body []shared.UnsafeEnvoyBuffer) {
				require.Len(t, body, 1)
				w.SetRequestBody([]byte(body[0].ToString()))
			})
			require.NoError(t, err)
			require.Equal(t, HTTPCalloutInitSuccess, init)
		},
	}

	status := f.OnRequestBody(fake.NewFakeBodyBuffer([]byte("hello")), true)
	require.Equal(t, shared.BodyStatusStopAndBuffer, status)
	require.NotNil(t, cb)

	cb.OnHttpCalloutDone(1, shared.HttpCalloutSuccess, nil, []shared.UnsafeEnvoyBuffer{unsafeBuffer("ok")})
	requireDone(t, handle.ContinueRequestC)
	require.Equal(t, 1, handle.ContinuedReq)
	require.Empty(t, handle.LocalResponses)
	require.Equal(t, []byte("ok"), handle.BufferedRequestBody().(*fake.FakeBodyBuffer).Body)
	require.Equal(t, "2", handle.RequestHeaders().GetOne("content-length").ToString())
}

func TestWriterHTTPCallout_fromRequestBodySynchronousLocalResponseStops(t *testing.T) {
	handle := testutil.NewFilterHandle(
		testutil.WithHTTPCalloutFunc(func(_ string, _ [][2]string, _ []byte, _ uint64, cb shared.HttpCalloutCallback) (shared.HttpCalloutInitResult, uint64) {
			cb.OnHttpCalloutDone(1, shared.HttpCalloutSuccess, nil, nil)
			return shared.HttpCalloutInitSuccess, 1
		}),
	)
	f := &filter{
		handle:  handle,
		handler: noopHandler,
		requestBodyHandler: func(w *Writer, _ *BodyChunk) {
			init, err := w.HTTPCallout(HTTPCalloutRequest{Cluster: "body-auth"}, func(_ HTTPCalloutResult, _ [][2]shared.UnsafeEnvoyBuffer, _ []shared.UnsafeEnvoyBuffer) {
				w.SendLocalResponse(403, []byte("blocked"), [2]string{"content-type", "text/plain"})
			})
			require.NoError(t, err)
			require.Equal(t, HTTPCalloutInitSuccess, init)
		},
	}

	status := f.OnRequestBody(fake.NewFakeBodyBuffer([]byte("hello")), true)
	require.Equal(t, shared.BodyStatusStopAndBuffer, status)
	requireDone(t, handle.LocalResponseC)
	require.Equal(t, 0, handle.ContinuedReq)
	require.Equal(t, uint32(403), handle.LocalResponses[0].Status)
}

func TestWriterGo_panicsAfterHTTPCallout(t *testing.T) {
	handle := testutil.NewFilterHandle(
		testutil.WithHTTPCalloutFunc(func(_ string, _ [][2]string, _ []byte, _ uint64, _ shared.HttpCalloutCallback) (shared.HttpCalloutInitResult, uint64) {
			return shared.HttpCalloutInitSuccess, 1
		}),
	)
	f := &filter{
		handle: handle,
		handler: func(w *Writer, _ *Request) {
			init, err := w.HTTPCallout(HTTPCalloutRequest{Cluster: "auth"}, func(_ HTTPCalloutResult, _ [][2]shared.UnsafeEnvoyBuffer, _ []shared.UnsafeEnvoyBuffer) {})
			require.NoError(t, err)
			require.Equal(t, HTTPCalloutInitSuccess, init)
			require.PanicsWithValue(t, "up: Go cannot be started after HTTPCallout", func() {
				w.Go(func(context.Context) {})
			})
		},
	}

	status := f.OnRequestHeaders(handle.RequestHeaders(), true)
	require.Equal(t, shared.HeadersStatusStop, status)
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

func TestWriterHTTPCallout_panicsAfterGo(t *testing.T) {
	// goStarted is a sticky Writer-level flag set when w.Go is called.
	// HTTPCallout must panic regardless of whether the goroutine is still running.
	// Construct the Writer directly to avoid real goroutine concurrency.
	w := &Writer{f: &filter{handle: testutil.NewFilterHandle()}, goStarted: true}
	require.PanicsWithValue(t, "up: HTTPCallout cannot be started after Go or another HTTPCallout", func() {
		_, _ = w.HTTPCallout(HTTPCalloutRequest{Cluster: "c"}, func(HTTPCalloutResult, [][2]shared.UnsafeEnvoyBuffer, []shared.UnsafeEnvoyBuffer) {
		})
	})
}

func TestWriterDo_panicsOutsideGo(t *testing.T) {
	handle := testutil.NewFilterHandle()
	f := &filter{
		handle: handle,
		handler: func(w *Writer, _ *Request) {
			require.PanicsWithValue(t, "up: Do called outside Go", func() {
				_, _ = w.Do(context.Background(), HTTPCalloutRequest{Cluster: "c"})
			})
		},
	}
	f.OnRequestHeaders(handle.RequestHeaders(), true)
}

func TestWriterGo_bodyCallbackProceedsAfterResume(t *testing.T) {
	// After w.Go finishes and goStarted is cleared, OnRequestBody must run the
	// body handler and return Continue — not StopAndBuffer.
	handle := testutil.NewFilterHandle(
		testutil.WithHeaders(map[string]string{":method": "POST", ":path": "/upload"}),
	)
	proceed := make(chan struct{})
	var bodyHandlerCalled bool
	f := &filter{
		handle:     handle,
		bufferBody: true,
		handler: func(w *Writer, _ *Request) {
			// Block the goroutine until close(proceed) so the goroutine's flush(true)
			// cannot race with OnRequestHeaders' read of goStarted. This models the
			// real Envoy scheduler, which posts the resume closure back to the stream
			// worker/event loop; because OnRequestHeaders is already running on that
			// worker, the closure cannot execute until the current filter callback
			// returns. (The schedule request itself may arrive earlier; only the
			// closure execution is deferred.)
			w.Go(func(_ context.Context) {
				<-proceed
				w.SetRequestHeader("x-go-done", "1")
			})
		},
		requestBodyHandler: func(_ *Writer, _ *BodyChunk) {
			bodyHandlerCalled = true
		},
	}

	// endOfStream=false: filter stops for the goroutine, no body yet.
	status := f.OnRequestHeaders(handle.RequestHeaders(), false)
	require.Equal(t, shared.HeadersStatusStop, status)

	// Release the goroutine now that OnRequestHeaders has returned.
	close(proceed)

	// Goroutine finishes; flush(true) clears goStarted and calls ContinueRequest.
	requireDone(t, handle.ContinueRequestC)
	require.Equal(t, "1", handle.RequestHeaders().GetOne("x-go-done").ToString())

	// Deliver the (empty) body. goStarted is now false; must call handler and continue.
	bodyStatus := f.OnRequestBody(fake.NewFakeBodyBuffer(nil), true)
	require.Equal(t, shared.BodyStatusContinue, bodyStatus)
	require.True(t, bodyHandlerCalled)
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
