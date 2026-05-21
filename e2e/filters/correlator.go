package filters

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"sync"

	"github.com/dio/transit/up"
)

func init() {
	up.RegisterWithResponse("e2e-correlator", correlatorOnRequest, correlatorOnResponse)
	up.RegisterAccessLogger("e2e-correlator-logger", &correlatorConfigFactory{})
}

// correlatorPending maps x-request-id to the partial record deposited by the
// HTTP filter. The access logger pops and enriches it on DownstreamEnd.
var correlatorPending correlatorMap

type correlatorMap struct{ m sync.Map }

func (p *correlatorMap) store(requestID string, status int) {
	if requestID != "" {
		p.m.Store(requestID, status)
	}
}

func (p *correlatorMap) loadAndDelete(requestID string) (int, bool) {
	v, ok := p.m.LoadAndDelete(requestID)
	if !ok {
		return 0, false
	}
	return v.(int), true
}

// HTTP filter: request side is a no-op; correlation data comes from the response.
func correlatorOnRequest(_ *up.Writer, _ *up.Request) {}

// HTTP filter: response headers — deposit status code keyed by request ID.
func correlatorOnResponse(w *up.Writer, chunk *up.ResponseChunk) {
	if chunk.StatusCode == 0 {
		return
	}
	rid, ok := w.GetAttributeString(up.AttributeIDRequestId)
	if !ok || rid.Len == 0 {
		return
	}
	correlatorPending.store(rid.ToString(), chunk.StatusCode)
}

// Access logger factory — parses {"sink_url":"..."} from Envoy config.
type correlatorConfigFactory struct{}

func (f *correlatorConfigFactory) Create(_ up.AccessLoggerConfigHandle, config []byte) (up.AccessLoggerFactory, error) {
	var cfg struct {
		SinkURL string `json:"sink_url"`
	}
	if err := json.Unmarshal(config, &cfg); err != nil {
		return nil, fmt.Errorf("e2e-correlator-logger: bad config: %w", err)
	}
	if cfg.SinkURL == "" {
		return nil, fmt.Errorf("e2e-correlator-logger: sink_url required")
	}
	return &correlatorLoggerFactory{sinkURL: cfg.SinkURL}, nil
}

type correlatorLoggerFactory struct{ sinkURL string }

func (f *correlatorLoggerFactory) NewLogger() up.AccessLogger {
	return &correlatorLogger{sinkURL: f.sinkURL}
}
func (f *correlatorLoggerFactory) OnDestroy() {}

type correlatorLogger struct {
	up.EmptyAccessLogger
	sinkURL string
}

func (l *correlatorLogger) OnLog(h up.AccessLoggerHandle, logType up.AccessLogType) {
	if logType != up.AccessLogTypeDownstreamEnd {
		return
	}

	ridBuf, ok := h.GetHeader(up.HttpHeaderTypeRequest, "x-request-id")
	if !ok || ridBuf.Len == 0 {
		return
	}
	requestID := ridBuf.ToString()

	statusFilter, ok := correlatorPending.loadAndDelete(requestID)
	if !ok {
		return
	}

	timing := h.GetTimingInfo()
	durationMs := int64(-1)
	if timing.RequestCompleteDurationNs >= 0 {
		durationMs = timing.RequestCompleteDurationNs / 1_000_000
	}
	b := h.GetBytesInfo()

	entry := map[string]any{
		"request_id":    requestID,
		"status_filter": statusFilter,
		"response_code": h.GetResponseCode(),
		"duration_ms":   durationMs,
		"bytes_sent":    b.BytesSent,
		"flags":         up.ResponseFlagsString(h.GetResponseFlags()),
	}

	body, _ := json.Marshal(entry)
	resp, err := http.Post(l.sinkURL+"/correlate", "application/json", bytes2reader(body))
	if err != nil {
		fmt.Fprintf(os.Stderr, "e2e-correlator-logger: POST failed: %v\n", err)
		return
	}
	resp.Body.Close()
}
