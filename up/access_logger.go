package up

import (
	"fmt"

	"github.com/dio/transit/down"
	"github.com/envoyproxy/envoy/source/extensions/dynamic_modules/sdk/go/shared"
)

type (
	TimingInfo     = down.TimingInfo
	BytesInfo      = down.BytesInfo
	AccessLogType  = down.AccessLogType
	HttpHeaderType = down.HttpHeaderType
)

// AccessLoggerHandle provides access to finalized stream state during OnLog.
// Values backed by Envoy memory are returned as Buffer.
type AccessLoggerHandle interface {
	GetTimingInfo() TimingInfo
	GetBytesInfo() BytesInfo
	GetResponseFlags() uint64
	GetResponseCode() uint32
	GetAttributeString(id AttributeID) (Buffer, bool)
	GetAttributeInt(id AttributeID) (int64, bool)
	GetAttributeBool(id AttributeID) (bool, bool)
	GetHeader(headerType HttpHeaderType, key string) (Buffer, bool)
	GetWorkerIndex() uint32
	GetTraceID() (Buffer, bool)
	GetSpanID() (Buffer, bool)
	IsTraceSampled() bool
	GetLocalReplyBody() (Buffer, bool)
	GetUpstreamPoolReadyDurationNs() int64
	GetUpstreamRequestAttemptCount() uint32
	Log(level LogLevel, format string, args ...any)
}

// AccessLoggerConfigHandle is passed to AccessLoggerConfigFactory.Create on the
// main thread. Use it to define Envoy metrics during initialization.
type AccessLoggerConfigHandle interface {
	Log(level LogLevel, format string, args ...any)
	DefineCounter(name string, tagKeys ...string) (MetricID, error)
	DefineGauge(name string, tagKeys ...string) (MetricID, error)
	DefineHistogram(name string, tagKeys ...string) (MetricID, error)
}

// AccessLogger is the per-worker-thread logger instance.
type AccessLogger interface {
	OnLog(handle AccessLoggerHandle, logType AccessLogType)
	OnDestroy()
}

// AccessLoggerFactory creates AccessLogger instances, one per worker thread.
type AccessLoggerFactory interface {
	NewLogger() AccessLogger
	OnDestroy()
}

// AccessLoggerConfigFactory parses the logger config and creates factories.
type AccessLoggerConfigFactory interface {
	Create(handle AccessLoggerConfigHandle, config []byte) (AccessLoggerFactory, error)
}

// EmptyAccessLogger is a no-op base; embed it to skip unused methods.
type EmptyAccessLogger struct{}

func (e *EmptyAccessLogger) OnLog(_ AccessLoggerHandle, _ AccessLogType) {}
func (e *EmptyAccessLogger) OnDestroy()                                  {}

// AccessLogType constants.
const (
	AccessLogTypeNotSet                                  = down.AccessLogTypeNotSet
	AccessLogTypeTcpUpstreamConnected                    = down.AccessLogTypeTcpUpstreamConnected
	AccessLogTypeTcpPeriodic                             = down.AccessLogTypeTcpPeriodic
	AccessLogTypeTcpConnectionEnd                        = down.AccessLogTypeTcpConnectionEnd
	AccessLogTypeDownstreamStart                         = down.AccessLogTypeDownstreamStart
	AccessLogTypeDownstreamPeriodic                      = down.AccessLogTypeDownstreamPeriodic
	AccessLogTypeDownstreamEnd                           = down.AccessLogTypeDownstreamEnd
	AccessLogTypeUpstreamPoolReady                       = down.AccessLogTypeUpstreamPoolReady
	AccessLogTypeUpstreamPeriodic                        = down.AccessLogTypeUpstreamPeriodic
	AccessLogTypeUpstreamEnd                             = down.AccessLogTypeUpstreamEnd
	AccessLogTypeDownstreamTunnelSuccessfullyEstablished = down.AccessLogTypeDownstreamTunnelSuccessfullyEstablished
	AccessLogTypeUdpTunnelUpstreamConnected              = down.AccessLogTypeUdpTunnelUpstreamConnected
	AccessLogTypeUdpPeriodic                             = down.AccessLogTypeUdpPeriodic
	AccessLogTypeUdpSessionEnd                           = down.AccessLogTypeUdpSessionEnd
)

// HttpHeaderType constants.
const (
	HttpHeaderTypeRequest         = down.HttpHeaderTypeRequest
	HttpHeaderTypeRequestTrailer  = down.HttpHeaderTypeRequestTrailer
	HttpHeaderTypeResponse        = down.HttpHeaderTypeResponse
	HttpHeaderTypeResponseTrailer = down.HttpHeaderTypeResponseTrailer
)

// ResponseFlagsString converts the GetResponseFlags() bitmask to Envoy's
// human-readable flag string (e.g. "UF,UT"), matching %RESPONSE_FLAGS%.
func ResponseFlagsString(mask uint64) string { return down.ResponseFlagsString(mask) }

func registerAccessLogger(name string, f AccessLoggerConfigFactory) {
	down.RegisterAccessLoggerConfigFactory(name, accessLoggerConfigFactoryAdapter{factory: f})
}

type accessLoggerConfigFactoryAdapter struct {
	factory AccessLoggerConfigFactory
}

