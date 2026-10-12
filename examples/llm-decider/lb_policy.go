package llmdecider

import "github.com/dio/transit/up"

// firstHealthyFactory is the LB Policy surface.
//
// An LB Policy chooses among hosts Envoy owns, so it sees indices rather than
// endpoint identities. Model-aware selection needs the model→host mapping, which
// is the Cluster Extension's to hold; this policy demonstrates the surface and a
// healthy-host pick, and would carry per-host load or locality logic in a real
// deployment.
type firstHealthyFactory struct{}

func (f *firstHealthyFactory) Create(_ []byte) (up.LBPolicyConfigFactory, error) {
	return &firstHealthyConfigFactory{}, nil
}

type firstHealthyConfigFactory struct{}

func (cf *firstHealthyConfigFactory) NewLBPolicy() up.LBPolicy { return &firstHealthyPolicy{} }
func (cf *firstHealthyConfigFactory) Close()                   {}

type firstHealthyPolicy struct{ up.EmptyLBPolicy }

func (p *firstHealthyPolicy) ChooseHost(
	lb up.LBHandle,
	_ up.LBContext,
	priority *uint32,
	index *uint32,
) bool {
	if lb.HealthyHostCount(0) == 0 {
		return false
	}
	*priority, *index = 0, 0
	return true
}
