// Package down bridges the official Envoy SDK registration and ABI layer.
// It also defines access logger types — absent from the official SDK — that
// down/abi_impl implements via CGO //export symbols.
//
// Callers never import this package directly; transit/up re-exports everything.
package down

import (
	"strconv"
	"strings"
	"sync"

	sdk "github.com/envoyproxy/envoy/source/extensions/dynamic_modules/sdk/go"
	"github.com/envoyproxy/envoy/source/extensions/dynamic_modules/sdk/go/shared"
)

// RegisterHttpFilter wires one named HTTP filter factory into the official SDK
// registry. Called by up.Register; must be called from an init() function.
func RegisterHttpFilter(name string, factory shared.HttpFilterConfigFactory) {
	sdk.RegisterHttpFilterConfigFactories(map[string]shared.HttpFilterConfigFactory{
		name: factory,
	})
}

// =============================================================================
// Access logger types (official SDK has no access logger API)
// =============================================================================

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

// =============================================================================
// ResponseFlagsString
// =============================================================================

// ResponseFlagsString converts the GetResponseFlags() bitmask to Envoy's
// human-readable flag string (e.g. "UF,UT"), matching %RESPONSE_FLAGS%.
// Returns "" when mask is 0.
func ResponseFlagsString(mask uint64) string {
	if mask == 0 {
		return ""
	}
	var out []string
	for i, name := range responseFlagNames {
		if mask&(1<<uint(i)) != 0 {
			out = append(out, name)
		}
	}
	for i := len(responseFlagNames); i < 64; i++ {
		if mask&(1<<uint(i)) != 0 {
			out = append(out, "0x"+strconv.FormatUint(1<<uint(i), 16))
		}
	}
	return strings.Join(out, ",")
}

// responseFlagNames maps CoreResponseFlag bit positions to their short strings,
// matching Envoy's %RESPONSE_FLAGS% access log format.
var responseFlagNames = [...]string{
	"LH",    // 0  FailedLocalHealthCheck
	"UH",    // 1  NoHealthyUpstream
	"UT",    // 2  UpstreamRequestTimeout
	"LR",    // 3  LocalReset
	"UR",    // 4  UpstreamRemoteReset
	"UF",    // 5  UpstreamConnectionFailure
	"UC",    // 6  UpstreamConnectionTermination
	"UO",    // 7  UpstreamOverflow
	"NR",    // 8  NoRouteFound
	"DI",    // 9  DelayInjected
	"FI",    // 10 FaultInjected
	"RL",    // 11 RateLimited
	"UAEX",  // 12 UnauthorizedExternalService
	"RLSE",  // 13 RateLimitServiceError
	"DC",    // 14 DownstreamConnectionTermination
	"URX",   // 15 UpstreamRetryLimitExceeded
	"SI",    // 16 StreamIdleTimeout
	"IH",    // 17 InvalidEnvoyRequestHeaders
	"DPE",   // 18 DownstreamProtocolError
	"UMSDR", // 19 UpstreamMaxStreamDurationReached
	"RFCF",  // 20 ResponseFromCacheFilter
	"NFCF",  // 21 NoFilterConfigFound
	"DT",    // 22 DurationTimeout
	"UPE",   // 23 UpstreamProtocolError
	"NC",    // 24 NoClusterFound
	"OM",    // 25 OverloadManager
}
