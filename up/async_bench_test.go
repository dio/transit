package up

import (
	"context"
	"fmt"
	"sync"
	"testing"

	"github.com/dio/transit/up/testutil"
	"github.com/envoyproxy/envoy/source/extensions/dynamic_modules/sdk/go/shared"
)

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
