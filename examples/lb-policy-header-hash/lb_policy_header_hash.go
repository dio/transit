package lbpolicyheaderhash

import (
	"hash/fnv"

	"github.com/dio/transit/up"
)

func init() {
	up.RegisterLBPolicy("header-hash", &headerHashFactory{})
}

type headerHashFactory struct{}

func (f *headerHashFactory) Create(_ []byte) (up.LBPolicyConfigFactory, error) {
	return &headerHashConfigFactory{}, nil
}

type headerHashConfigFactory struct{}

func (cf *headerHashConfigFactory) NewLBPolicy() up.LBPolicy { return &headerHashPolicy{} }
func (cf *headerHashConfigFactory) Close()                   {}

type headerHashPolicy struct{ up.EmptyLBPolicy }

func (p *headerHashPolicy) ChooseHost(lb up.LBHandle, ctx up.LBContext, priority *uint32, index *uint32) bool {
	n := lb.HealthyHostCount(0)
	if n == 0 {
		return false
	}
	*priority = 0
	val, ok := ctx.GetHeader("x-session-id")
	if !ok || val == "" {
		*index = 0
		return true
	}
	h := fnv.New32a()
	h.Write([]byte(val))
	*index = uint32(int(h.Sum32()) % n)
	return true
}
