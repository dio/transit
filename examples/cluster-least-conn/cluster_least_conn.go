// Package clusterleastconn demonstrates the Cluster Extension point with a
// least-connections load balancer. The module owns the host set (parsed from
// JSON config identical to the "cluster" example) and picks the host with the
// fewest active requests across ALL hosts.
package clusterleastconn

import (
	"encoding/json"
	"fmt"

	"github.com/dio/transit/up"
)

func init() {
	up.RegisterCluster("least-conn", &leastConnFactory{})
}

type clusterConfig struct {
	Hosts []hostConfig `json:"hosts"`
}

type hostConfig struct {
	Address string `json:"address"`
	Weight  uint32 `json:"weight,omitempty"`
}

type leastConnFactory struct{}

func (f *leastConnFactory) Create(config []byte) (up.ClusterConfigFactory, error) {
	hosts, err := parseHosts(config)
	if err != nil {
		return nil, err
	}
	return &leastConnConfigFactory{hosts: hosts}, nil
}

func parseHosts(config []byte) ([]up.HostSpec, error) {
	var parsed clusterConfig
	if len(config) == 0 {
		return nil, fmt.Errorf("cluster-least-conn: config must include at least one host")
	}
	if err := json.Unmarshal(config, &parsed); err != nil {
		return nil, fmt.Errorf("cluster-least-conn: parse config: %w", err)
	}
	if len(parsed.Hosts) == 0 {
		return nil, fmt.Errorf("cluster-least-conn: config must include at least one host")
	}

	hosts := make([]up.HostSpec, 0, len(parsed.Hosts))
	for i, host := range parsed.Hosts {
		if host.Address == "" {
			return nil, fmt.Errorf("cluster-least-conn: host %d address is required", i)
		}
		hosts = append(hosts, up.HostSpec{
			Address: host.Address,
			Weight:  host.Weight,
		})
	}
	return hosts, nil
}

type leastConnConfigFactory struct {
	hosts []up.HostSpec
}

func (cf *leastConnConfigFactory) NewCluster(_ up.ClusterHandle) up.Cluster {
	return &leastConnCluster{hosts: cf.hosts}
}

func (cf *leastConnConfigFactory) Close() {}

type leastConnCluster struct {
	hosts []up.HostSpec
	ptrs  []up.HostPtr
}

func (c *leastConnCluster) Init(h up.ClusterHandle) {
	c.ptrs = h.AddHosts(c.hosts)
	for _, ptr := range c.ptrs {
		h.UpdateHostHealth(ptr, up.HostHealthy)
	}
	h.PreInitComplete()
}

func (c *leastConnCluster) NewClusterLB() up.ClusterLB {
	return &leastConnClusterLB{}
}

func (c *leastConnCluster) ServerInitialized(_ up.ClusterHandle) {}
func (c *leastConnCluster) DrainStarted(_ up.ClusterHandle)      {}
func (c *leastConnCluster) Shutdown(_ up.ClusterHandle, done func()) {
	done()
}
func (c *leastConnCluster) Close() {}

type leastConnClusterLB struct{ up.EmptyClusterLB }

// ChooseHost scans all hosts (all-hosts index, priority 0) and returns the one
// with the fewest active requests. Returns nil, nil when there are no hosts.
func (lb *leastConnClusterLB) ChooseHost(h up.ClusterLBHandle, _ up.ClusterLBContext) (up.HostPtr, *up.ClusterLBCompletion) {
	total := h.HostCount(0)
	if total == 0 {
		return nil, nil
	}

	minIdx := 0
	minActive := h.HostStat(0, 0, up.HostStatRqActive)
	for i := 1; i < total; i++ {
		active := h.HostStat(0, i, up.HostStatRqActive)
		if active < minActive {
			minActive = active
			minIdx = i
		}
	}
	return h.Host(0, minIdx), nil
}
