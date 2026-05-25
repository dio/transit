package down

import (
	"sync"
)

// LBContext provides per-request information inside LBPolicy.ChooseHost.
// Note: filter-state and downstream-SNI are unavailable in LB Policy context;
// use the Cluster Extension (ClusterLBContext) if those are needed.
type LBContext interface {
	GetAllHeaders() [][2]string
	GetOverrideHost() (addr string, strict bool)
	GetHeader(name string) (string, bool)
	ComputeHashKey() (uint64, bool)
	GetHostSelectionRetryCount() uint32
	ShouldSelectAnotherHost(lb LBHandle, priority uint32, index int) bool
}

// LBHandle gives an LBPolicy access to the cluster's host set.
type LBHandle interface {
	ClusterName() string
	PriorityCount() int
	HostCount(priority uint32) int
	HealthyHostCount(priority uint32) int
	DegradedHostCount(priority uint32) int
	HostAddress(priority uint32, index int) (string, bool)
	HealthyHostAddress(priority uint32, index int) (string, bool)
	HostWeight(priority uint32, index int) uint32
	HealthyHostWeight(priority uint32, index int) uint32
	HostHealth(priority uint32, index int) HostHealth
	HostHealthByAddress(addr string) (HostHealth, bool)
	HostStat(priority uint32, index int, stat HostStat) uint64
	MemberUpdateHostAddress(index int, isAdded bool) (string, bool)
	HostLocality(priority uint32, index int) (region, zone, subZone string, ok bool)
	SetHostData(priority uint32, index int, data uintptr) bool
	GetHostData(priority uint32, index int) (uintptr, bool)
	HostMetadataString(priority uint32, index int, filterName, key string) (string, bool)
	HostMetadataNumber(priority uint32, index int, filterName, key string) (float64, bool)
	HostMetadataBool(priority uint32, index int, filterName, key string) (bool, bool)
	LocalityCount(priority uint32) int
	LocalityHostCount(priority uint32, localityIndex int) int
	LocalityHostAddress(priority uint32, localityIndex, hostIndex int) (string, bool)
	LocalityWeight(priority uint32, localityIndex int) uint32
}

// LBPolicy is the per-worker-thread load balancer for the LB Policy extension.
type LBPolicy interface {
	// ChooseHost selects a host. Write result priority and index into the
	// output params and return true, or return false for no host (→ 503).
	ChooseHost(lb LBHandle, ctx LBContext, priority *uint32, index *uint32) bool

	// OnHostMembershipUpdate is called when the host set changes.
	OnHostMembershipUpdate(lb LBHandle, numAdded, numRemoved int)

	// Close is called when this per-worker instance is destroyed.
	Close()
}

// LBPolicyConfigFactory is created once per LB policy config (main thread).
type LBPolicyConfigFactory interface {
	NewLBPolicy() LBPolicy
	Close()
}

// LBPolicyFactory parses config and produces LBPolicyConfigFactory instances.
type LBPolicyFactory interface {
	Create(config []byte) (LBPolicyConfigFactory, error)
}

// EmptyLBPolicy embeds into LBPolicy implementations to no-op optional methods.
type EmptyLBPolicy struct{}

func (EmptyLBPolicy) OnHostMembershipUpdate(_ LBHandle, _, _ int) {}
func (EmptyLBPolicy) Close()                                      {}

// =============================================================================
// LB Policy registry
// =============================================================================

var (
	lbPolicyMu       sync.RWMutex
	lbPolicyRegistry = map[string]LBPolicyFactory{}
)

// RegisterLBPolicy registers a named LBPolicyFactory.
// Must be called from an init() function. Panics on duplicate names.
func RegisterLBPolicy(name string, f LBPolicyFactory) {
	lbPolicyMu.Lock()
	defer lbPolicyMu.Unlock()
	if _, ok := lbPolicyRegistry[name]; ok {
		panic("down: LB policy already registered: " + name)
	}
	lbPolicyRegistry[name] = f
}

// GetLBPolicyFactory returns the factory for the given name, or nil.
func GetLBPolicyFactory(name string) LBPolicyFactory {
	lbPolicyMu.RLock()
	defer lbPolicyMu.RUnlock()
	return lbPolicyRegistry[name]
}
