// Package lbpolicymetadataroute demonstrates capability-based routing using
// Envoy endpoint filter_metadata. The policy reads the x-required-capability
// request header and selects the first healthy host whose filter_metadata
// (namespace "envoy.lb", key "capability") matches the header value. When the
// header is absent or no host matches, the policy falls back to index 0.
//
// To build the shared library:
//
//	CGO_ENABLED=1 go build -trimpath -buildmode=c-shared \
//	  -o liblb-policy-metadata-route.so ./lb-policy-metadata-route/cmd
//
// (run from the examples/ directory)
package lbpolicymetadataroute

import "github.com/dio/transit/up"

func init() {
	up.RegisterLBPolicy("metadata-route", &metadataRouteFactory{})
}

type metadataRouteFactory struct{}

func (f *metadataRouteFactory) Create(_ []byte) (up.LBPolicyConfigFactory, error) {
	return &metadataRouteConfigFactory{}, nil
}

type metadataRouteConfigFactory struct{}

func (cf *metadataRouteConfigFactory) NewLBPolicy() up.LBPolicy { return &metadataRoutePolicy{} }
func (cf *metadataRouteConfigFactory) Close()                   {}

type metadataRoutePolicy struct{ up.EmptyLBPolicy }

func (p *metadataRoutePolicy) ChooseHost(lb up.LBHandle, ctx up.LBContext, priority *uint32, index *uint32) bool {
	n := lb.HealthyHostCount(0)
	if n == 0 {
		return false
	}
	*priority = 0
	required, ok := ctx.GetHeader("x-required-capability")
	if !ok || required == "" {
		*index = 0
		return true
	}
	for i := 0; i < n; i++ {
		cap, ok := lb.HostMetadataString(0, i, "envoy.lb", "capability")
		if ok && cap == required {
			*index = uint32(i)
			return true
		}
	}
	// no match — fall back to first
	*index = 0
	return true
}
