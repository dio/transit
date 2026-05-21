package up

import "github.com/envoyproxy/envoy/source/extensions/dynamic_modules/sdk/go/shared"

// AttributeID identifies a stream attribute readable via Writer.GetAttributeString etc.
type AttributeID = shared.AttributeID

// Request attributes.
const (
	AttributeIDRequestPath      = shared.AttributeIDRequestPath
	AttributeIDRequestUrlPath   = shared.AttributeIDRequestUrlPath
	AttributeIDRequestHost      = shared.AttributeIDRequestHost
	AttributeIDRequestScheme    = shared.AttributeIDRequestScheme
	AttributeIDRequestMethod    = shared.AttributeIDRequestMethod
	AttributeIDRequestHeaders   = shared.AttributeIDRequestHeaders
	AttributeIDRequestReferer   = shared.AttributeIDRequestReferer
	AttributeIDRequestUserAgent = shared.AttributeIDRequestUserAgent
	AttributeIDRequestTime      = shared.AttributeIDRequestTime
	AttributeIDRequestId        = shared.AttributeIDRequestId
	AttributeIDRequestProtocol  = shared.AttributeIDRequestProtocol
	AttributeIDRequestQuery     = shared.AttributeIDRequestQuery
	AttributeIDRequestDuration  = shared.AttributeIDRequestDuration
	AttributeIDRequestSize      = shared.AttributeIDRequestSize
	AttributeIDRequestTotalSize = shared.AttributeIDRequestTotalSize
)

// Response attributes.
const (
	AttributeIDResponseCode           = shared.AttributeIDResponseCode
	AttributeIDResponseCodeDetails    = shared.AttributeIDResponseCodeDetails
	AttributeIDResponseFlags          = shared.AttributeIDResponseFlags
	AttributeIDResponseGrpcStatus     = shared.AttributeIDResponseGrpcStatus
	AttributeIDResponseHeaders        = shared.AttributeIDResponseHeaders
	AttributeIDResponseTrailers       = shared.AttributeIDResponseTrailers
	AttributeIDResponseSize           = shared.AttributeIDResponseSize
	AttributeIDResponseTotalSize      = shared.AttributeIDResponseTotalSize
	AttributeIDResponseBackendLatency = shared.AttributeIDResponseBackendLatency
)

// Source / destination / connection attributes.
const (
	AttributeIDSourceAddress      = shared.AttributeIDSourceAddress
	AttributeIDSourcePort         = shared.AttributeIDSourcePort
	AttributeIDDestinationAddress = shared.AttributeIDDestinationAddress
	AttributeIDDestinationPort    = shared.AttributeIDDestinationPort
	AttributeIDConnectionId       = shared.AttributeIDConnectionId
	AttributeIDConnectionMtls     = shared.AttributeIDConnectionMtls

	AttributeIDConnectionRequestedServerName         = shared.AttributeIDConnectionRequestedServerName
	AttributeIDConnectionTlsVersion                  = shared.AttributeIDConnectionTlsVersion
	AttributeIDConnectionSubjectLocalCertificate     = shared.AttributeIDConnectionSubjectLocalCertificate
	AttributeIDConnectionSubjectPeerCertificate      = shared.AttributeIDConnectionSubjectPeerCertificate
	AttributeIDConnectionDnsSanLocalCertificate      = shared.AttributeIDConnectionDnsSanLocalCertificate
	AttributeIDConnectionDnsSanPeerCertificate       = shared.AttributeIDConnectionDnsSanPeerCertificate
	AttributeIDConnectionUriSanLocalCertificate      = shared.AttributeIDConnectionUriSanLocalCertificate
	AttributeIDConnectionUriSanPeerCertificate       = shared.AttributeIDConnectionUriSanPeerCertificate
	AttributeIDConnectionSha256PeerCertificateDigest = shared.AttributeIDConnectionSha256PeerCertificateDigest
	AttributeIDConnectionTransportFailureReason      = shared.AttributeIDConnectionTransportFailureReason
	AttributeIDConnectionTerminationDetails          = shared.AttributeIDConnectionTerminationDetails
)

// Upstream attributes.
const (
	AttributeIDUpstreamAddress                     = shared.AttributeIDUpstreamAddress
	AttributeIDUpstreamPort                        = shared.AttributeIDUpstreamPort
	AttributeIDUpstreamLocalAddress                = shared.AttributeIDUpstreamLocalAddress
	AttributeIDUpstreamTransportFailureReason      = shared.AttributeIDUpstreamTransportFailureReason
	AttributeIDUpstreamRequestAttemptCount         = shared.AttributeIDUpstreamRequestAttemptCount
	AttributeIDUpstreamCxPoolReadyDuration         = shared.AttributeIDUpstreamCxPoolReadyDuration
	AttributeIDUpstreamLocality                    = shared.AttributeIDUpstreamLocality
	AttributeIDUpstreamTlsVersion                  = shared.AttributeIDUpstreamTlsVersion
	AttributeIDUpstreamSubjectLocalCertificate     = shared.AttributeIDUpstreamSubjectLocalCertificate
	AttributeIDUpstreamSubjectPeerCertificate      = shared.AttributeIDUpstreamSubjectPeerCertificate
	AttributeIDUpstreamDnsSanLocalCertificate      = shared.AttributeIDUpstreamDnsSanLocalCertificate
	AttributeIDUpstreamDnsSanPeerCertificate       = shared.AttributeIDUpstreamDnsSanPeerCertificate
	AttributeIDUpstreamUriSanLocalCertificate      = shared.AttributeIDUpstreamUriSanLocalCertificate
	AttributeIDUpstreamUriSanPeerCertificate       = shared.AttributeIDUpstreamUriSanPeerCertificate
	AttributeIDUpstreamSha256PeerCertificateDigest = shared.AttributeIDUpstreamSha256PeerCertificateDigest
)

// XDS / metadata attributes.
const (
	AttributeIDXdsNode            = shared.AttributeIDXdsNode
	AttributeIDXdsClusterName     = shared.AttributeIDXdsClusterName
	AttributeIDXdsClusterMetadata = shared.AttributeIDXdsClusterMetadata
)
