package up

import "github.com/envoyproxy/envoy/source/extensions/dynamic_modules/sdk/go/shared"

// AttributeID identifies a stream attribute readable via Writer.GetAttributeString etc.
type AttributeID uint32

// Request attributes.
const (
	AttributeIDRequestPath      AttributeID = AttributeID(shared.AttributeIDRequestPath)
	AttributeIDRequestUrlPath   AttributeID = AttributeID(shared.AttributeIDRequestUrlPath)
	AttributeIDRequestHost      AttributeID = AttributeID(shared.AttributeIDRequestHost)
	AttributeIDRequestScheme    AttributeID = AttributeID(shared.AttributeIDRequestScheme)
	AttributeIDRequestMethod    AttributeID = AttributeID(shared.AttributeIDRequestMethod)
	AttributeIDRequestHeaders   AttributeID = AttributeID(shared.AttributeIDRequestHeaders)
	AttributeIDRequestReferer   AttributeID = AttributeID(shared.AttributeIDRequestReferer)
	AttributeIDRequestUserAgent AttributeID = AttributeID(shared.AttributeIDRequestUserAgent)
	AttributeIDRequestTime      AttributeID = AttributeID(shared.AttributeIDRequestTime)
	AttributeIDRequestId        AttributeID = AttributeID(shared.AttributeIDRequestId)
	AttributeIDRequestProtocol  AttributeID = AttributeID(shared.AttributeIDRequestProtocol)
	AttributeIDRequestQuery     AttributeID = AttributeID(shared.AttributeIDRequestQuery)
	AttributeIDRequestDuration  AttributeID = AttributeID(shared.AttributeIDRequestDuration)
	AttributeIDRequestSize      AttributeID = AttributeID(shared.AttributeIDRequestSize)
	AttributeIDRequestTotalSize AttributeID = AttributeID(shared.AttributeIDRequestTotalSize)
)

// Response attributes.
const (
	AttributeIDResponseCode           AttributeID = AttributeID(shared.AttributeIDResponseCode)
	AttributeIDResponseCodeDetails    AttributeID = AttributeID(shared.AttributeIDResponseCodeDetails)
	AttributeIDResponseFlags          AttributeID = AttributeID(shared.AttributeIDResponseFlags)
	AttributeIDResponseGrpcStatus     AttributeID = AttributeID(shared.AttributeIDResponseGrpcStatus)
	AttributeIDResponseHeaders        AttributeID = AttributeID(shared.AttributeIDResponseHeaders)
	AttributeIDResponseTrailers       AttributeID = AttributeID(shared.AttributeIDResponseTrailers)
	AttributeIDResponseSize           AttributeID = AttributeID(shared.AttributeIDResponseSize)
	AttributeIDResponseTotalSize      AttributeID = AttributeID(shared.AttributeIDResponseTotalSize)
	AttributeIDResponseBackendLatency AttributeID = AttributeID(shared.AttributeIDResponseBackendLatency)
)

// Source / destination / connection attributes.
const (
	AttributeIDSourceAddress      AttributeID = AttributeID(shared.AttributeIDSourceAddress)
	AttributeIDSourcePort         AttributeID = AttributeID(shared.AttributeIDSourcePort)
	AttributeIDDestinationAddress AttributeID = AttributeID(shared.AttributeIDDestinationAddress)
	AttributeIDDestinationPort    AttributeID = AttributeID(shared.AttributeIDDestinationPort)
	AttributeIDConnectionId       AttributeID = AttributeID(shared.AttributeIDConnectionId)

	// MTLS / TLS — acronyms use all-caps per Go conventions (changed in SDK 1.39-dev).
	AttributeIDConnectionMTLS       AttributeID = AttributeID(shared.AttributeIDConnectionMTLS)
	AttributeIDConnectionTLSVersion AttributeID = AttributeID(shared.AttributeIDConnectionTLSVersion)

	AttributeIDConnectionRequestedServerName         AttributeID = AttributeID(shared.AttributeIDConnectionRequestedServerName)
	AttributeIDConnectionSubjectLocalCertificate     AttributeID = AttributeID(shared.AttributeIDConnectionSubjectLocalCertificate)
	AttributeIDConnectionSubjectPeerCertificate      AttributeID = AttributeID(shared.AttributeIDConnectionSubjectPeerCertificate)
	AttributeIDConnectionDNSSanLocalCertificate      AttributeID = AttributeID(shared.AttributeIDConnectionDNSSanLocalCertificate)
	AttributeIDConnectionDNSSanPeerCertificate       AttributeID = AttributeID(shared.AttributeIDConnectionDNSSanPeerCertificate)
	AttributeIDConnectionURISanLocalCertificate      AttributeID = AttributeID(shared.AttributeIDConnectionURISanLocalCertificate)
	AttributeIDConnectionURISanPeerCertificate       AttributeID = AttributeID(shared.AttributeIDConnectionURISanPeerCertificate)
	AttributeIDConnectionSha256PeerCertificateDigest AttributeID = AttributeID(shared.AttributeIDConnectionSha256PeerCertificateDigest)
	AttributeIDConnectionTransportFailureReason      AttributeID = AttributeID(shared.AttributeIDConnectionTransportFailureReason)
	AttributeIDConnectionTerminationDetails          AttributeID = AttributeID(shared.AttributeIDConnectionTerminationDetails)
)

