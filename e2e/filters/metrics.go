// e2e-metrics defines and increments a custom counter on every request so
// OtelMetricsSuite can verify that dynamicmodulescustom.* metrics defined by a
// dynamic module filter are exported via the OTel stats sink.
package filters

import "github.com/dio/transit/up"

var e2eRequestsID up.MetricID

func init() {
	up.RegisterWithConfig(
		"e2e-metrics",
		func(h up.ConfigHandle) error {
			var err error
			e2eRequestsID, err = h.DefineCounter("e2e.requests_total")
			return err
		},
		func(w *up.Writer, _ *up.Request) {
			w.IncrementCounter(e2eRequestsID, 1)
		},
		nil,
	)
}
