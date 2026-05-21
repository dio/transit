// Package lbpolicy demonstrates the LB Policy extension point: Envoy manages
// the host set via static config or EDS; the module only implements ChooseHost.
//
// Policy: "first-host" — always picks the first healthy host at priority 0,
// index 0. Responses 503 when the healthy-host list is empty.
//
// To build the shared library:
//
//	CGO_ENABLED=1 go build -trimpath -buildmode=c-shared \
//	  -o liblb-policy.so ./lb-policy/cmd
//
// (run from the examples/ directory)
package lbpolicy

import "github.com/dio/transit/up"

func init() {
	up.RegisterLBPolicy("first-host", &firstHostFactory{})
}

// firstHostFactory parses config (none required) and creates per-cluster
// config factories.
type firstHostFactory struct{}

func (f *firstHostFactory) Create(_ []byte) (up.LBPolicyConfigFactory, error) {
	return &firstHostConfigFactory{}, nil
}

// firstHostConfigFactory creates per-worker-thread LB instances.
type firstHostConfigFactory struct{}

func (cf *firstHostConfigFactory) NewLBPolicy() up.LBPolicy { return &firstHostPolicy{} }
func (cf *firstHostConfigFactory) Close()                   {}

// firstHostPolicy always returns (priority=0, index=0, ok=true) when at least
// one healthy host exists, signalling Envoy to use the first available host.
type firstHostPolicy struct{ up.EmptyLBPolicy }

func (p *firstHostPolicy) ChooseHost(lb up.LBHandle, _ up.LBContext, priority *uint32, index *uint32) bool {
	if lb.HealthyHostCount(0) == 0 {
		return false
	}
	*priority, *index = 0, 0
	return true
}
