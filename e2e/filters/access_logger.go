// e2e-logger is a minimal access logger that POSTs a JSON record to a
// configurable sink URL on each DownstreamEnd event. Used by AccessLoggerSuite
// to verify GetTimingInfo, GetBytesInfo, GetResponseCode, GetResponseFlags, and
// GetAttributeString across the access log ABI.
//
// Config shape (Envoy YAML typed_config): {"sink_url":"http://..."}
package filters

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"os"

	"github.com/dio/transit/e2e/internal/e2etest"
	"github.com/dio/transit/up"
	"github.com/envoyproxy/envoy/source/extensions/dynamic_modules/sdk/go/shared"
)

func init() {
	up.RegisterAccessLogger("e2e-logger", &e2eLoggerConfigFactory{})
}

// e2eLoggerConfigFactory parses {"sink_url":"..."} from the Envoy YAML config.
type e2eLoggerConfigFactory struct{}

func (f *e2eLoggerConfigFactory) Create(_ up.AccessLoggerConfigHandle, config []byte) (up.AccessLoggerFactory, error) {
	var cfg struct {
		SinkURL string `json:"sink_url"`
	}
	if err := json.Unmarshal(config, &cfg); err != nil {
		return nil, fmt.Errorf("e2e-logger: bad config: %w", err)
	}
	if cfg.SinkURL == "" {
		return nil, fmt.Errorf("e2e-logger: sink_url required")
	}
	return &e2eLoggerFactory{sinkURL: cfg.SinkURL}, nil
}

type e2eLoggerFactory struct{ sinkURL string }

func (f *e2eLoggerFactory) NewLogger() up.AccessLogger { return &e2eLogger{sinkURL: f.sinkURL} }
func (f *e2eLoggerFactory) OnDestroy()                 {}

type e2eLogger struct {
	up.EmptyAccessLogger
	sinkURL string
}

func (l *e2eLogger) OnLog(h up.AccessLoggerHandle, logType up.AccessLogType) {
	if logType != up.AccessLogTypeDownstreamEnd {
		return
	}

	timing := h.GetTimingInfo()
	bytes := h.GetBytesInfo()

	durationMs := int64(-1)
	if timing.RequestCompleteDurationNs >= 0 {
		durationMs = timing.RequestCompleteDurationNs / 1_000_000
	}

	codeDetails := ""
	if buf, ok := h.GetAttributeString(shared.AttributeIDResponseCodeDetails); ok {
		codeDetails = buf.ToString()
	}

	entry := map[string]any{
		"log_type":      int(logType),
		"duration_ms":   durationMs,
		"bytes_sent":    bytes.BytesSent,
		"response_code": h.GetResponseCode(),
		"code_details":  codeDetails,
		"flags":         up.ResponseFlagsString(h.GetResponseFlags()),
	}

	body, _ := json.Marshal(entry)
	resp, err := http.Post(l.sinkURL+"/log", "application/json", bytes2reader(body))
	if err != nil {
		fmt.Fprintf(os.Stderr, "e2e-logger: POST failed: %v\n", err)
		return
	}
	e2etest.CloseBody(resp)
}

func bytes2reader(b []byte) *bytes.Reader { return bytes.NewReader(b) }
