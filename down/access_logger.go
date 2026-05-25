package down

import (
	"sync"

	"github.com/envoyproxy/envoy/source/extensions/dynamic_modules/sdk/go/shared"
)

// TimingInfo holds finalized stream timing from Envoy StreamInfo.
// All durations are in nanoseconds; -1 means the timing is unavailable.
type TimingInfo struct {
	StartTimeUnixNs               int64
	RequestCompleteDurationNs     int64
	FirstUpstreamTxByteSentNs     int64
	LastUpstreamTxByteSentNs      int64
	FirstUpstreamRxByteReceivedNs int64
	LastUpstreamRxByteReceivedNs  int64
	FirstDownstreamTxByteSentNs   int64
	LastDownstreamTxByteSentNs    int64
}

// BytesInfo holds finalized byte counts from Envoy StreamInfo.
type BytesInfo struct {
	BytesReceived     uint64
	BytesSent         uint64
	WireBytesReceived uint64
	WireBytesSent     uint64
}

// AccessLogType identifies the type of access log event.
type AccessLogType int32

const (
	AccessLogTypeNotSet                                  AccessLogType = 0
	AccessLogTypeTcpUpstreamConnected                    AccessLogType = 1
	AccessLogTypeTcpPeriodic                             AccessLogType = 2
	AccessLogTypeTcpConnectionEnd                        AccessLogType = 3
	AccessLogTypeDownstreamStart                         AccessLogType = 4
	AccessLogTypeDownstreamPeriodic                      AccessLogType = 5
	AccessLogTypeDownstreamEnd                           AccessLogType = 6
	AccessLogTypeUpstreamPoolReady                       AccessLogType = 7
	AccessLogTypeUpstreamPeriodic                        AccessLogType = 8
	AccessLogTypeUpstreamEnd                             AccessLogType = 9
	AccessLogTypeDownstreamTunnelSuccessfullyEstablished AccessLogType = 10
	AccessLogTypeUdpTunnelUpstreamConnected              AccessLogType = 11
	AccessLogTypeUdpPeriodic                             AccessLogType = 12
	AccessLogTypeUdpSessionEnd                           AccessLogType = 13
)

// HttpHeaderType selects which header map to read in AccessLoggerHandle.GetHeader.
type HttpHeaderType int32

const (
	HttpHeaderTypeRequest         HttpHeaderType = 0
	HttpHeaderTypeRequestTrailer  HttpHeaderType = 1
	HttpHeaderTypeResponse        HttpHeaderType = 2
	HttpHeaderTypeResponseTrailer HttpHeaderType = 3
)

// AccessLoggerHandle provides access to finalized stream state during OnLog.
// Valid only for the duration of the OnLog callback; do not retain it.
type AccessLoggerHandle interface {
	// GetTimingInfo returns finalized stream timing. -1 means unavailable.
	GetTimingInfo() TimingInfo

	// GetBytesInfo returns finalized byte counts.
	GetBytesInfo() BytesInfo

	// GetResponseFlags returns Envoy response flags as a bitmask.
	// Pass to ResponseFlagsString for the human-readable representation.
	GetResponseFlags() uint64

	// GetResponseCode returns the finalized HTTP response code.
	GetResponseCode() uint32

	// GetAttributeString returns a finalized string stream attribute.
	GetAttributeString(id shared.AttributeID) (shared.UnsafeEnvoyBuffer, bool)

	// GetAttributeInt returns a finalized integer stream attribute.
	GetAttributeInt(id shared.AttributeID) (int64, bool)

	// GetAttributeBool returns a finalized bool stream attribute.
	GetAttributeBool(id shared.AttributeID) (bool, bool)

	// GetHeader retrieves a header value from the specified header map.
	GetHeader(headerType HttpHeaderType, key string) (shared.UnsafeEnvoyBuffer, bool)

	// GetWorkerIndex returns the worker index for this access log event.
	GetWorkerIndex() uint32

	// GetTraceID returns the trace ID from the active span, if tracing is enabled.
	GetTraceID() (shared.UnsafeEnvoyBuffer, bool)

	// GetSpanID returns the span ID from the active span, if tracing is enabled.
	GetSpanID() (shared.UnsafeEnvoyBuffer, bool)

	// IsTraceSampled reports whether the request was sampled for tracing.
	IsTraceSampled() bool

	// GetLocalReplyBody returns the body Envoy sent in a local reply, if any.
	GetLocalReplyBody() (shared.UnsafeEnvoyBuffer, bool)

	// GetUpstreamPoolReadyDurationNs returns nanoseconds spent waiting for an
	// upstream connection from the pool. -1 if unavailable.
	GetUpstreamPoolReadyDurationNs() int64

	// GetUpstreamRequestAttemptCount returns the number of upstream attempts
	// (> 1 means retries occurred).
	GetUpstreamRequestAttemptCount() uint32

	// Log emits a message via Envoy's logging system.
	Log(level shared.LogLevel, format string, args ...any)
}

