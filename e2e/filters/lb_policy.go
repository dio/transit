package filters

import "github.com/dio/transit/up"

func init() {
	up.RegisterLBPolicy("first-host", &lbFirstHostFactory{})
}

type lbFirstHostFactory struct{}

func (f *lbFirstHostFactory) Create(_ []byte) (up.LBPolicyConfigFactory, error) {
	return &lbFirstHostConfigFactory{}, nil
}

type lbFirstHostConfigFactory struct{}

func (cf *lbFirstHostConfigFactory) NewLBPolicy() up.LBPolicy { return &lbFirstHostPolicy{} }
func (cf *lbFirstHostConfigFactory) Close()                   {}

type lbFirstHostPolicy struct{ up.EmptyLBPolicy }

func (p *lbFirstHostPolicy) ChooseHost(lb up.LBHandle, _ up.LBContext, priority *uint32, index *uint32) bool {
	if lb.HealthyHostCount(0) == 0 {
		return false
	}
	*priority, *index = 0, 0
	return true
}
