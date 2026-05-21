package up

import "github.com/dio/transit/down"

// LB Policy type aliases — defined in down to avoid import cycles.
// Users interact exclusively through these up-package names.

type (
	LBContext             = down.LBContext
	LBHandle              = down.LBHandle
	LBPolicy              = down.LBPolicy
	LBPolicyConfigFactory = down.LBPolicyConfigFactory
	LBPolicyFactory       = down.LBPolicyFactory

	EmptyLBPolicy = down.EmptyLBPolicy
)

// RegisterLBPolicy registers a named LBPolicyFactory. Must be called from an
// init() function. Panics on duplicate names.
func RegisterLBPolicy(name string, f LBPolicyFactory) {
	down.RegisterLBPolicy(name, f)
}
