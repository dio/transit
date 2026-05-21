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
