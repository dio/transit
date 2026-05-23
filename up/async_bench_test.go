package up

import (
	"context"
	"fmt"
	"sync"
	"testing"

	"github.com/dio/transit/up/testutil"
	"github.com/envoyproxy/envoy/source/extensions/dynamic_modules/sdk/go/shared"
)

// BenchmarkFilter measures per-stream allocations for the three request-handling
// modes. BenchmarkFilter_handleBaseline measures testutil handle construction
// alone so that the delta to each mode shows the filter+operation overhead.
//
// Goal: confirm whether the single filter struct allocation is visible relative
// to the operation cost, to decide whether filter pooling is worth the
// complexity.

func BenchmarkFilter_handleBaseline(b *testing.B) {
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		_ = testutil.NewFilterHandle()
	}
}

// BenchmarkFilter_filterAlloc measures just the filter struct allocation so the
// delta (sync - filterAlloc) shows the OnRequestHeaders operation overhead.
func BenchmarkFilter_filterAlloc(b *testing.B) {
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		handle := testutil.NewFilterHandle()
		f := &filter{handle: handle}
		_ = f
	}
}

func BenchmarkFilter_sync(b *testing.B) {
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		handle := testutil.NewFilterHandle()
		f := &filter{
			handle: handle,
			handler: func(w *Writer, _ *Request) {
				w.SetRequestHeader("x-bench", "1")
			},
		}
		_ = f.OnRequestHeaders(handle.RequestHeaders(), true)
	}
}

func BenchmarkFilter_httpCallout(b *testing.B) {
	// Uses the early/synchronous callback path: the callout func fires
	// OnHttpCalloutDone before HttpCallout returns, so OnRequestHeaders never
	// pauses — it detects Done and flushes inline, returning Continue.
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		handle := testutil.NewFilterHandle(
			testutil.WithHTTPCalloutFunc(func(_ string, _ [][2]string, _ []byte, _ uint64, cb shared.HttpCalloutCallback) (shared.HttpCalloutInitResult, uint64) {
				cb.OnHttpCalloutDone(1, shared.HttpCalloutSuccess, nil, nil)
				return shared.HttpCalloutInitSuccess, 1
			}),
		)
		f := &filter{
			handle: handle,
			handler: func(w *Writer, _ *Request) {
				_, _ = w.HTTPCallout(HTTPCalloutRequest{Cluster: "auth"}, func(_ HTTPCalloutResult, _ [][2]shared.UnsafeEnvoyBuffer, _ []shared.UnsafeEnvoyBuffer) {
					w.SetRequestHeader("x-auth", "ok")
				})
			},
		}
		_ = f.OnRequestHeaders(handle.RequestHeaders(), true)
	}
}

func BenchmarkFilter_goDo(b *testing.B) {
	// Single w.Do callout inside w.Go. The fakeScheduler is synchronous so the
	// goroutine runs to completion before the benchmark advances.
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		handle := testutil.NewFilterHandle(
			testutil.WithHTTPCalloutFunc(func(_ string, _ [][2]string, _ []byte, _ uint64, cb shared.HttpCalloutCallback) (shared.HttpCalloutInitResult, uint64) {
				cb.OnHttpCalloutDone(1, shared.HttpCalloutSuccess, nil, nil)
				return shared.HttpCalloutInitSuccess, 1
			}),
		)
		done := make(chan struct{})
		f := &filter{
			handle: handle,
			handler: func(w *Writer, _ *Request) {
				w.Go(func(ctx context.Context) {
					_, _ = w.Do(ctx, HTTPCalloutRequest{Cluster: "backend"})
					w.SetRequestHeader("x-go", "1")
					close(done)
				})
			},
		}
		_ = f.OnRequestHeaders(handle.RequestHeaders(), true)
		<-done
	}
}

func BenchmarkWriterDoFanout(b *testing.B) {
	for _, width := range []int{1, 2, 4, 8, 16} {
		b.Run(fmt.Sprintf("parallel-%d", width), func(b *testing.B) {
			benchmarkDoFanout(b, width, true)
		})
		b.Run(fmt.Sprintf("sequential-%d", width), func(b *testing.B) {
			benchmarkDoFanout(b, width, false)
		})
	}
}

func benchmarkDoFanout(b *testing.B, width int, parallel bool) {
	b.Helper()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		handle := testutil.NewFilterHandle(
			testutil.WithHTTPCalloutFunc(func(cluster string, _ [][2]string, _ []byte, _ uint64, cb shared.HttpCalloutCallback) (shared.HttpCalloutInitResult, uint64) {
				cb.OnHttpCalloutDone(1, shared.HttpCalloutSuccess, nil, []shared.UnsafeEnvoyBuffer{unsafeBuffer(cluster)})
				return shared.HttpCalloutInitSuccess, 1
			}),
		)
		done := make(chan struct{})
		f := &filter{
			handle: handle,
			handler: func(w *Writer, _ *Request) {
				w.Go(func(ctx context.Context) {
					if parallel {
						var wg sync.WaitGroup
						for j := 0; j < width; j++ {
							wg.Add(1)
							go func(j int) {
								defer wg.Done()
								_, _ = w.Do(ctx, HTTPCalloutRequest{Cluster: fmt.Sprintf("backend-%d", j)})
							}(j)
						}
						wg.Wait()
					} else {
						for j := 0; j < width; j++ {
							_, _ = w.Do(ctx, HTTPCalloutRequest{Cluster: fmt.Sprintf("backend-%d", j)})
						}
					}
					close(done)
				})
			},
		}
		_ = f.OnRequestHeaders(handle.RequestHeaders(), true)
		<-done
	}
}
