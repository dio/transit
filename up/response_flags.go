package up

// Response flag constants match Envoy's %RESPONSE_FLAGS% access log format.
// ResponseFlagsString converts the bitmask from AccessLoggerHandle.GetResponseFlags()
// to a comma-separated string using these tokens (e.g. "UF,UT").
const (
	ResponseFlagFailedLocalHealthCheck          = "LH"
	ResponseFlagNoHealthyUpstream               = "UH"
	ResponseFlagUpstreamRequestTimeout          = "UT"
	ResponseFlagLocalReset                      = "LR"
	ResponseFlagUpstreamRemoteReset             = "UR"
	ResponseFlagUpstreamConnectionFailure       = "UF"
	ResponseFlagUpstreamConnectionTermination   = "UC"
	ResponseFlagUpstreamOverflow                = "UO"
	ResponseFlagNoRouteFound                    = "NR"
	ResponseFlagDelayInjected                   = "DI"
	ResponseFlagFaultInjected                   = "FI"
	ResponseFlagRateLimited                     = "RL"
	ResponseFlagUnauthorizedExternalService     = "UAEX"
	ResponseFlagRateLimitServiceError           = "RLSE"
	ResponseFlagDownstreamConnectionTermination = "DC"
	ResponseFlagUpstreamRetryLimitExceeded      = "URX"
	ResponseFlagStreamIdleTimeout               = "SI"
	ResponseFlagInvalidRequestHeaders           = "IH"
	ResponseFlagDownstreamProtocolError         = "DPE"
	ResponseFlagUpstreamMaxStreamDuration       = "UMSDR"
	ResponseFlagResponseFromCacheFilter         = "RFCF"
	ResponseFlagNoFilterConfigFound             = "NFCF"
	ResponseFlagDurationTimeout                 = "DT"
	ResponseFlagUpstreamProtocolError           = "UPE"
	ResponseFlagNoClusterFound                  = "NC"
	ResponseFlagOverloadManager                 = "OM"
)
