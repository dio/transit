package filters

import (
	"encoding/json"
	"fmt"

	"github.com/dio/transit/up"
)

func init() {
	up.RegisterCluster("static-hosts", &clusterStaticHostsFactory{})
}

type clusterStaticHostsFactory struct{}

func (f *clusterStaticHostsFactory) Create(config []byte) (up.ClusterConfigFactory, error) {
	var parsed struct {
		Hosts []struct {
			Address string `json:"address"`
			Weight  uint32 `json:"weight,omitempty"`
		} `json:"hosts"`
	}
	if err := json.Unmarshal(config, &parsed); err != nil {
		return nil, fmt.Errorf("cluster e2e: parse config: %w", err)
	}
	if len(parsed.Hosts) == 0 {
		return nil, fmt.Errorf("cluster e2e: config must include at least one host")
	}

	hosts := make([]up.HostSpec, 0, len(parsed.Hosts))
	for i, host := range parsed.Hosts {
		if host.Address == "" {
			return nil, fmt.Errorf("cluster e2e: host %d address is required", i)
		}
		hosts = append(hosts, up.HostSpec{Address: host.Address, Weight: host.Weight})
	}
	return &clusterStaticHostsConfigFactory{hosts: hosts}, nil
}

type clusterStaticHostsConfigFactory struct {
	hosts []up.HostSpec
}

func (cf *clusterStaticHostsConfigFactory) NewCluster(_ up.ClusterHandle) up.Cluster {
	return &clusterStaticHostsCluster{hosts: cf.hosts}
}

func (cf *clusterStaticHostsConfigFactory) Close() {}

type clusterStaticHostsCluster struct {
	hosts []up.HostSpec
	ptrs  []up.HostPtr
}

func (c *clusterStaticHostsCluster) Init(h up.ClusterHandle) {
	c.ptrs = h.AddHosts(c.hosts)
	for _, ptr := range c.ptrs {
		h.UpdateHostHealth(ptr, up.HostHealthy)
	}
	h.PreInitComplete()
}

func (c *clusterStaticHostsCluster) NewClusterLB() up.ClusterLB {
	return &clusterFirstHealthyLB{}
}

func (c *clusterStaticHostsCluster) ServerInitialized(_ up.ClusterHandle) {}
func (c *clusterStaticHostsCluster) DrainStarted(_ up.ClusterHandle)      {}
func (c *clusterStaticHostsCluster) Shutdown(_ up.ClusterHandle, done func()) {
	done()
}
func (c *clusterStaticHostsCluster) Close() {}

type clusterFirstHealthyLB struct{ up.EmptyClusterLB }

func (lb *clusterFirstHealthyLB) ChooseHost(h up.ClusterLBHandle, _ up.ClusterLBContext) (up.HostPtr, *up.ClusterLBCompletion) {
	if h.HealthyHostCount(0) == 0 {
		return nil, nil
	}
	return h.HealthyHost(0, 0), nil
}