// Upstream attributes.
const (
	AttributeIDUpstreamAddress                AttributeID = AttributeID(shared.AttributeIDUpstreamAddress)
	AttributeIDUpstreamPort                   AttributeID = AttributeID(shared.AttributeIDUpstreamPort)
	AttributeIDUpstreamLocalAddress           AttributeID = AttributeID(shared.AttributeIDUpstreamLocalAddress)
	AttributeIDUpstreamTransportFailureReason AttributeID = AttributeID(shared.AttributeIDUpstreamTransportFailureReason)
	AttributeIDUpstreamRequestAttemptCount    AttributeID = AttributeID(shared.AttributeIDUpstreamRequestAttemptCount)
	AttributeIDUpstreamCxPoolReadyDuration    AttributeID = AttributeID(shared.AttributeIDUpstreamCxPoolReadyDuration)
	AttributeIDUpstreamLocality               AttributeID = AttributeID(shared.AttributeIDUpstreamLocality)

	// TLS / DNS / URI — acronyms use all-caps per Go conventions (changed in SDK 1.39-dev).
	AttributeIDUpstreamTLSVersion                  AttributeID = AttributeID(shared.AttributeIDUpstreamTLSVersion)
	AttributeIDUpstreamSubjectLocalCertificate     AttributeID = AttributeID(shared.AttributeIDUpstreamSubjectLocalCertificate)
	AttributeIDUpstreamSubjectPeerCertificate      AttributeID = AttributeID(shared.AttributeIDUpstreamSubjectPeerCertificate)
	AttributeIDUpstreamDNSSanLocalCertificate      AttributeID = AttributeID(shared.AttributeIDUpstreamDNSSanLocalCertificate)
	AttributeIDUpstreamDNSSanPeerCertificate       AttributeID = AttributeID(shared.AttributeIDUpstreamDNSSanPeerCertificate)
	AttributeIDUpstreamURISanLocalCertificate      AttributeID = AttributeID(shared.AttributeIDUpstreamURISanLocalCertificate)
	AttributeIDUpstreamURISanPeerCertificate       AttributeID = AttributeID(shared.AttributeIDUpstreamURISanPeerCertificate)
	AttributeIDUpstreamSha256PeerCertificateDigest AttributeID = AttributeID(shared.AttributeIDUpstreamSha256PeerCertificateDigest)
)

// XDS / metadata attributes.
const (
	AttributeIDXdsNode                 AttributeID = AttributeID(shared.AttributeIDXdsNode)
	AttributeIDXdsClusterName          AttributeID = AttributeID(shared.AttributeIDXdsClusterName)
	AttributeIDXdsClusterMetadata      AttributeID = AttributeID(shared.AttributeIDXdsClusterMetadata)
	AttributeIDXdsFilterChainName      AttributeID = AttributeID(shared.AttributeIDXdsFilterChainName)
	AttributeIDXdsListenerDirection    AttributeID = AttributeID(shared.AttributeIDXdsListenerDirection)
	AttributeIDXdsListenerMetadata     AttributeID = AttributeID(shared.AttributeIDXdsListenerMetadata)
	AttributeIDXdsRouteMetadata        AttributeID = AttributeID(shared.AttributeIDXdsRouteMetadata)
	AttributeIDXdsRouteName            AttributeID = AttributeID(shared.AttributeIDXdsRouteName)
	AttributeIDXdsVirtualHostName      AttributeID = AttributeID(shared.AttributeIDXdsVirtualHostName)
	AttributeIDXdsVirtualHostMetadata  AttributeID = AttributeID(shared.AttributeIDXdsVirtualHostMetadata)
	AttributeIDXdsUpstreamHostMetadata AttributeID = AttributeID(shared.AttributeIDXdsUpstreamHostMetadata)
)

// Health check attribute.
const (
	AttributeIDHealthCheck AttributeID = AttributeID(shared.AttributeIDHealthCheck)
)
