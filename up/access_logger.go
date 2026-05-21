package up

import "github.com/dio/transit/down"

// Access logger type aliases — all types are defined in down to avoid import
// cycles (down/abi_impl implements them and imports down, not up).
// Users interact exclusively through these up-package names.

type (
	TimingInfo     = down.TimingInfo
	BytesInfo      = down.BytesInfo
	AccessLogType  = down.AccessLogType
	HttpHeaderType = down.HttpHeaderType

	AccessLoggerHandle        = down.AccessLoggerHandle
	AccessLoggerConfigHandle  = down.AccessLoggerConfigHandle
	AccessLogger              = down.AccessLogger
	AccessLoggerFactory       = down.AccessLoggerFactory
	AccessLoggerConfigFactory = down.AccessLoggerConfigFactory

	EmptyAccessLogger = down.EmptyAccessLogger
)

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
