// Package cluster demonstrates the Cluster Extension point. The module owns the
// host set, completes cluster initialization, and returns a ClusterLB that
// selects the first healthy host.
package cluster

import (
	"encoding/json"
	"fmt"

	"github.com/dio/transit/up"
)

func init() {
	up.RegisterCluster("static-hosts", &staticHostsFactory{})
}

type clusterConfig struct {
	Hosts []hostConfig `json:"hosts"`
}

type hostConfig struct {
	Address string `json:"address"`
	Weight  uint32 `json:"weight,omitempty"`
}

type staticHostsFactory struct{}

func (f *staticHostsFactory) Create(config []byte) (up.ClusterConfigFactory, error) {
	hosts, err := parseHosts(config)
	if err != nil {
		return nil, err
	}
	return &staticHostsConfigFactory{hosts: hosts}, nil
}

func parseHosts(config []byte) ([]up.HostSpec, error) {
	var parsed clusterConfig
	if len(config) == 0 {
		return nil, fmt.Errorf("cluster: config must include at least one host")
	}
	if err := json.Unmarshal(config, &parsed); err != nil {
		return nil, fmt.Errorf("cluster: parse config: %w", err)
	}
	if len(parsed.Hosts) == 0 {
		return nil, fmt.Errorf("cluster: config must include at least one host")
	}

	hosts := make([]up.HostSpec, 0, len(parsed.Hosts))
	for i, host := range parsed.Hosts {
		if host.Address == "" {
			return nil, fmt.Errorf("cluster: host %d address is required", i)
		}
		hosts = append(hosts, up.HostSpec{
			Address: host.Address,
			Weight:  host.Weight,
		})
	}
	return hosts, nil
}

type staticHostsConfigFactory struct {
	hosts []up.HostSpec
}

func (cf *staticHostsConfigFactory) NewCluster(_ up.ClusterHandle) up.Cluster {
	return &staticHostsCluster{hosts: cf.hosts}
}

func (cf *staticHostsConfigFactory) Close() {}

type staticHostsCluster struct {
	hosts []up.HostSpec
	ptrs  []up.HostPtr
}

func (c *staticHostsCluster) Init(h up.ClusterHandle) {
	c.ptrs = h.AddHosts(c.hosts)
	for _, ptr := range c.ptrs {
		h.UpdateHostHealth(ptr, up.HostHealthy)
	}
	h.PreInitComplete()
}

func (c *staticHostsCluster) NewClusterLB() up.ClusterLB {
	return &firstHealthyClusterLB{}
}

func (c *staticHostsCluster) ServerInitialized(_ up.ClusterHandle) {}
func (c *staticHostsCluster) DrainStarted(_ up.ClusterHandle)      {}
func (c *staticHostsCluster) Shutdown(_ up.ClusterHandle, done func()) {
	done()
}
func (c *staticHostsCluster) Close() {}

type firstHealthyClusterLB struct{ up.EmptyClusterLB }

func (lb *firstHealthyClusterLB) ChooseHost(h up.ClusterLBHandle, _ up.ClusterLBContext) (up.HostPtr, *up.ClusterLBCompletion) {
	if h.HealthyHostCount(0) == 0 {
		return nil, nil
	}
	return h.HealthyHost(0, 0), nil
}
