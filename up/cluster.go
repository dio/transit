package up

import (
	"fmt"

	"github.com/envoyproxy/envoy/source/extensions/dynamic_modules/sdk/go/shared"

	"github.com/dio/transit/down"
)

// Cluster Extension type aliases — defined in down to avoid import cycles.
// Users interact exclusively through these up-package names.

type (
	HostPtr  = down.HostPtr
	HostSpec = down.HostSpec

	ClusterLBCompletion = down.ClusterLBCompletion
	ClusterLBContext    = down.ClusterLBContext
	ClusterLBHandle     = down.ClusterLBHandle
	ClusterLB           = down.ClusterLB
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

// ClusterMetrics provides access to Envoy cluster metrics.
// Define metrics during ClusterFactoryWithMetrics.CreateWithMetrics on the main thread;
// record them at any time during the cluster's lifetime.
type ClusterMetrics interface {
	DefineCounter(name string, tagKeys ...string) (MetricID, error)
	DefineGauge(name string, tagKeys ...string) (MetricID, error)
	DefineHistogram(name string, tagKeys ...string) (MetricID, error)

	IncrementCounter(id MetricID, delta uint64, labelValues ...string) error
	SetGauge(id MetricID, value uint64, labelValues ...string) error
	IncrementGauge(id MetricID, delta uint64, labelValues ...string) error
	DecrementGauge(id MetricID, delta uint64, labelValues ...string) error
	RecordHistogram(id MetricID, value uint64, labelValues ...string) error
}

// ClusterFactoryWithMetrics is an optional extension of ClusterFactory.
// When implemented, CreateWithMetrics is called instead of Create, providing
// a ClusterMetrics handle for defining Envoy metrics at config-load time.
type ClusterFactoryWithMetrics interface {
	CreateWithMetrics(metrics ClusterMetrics, config []byte) (ClusterConfigFactory, error)
}

// RegisterCluster registers a named ClusterFactory. Must be called from an
// init() function. Panics on duplicate names.
func RegisterCluster(name string, f ClusterFactory) {
	if fwm, ok := f.(ClusterFactoryWithMetrics); ok {
		down.RegisterCluster(name, &upClusterFactoryWithMetricsAdapter{factory: f, withMetrics: fwm})
		return
	}
	down.RegisterCluster(name, f)
}

// upClusterFactoryWithMetricsAdapter bridges an up.ClusterFactoryWithMetrics to
// the down layer, implementing both down.ClusterFactory and down.ClusterFactoryWithMetrics.
type upClusterFactoryWithMetricsAdapter struct {
	factory     ClusterFactory
	withMetrics ClusterFactoryWithMetrics
}

func (a *upClusterFactoryWithMetricsAdapter) Create(config []byte) (down.ClusterConfigFactory, error) {
	return a.factory.Create(config)
}

func (a *upClusterFactoryWithMetricsAdapter) CreateWithMetrics(metrics down.ClusterMetricsHandle, config []byte) (down.ClusterConfigFactory, error) {
	return a.withMetrics.CreateWithMetrics(&clusterMetricsAdapter{handle: metrics}, config)
}

// clusterMetricsAdapter wraps down.ClusterMetricsHandle to implement ClusterMetrics.
type clusterMetricsAdapter struct {
	handle down.ClusterMetricsHandle
}

func (a *clusterMetricsAdapter) DefineCounter(name string, tagKeys ...string) (MetricID, error) {
	id, res := a.handle.DefineCounter(name, tagKeys...)
	if res != shared.MetricsSuccess {
		return 0, fmt.Errorf("up: DefineCounter %q failed (result=%d)", name, res)
	}
	return MetricID(id), nil
}

func (a *clusterMetricsAdapter) DefineGauge(name string, tagKeys ...string) (MetricID, error) {
	id, res := a.handle.DefineGauge(name, tagKeys...)
	if res != shared.MetricsSuccess {
		return 0, fmt.Errorf("up: DefineGauge %q failed (result=%d)", name, res)
	}
	return MetricID(id), nil
}

func (a *clusterMetricsAdapter) DefineHistogram(name string, tagKeys ...string) (MetricID, error) {
	id, res := a.handle.DefineHistogram(name, tagKeys...)
	if res != shared.MetricsSuccess {
		return 0, fmt.Errorf("up: DefineHistogram %q failed (result=%d)", name, res)
	}
	return MetricID(id), nil
}

func (a *clusterMetricsAdapter) IncrementCounter(id MetricID, delta uint64, labelValues ...string) error {
	res := a.handle.IncrementCounterValue(shared.MetricID(id), delta, labelValues...)
	if res != shared.MetricsSuccess {
		return fmt.Errorf("up: IncrementCounter id=%d failed (result=%d)", id, res)
	}
	return nil
}

func (a *clusterMetricsAdapter) SetGauge(id MetricID, value uint64, labelValues ...string) error {
	res := a.handle.SetGaugeValue(shared.MetricID(id), value, labelValues...)
	if res != shared.MetricsSuccess {
		return fmt.Errorf("up: SetGauge id=%d failed (result=%d)", id, res)
	}
	return nil
}

func (a *clusterMetricsAdapter) IncrementGauge(id MetricID, delta uint64, labelValues ...string) error {
	res := a.handle.IncrementGaugeValue(shared.MetricID(id), delta, labelValues...)
	if res != shared.MetricsSuccess {
		return fmt.Errorf("up: IncrementGauge id=%d failed (result=%d)", id, res)
	}
	return nil
}

func (a *clusterMetricsAdapter) DecrementGauge(id MetricID, delta uint64, labelValues ...string) error {
	res := a.handle.DecrementGaugeValue(shared.MetricID(id), delta, labelValues...)
	if res != shared.MetricsSuccess {
		return fmt.Errorf("up: DecrementGauge id=%d failed (result=%d)", id, res)
	}
	return nil
}

func (a *clusterMetricsAdapter) RecordHistogram(id MetricID, value uint64, labelValues ...string) error {
	res := a.handle.RecordHistogramValue(shared.MetricID(id), value, labelValues...)
	if res != shared.MetricsSuccess {
		return fmt.Errorf("up: RecordHistogram id=%d failed (result=%d)", id, res)
	}
	return nil
}
