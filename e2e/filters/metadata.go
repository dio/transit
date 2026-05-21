// e2e-metadata sets two dynamic metadata values on every request so the OTel
// access log body template can render them via %DYNAMIC_METADATA(...)%. Used
// by OtelMetadataSuite to verify that SetMetadata values flow through Envoy
// into exported OTLP log records.
//
// Namespace "e2e", keys:
//   - "custom_field" = "hello-from-filter"  (appears in the log body)
//   - "method"       = the HTTP request method (appears as an attribute)
package filters

import "github.com/dio/transit/up"

func init() {
	up.Register("e2e-metadata", metadataOnRequest)
}

func metadataOnRequest(w *up.Writer, r *up.Request) {
	w.SetMetadata("e2e", "custom_field", "hello-from-filter")
	w.SetMetadata("e2e", "method", r.Method)
}