func (a accessLoggerConfigFactoryAdapter) Create(
	handle down.AccessLoggerConfigHandle,
	config []byte,
) (down.AccessLoggerFactory, error) {
	factory, err := a.factory.Create(accessLoggerConfigHandleAdapter{handle: handle}, config)
	if err != nil {
		return nil, err
	}
	return accessLoggerFactoryAdapter{factory: factory}, nil
}

type accessLoggerConfigHandleAdapter struct {
	handle down.AccessLoggerConfigHandle
}

func (a accessLoggerConfigHandleAdapter) Log(level LogLevel, format string, args ...any) {
	a.handle.Log(shared.LogLevel(level), format, args...)
}

func (a accessLoggerConfigHandleAdapter) DefineCounter(name string, tagKeys ...string) (MetricID, error) {
	id, res := a.handle.DefineCounter(name, tagKeys...)
	if res != shared.MetricsSuccess {
		return 0, fmt.Errorf("up: DefineCounter %q failed (result=%d)", name, res)
	}
	return MetricID(id), nil
}

func (a accessLoggerConfigHandleAdapter) DefineGauge(name string, tagKeys ...string) (MetricID, error) {
	id, res := a.handle.DefineGauge(name, tagKeys...)
	if res != shared.MetricsSuccess {
		return 0, fmt.Errorf("up: DefineGauge %q failed (result=%d)", name, res)
	}
	return MetricID(id), nil
}

func (a accessLoggerConfigHandleAdapter) DefineHistogram(name string, tagKeys ...string) (MetricID, error) {
	id, res := a.handle.DefineHistogram(name, tagKeys...)
	if res != shared.MetricsSuccess {
		return 0, fmt.Errorf("up: DefineHistogram %q failed (result=%d)", name, res)
	}
	return MetricID(id), nil
}

type accessLoggerFactoryAdapter struct {
	factory AccessLoggerFactory
}

func (a accessLoggerFactoryAdapter) NewLogger() down.AccessLogger {
	return accessLoggerAdapter{logger: a.factory.NewLogger()}
}

func (a accessLoggerFactoryAdapter) OnDestroy() {
	a.factory.OnDestroy()
}

type accessLoggerAdapter struct {
	logger AccessLogger
}

func (a accessLoggerAdapter) OnLog(handle down.AccessLoggerHandle, logType down.AccessLogType) {
	a.logger.OnLog(accessLoggerHandleAdapter{handle: handle}, logType)
}

func (a accessLoggerAdapter) OnDestroy() {
	a.logger.OnDestroy()
}

type accessLoggerHandleAdapter struct {
	handle down.AccessLoggerHandle
}

func (a accessLoggerHandleAdapter) GetTimingInfo() TimingInfo {
	return a.handle.GetTimingInfo()
}

func (a accessLoggerHandleAdapter) GetBytesInfo() BytesInfo {
	return a.handle.GetBytesInfo()
}

func (a accessLoggerHandleAdapter) GetResponseFlags() uint64 {
	return a.handle.GetResponseFlags()
}

func (a accessLoggerHandleAdapter) GetResponseCode() uint32 {
	return a.handle.GetResponseCode()
}

func (a accessLoggerHandleAdapter) GetAttributeString(id AttributeID) (Buffer, bool) {
	v, ok := a.handle.GetAttributeString(shared.AttributeID(id))
	if !ok {
		return Buffer{}, false
	}
	return newBuffer(v), true
}

func (a accessLoggerHandleAdapter) GetAttributeInt(id AttributeID) (int64, bool) {
	return a.handle.GetAttributeInt(shared.AttributeID(id))
}

func (a accessLoggerHandleAdapter) GetAttributeBool(id AttributeID) (bool, bool) {
	return a.handle.GetAttributeBool(shared.AttributeID(id))
}

func (a accessLoggerHandleAdapter) GetHeader(headerType HttpHeaderType, key string) (Buffer, bool) {
	v, ok := a.handle.GetHeader(headerType, key)
	if !ok {
		return Buffer{}, false
	}
	return newBuffer(v), true
}

func (a accessLoggerHandleAdapter) GetWorkerIndex() uint32 {
	return a.handle.GetWorkerIndex()
}

func (a accessLoggerHandleAdapter) GetTraceID() (Buffer, bool) {
	v, ok := a.handle.GetTraceID()
	if !ok {
		return Buffer{}, false
	}
	return newBuffer(v), true
}

func (a accessLoggerHandleAdapter) GetSpanID() (Buffer, bool) {
	v, ok := a.handle.GetSpanID()
	if !ok {
		return Buffer{}, false
	}
	return newBuffer(v), true
}

func (a accessLoggerHandleAdapter) IsTraceSampled() bool {
	return a.handle.IsTraceSampled()
}

func (a accessLoggerHandleAdapter) GetLocalReplyBody() (Buffer, bool) {
	v, ok := a.handle.GetLocalReplyBody()
	if !ok {
		return Buffer{}, false
	}
	return newBuffer(v), true
}

func (a accessLoggerHandleAdapter) GetUpstreamPoolReadyDurationNs() int64 {
	return a.handle.GetUpstreamPoolReadyDurationNs()
}

func (a accessLoggerHandleAdapter) GetUpstreamRequestAttemptCount() uint32 {
	return a.handle.GetUpstreamRequestAttemptCount()
}

func (a accessLoggerHandleAdapter) Log(level LogLevel, format string, args ...any) {
	a.handle.Log(shared.LogLevel(level), format, args...)
}