// AccessLoggerConfigHandle is passed to AccessLoggerConfigFactory.Create on the
// main thread. Use it to define Envoy metrics during initialization.
type AccessLoggerConfigHandle interface {
	// Log emits a message via Envoy's logging system.
	Log(level shared.LogLevel, format string, args ...any)

	// DefineCounter registers a counter metric; returns its ID for later use.
	DefineCounter(name string, tagKeys ...string) (shared.MetricID, shared.MetricsResult)

	// DefineGauge registers a gauge metric.
	DefineGauge(name string, tagKeys ...string) (shared.MetricID, shared.MetricsResult)

	// DefineHistogram registers a histogram metric.
	DefineHistogram(name string, tagKeys ...string) (shared.MetricID, shared.MetricsResult)
}

// AccessLogger is the per-worker-thread logger instance.
type AccessLogger interface {
	// OnLog is called for each access log event.
	// handle is valid only for the duration of this call; do not retain it.
	OnLog(handle AccessLoggerHandle, logType AccessLogType)

	// OnDestroy is called when this logger instance is being destroyed.
	OnDestroy()
}

// AccessLoggerFactory creates AccessLogger instances, one per worker thread.
// Implementations must be safe for concurrent use.
type AccessLoggerFactory interface {
	// NewLogger creates a logger instance for one worker thread.
	NewLogger() AccessLogger

	// OnDestroy is called when the factory is being destroyed.
	OnDestroy()
}

// AccessLoggerConfigFactory is created once on the main thread.
// It parses the logger config and vends AccessLoggerFactory instances.
type AccessLoggerConfigFactory interface {
	// Create is called on the main thread when the access logger config is loaded.
	// config is the raw bytes from the Envoy YAML logger_config field.
	Create(handle AccessLoggerConfigHandle, config []byte) (AccessLoggerFactory, error)
}

// EmptyAccessLogger is a no-op base; embed it to skip unused methods.
type EmptyAccessLogger struct{}

func (e *EmptyAccessLogger) OnLog(_ AccessLoggerHandle, _ AccessLogType) {}
func (e *EmptyAccessLogger) OnDestroy()                                  {}

// =============================================================================
// Access logger registry
// =============================================================================

var (
	accessLoggerMu       sync.RWMutex
	accessLoggerRegistry = map[string]AccessLoggerConfigFactory{}
)

// RegisterAccessLoggerConfigFactory adds a named access logger factory.
// Called by up.RegisterAccessLogger; must be called from an init() function.
func RegisterAccessLoggerConfigFactory(name string, f AccessLoggerConfigFactory) {
	accessLoggerMu.Lock()
	defer accessLoggerMu.Unlock()
	if _, ok := accessLoggerRegistry[name]; ok {
		panic("down: access logger already registered: " + name)
	}
	accessLoggerRegistry[name] = f
}

// GetAccessLoggerConfigFactory returns the factory for the given name,
// or nil if not found. Called by down/abi_impl.
func GetAccessLoggerConfigFactory(name string) AccessLoggerConfigFactory {
	accessLoggerMu.RLock()
	defer accessLoggerMu.RUnlock()
	return accessLoggerRegistry[name]
}
