package up

import "github.com/dio/transit/down"

// Cluster Extension type aliases — defined in down to avoid import cycles.
// Users interact exclusively through these up-package names.

type (
	HostPtr  = down.HostPtr
	HostSpec = down.HostSpec

	ClusterLBCompletion  = down.ClusterLBCompletion
	ClusterLBContext     = down.ClusterLBContext
	ClusterLBHandle      = down.ClusterLBHandle
	ClusterLB            = down.ClusterLB
	// ClusterHandle gives a Cluster access to Envoy cluster operations.
	// All methods except Schedule must be called on the main thread —
	// AddHosts, RemoveHosts, UpdateHostHealth, FindHostByAddress, and
	// PreInitComplete will silently no-op (and log an envoy_bug error) if
	// invoked from a background goroutine. Use Schedule to marshal mutations
	// back to the main thread from refresh loops or async callbacks.
	// See .agents/skills/transit-cluster-main-thread/SKILL.md for the pattern.
	ClusterHandle        = down.ClusterHandle
	Cluster              = down.Cluster
	ClusterConfigFactory = down.ClusterConfigFactory
	ClusterFactory       = down.ClusterFactory

	EmptyClusterLB = down.EmptyClusterLB
)

// HostHealth and HostStat are shared with LB Policy; defined here for convenience.
type (
	HostHealth = down.HostHealth
	HostStat   = down.HostStat
)

const (
	HostUnhealthy = down.HostUnhealthy
	HostDegraded  = down.HostDegraded
	HostHealthy   = down.HostHealthy
)

const (
	HostStatCxConnectFail = down.HostStatCxConnectFail
	HostStatCxTotal       = down.HostStatCxTotal
	HostStatRqError       = down.HostStatRqError
	HostStatRqSuccess     = down.HostStatRqSuccess
	HostStatRqTimeout     = down.HostStatRqTimeout
	HostStatRqTotal       = down.HostStatRqTotal
	HostStatCxActive      = down.HostStatCxActive
	HostStatRqActive      = down.HostStatRqActive
)

// RegisterCluster registers a named ClusterFactory. Must be called from an
// init() function. Panics on duplicate names.
func RegisterCluster(name string, f ClusterFactory) {
	down.RegisterCluster(name, f)
}
