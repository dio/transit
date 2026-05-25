// Package lbpolicyretryaware demonstrates the LB Policy extension point with a
// retry-aware selection strategy.
//
// Policy: "retry-aware" — on retry attempts, skip hosts that Envoy has already
// tried (ctx.ShouldSelectAnotherHost returns true for those). Scans healthy
// hosts in order and picks the first one where ShouldSelectAnotherHost returns
// false. If all hosts have been tried (or there are no hosts), falls back to
// index 0 to avoid a hard 503 when all hosts have been tried.
//
// To build the shared library:
//
//	CGO_ENABLED=1 go build -trimpath -buildmode=c-shared \
//	  -o liblb-policy-retry-aware.so ./lb-policy-retry-aware/cmd
//
// (run from the examples/ directory)
package lbpolicyretryaware

import "github.com/dio/transit/up"

func init() {
	up.RegisterLBPolicy("retry-aware", &retryAwareFactory{})
}

// retryAwareFactory parses config (none required) and creates per-cluster
// config factories.
type retryAwareFactory struct{}

func (f *retryAwareFactory) Create(_ []byte) (up.LBPolicyConfigFactory, error) {
	return &retryAwareConfigFactory{}, nil
}

// retryAwareConfigFactory creates per-worker-thread LB instances.
type retryAwareConfigFactory struct{}

func (cf *retryAwareConfigFactory) NewLBPolicy() up.LBPolicy { return &retryAwarePolicy{} }
func (cf *retryAwareConfigFactory) Close()                   {}

// retryAwarePolicy scans healthy hosts and skips any that Envoy has already
// tried. Falls back to index 0 when all hosts have been tried.
type retryAwarePolicy struct{ up.EmptyLBPolicy }

func (p *retryAwarePolicy) ChooseHost(lb up.LBHandle, ctx up.LBContext, priority *uint32, index *uint32) bool {
	n := lb.HealthyHostCount(0)
	if n == 0 {
		return false
	}
	*priority = 0
	for i := 0; i < n; i++ {
		*index = uint32(i)
		if !ctx.ShouldSelectAnotherHost(lb, 0, i) {
			return true
		}
	}
	// all tried — fall back to 0
	*index = 0
	return true
}
