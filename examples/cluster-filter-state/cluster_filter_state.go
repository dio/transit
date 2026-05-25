// Package clusterfilterstate demonstrates passing filter state from an HTTP
// filter to a Cluster Extension. The HTTP filter reads the "x-target-host"
// request header and writes it as the "transit.target_host" filter state key.
// The cluster extension reads that key in ChooseHost and selects the matching
// upstream host, falling back to the first healthy host when no match is found.
package clusterfilterstate

import (
	"encoding/json"
	"fmt"

	"github.com/dio/transit/up"
)

func init() {
	up.Register("filter-state-writer", handler)
	up.RegisterCluster("filter-state-router", &routerFactory{})
}

// handler is the HTTP filter: reads "x-target-host" and stores it as filter state.
func handler(w *up.Writer, r *up.Request) {
	target := r.Header("x-target-host")
	if target != "" {
		w.SetFilterState("transit.target_host", target)
	}
}

// =============================================================================
// Cluster extension
// =============================================================================

type clusterConfig struct {
	Hosts []hostConfig `json:"hosts"`
}

type hostConfig struct {
	Address string `json:"address"`
	Weight  uint32 `json:"weight,omitempty"`
}

type routerFactory struct{}

func (f *routerFactory) Create(config []byte) (up.ClusterConfigFactory, error) {
	hosts, err := parseHosts(config)
	if err != nil {
		return nil, err
	}
	return &routerConfigFactory{hosts: hosts}, nil
}

func parseHosts(config []byte) ([]up.HostSpec, error) {
	if len(config) == 0 {
		return nil, fmt.Errorf("cluster-filter-state: config must include at least one host")
	}
	var parsed clusterConfig
	if err := json.Unmarshal(config, &parsed); err != nil {
		return nil, fmt.Errorf("cluster-filter-state: parse config: %w", err)
	}
	if len(parsed.Hosts) == 0 {
		return nil, fmt.Errorf("cluster-filter-state: config must include at least one host")
	}
	hosts := make([]up.HostSpec, 0, len(parsed.Hosts))
	for i, h := range parsed.Hosts {
		if h.Address == "" {
			return nil, fmt.Errorf("cluster-filter-state: host %d address is required", i)
		}
		hosts = append(hosts, up.HostSpec{Address: h.Address, Weight: h.Weight})
	}
	return hosts, nil
}

type routerConfigFactory struct {
	hosts []up.HostSpec
}

func (cf *routerConfigFactory) NewCluster(_ up.ClusterHandle) up.Cluster {
	return &routerCluster{hosts: cf.hosts}
}

func (cf *routerConfigFactory) Close() {}

type routerCluster struct {
	hosts []up.HostSpec
	ptrs  []up.HostPtr
}

func (c *routerCluster) Init(h up.ClusterHandle) {
	c.ptrs = h.AddHosts(c.hosts)
	for _, ptr := range c.ptrs {
		h.UpdateHostHealth(ptr, up.HostHealthy)
	}
	h.PreInitComplete()
}

func (c *routerCluster) NewClusterLB() up.ClusterLB {
	// Pass both the host specs (for address matching) and the ptrs slice.
	// ptrs is populated by Init before any worker calls NewClusterLB.
	return &filterStateRouterLB{
		hosts: c.hosts,
		ptrs:  c.ptrs,
	}
}

func (c *routerCluster) ServerInitialized(_ up.ClusterHandle) {}
func (c *routerCluster) DrainStarted(_ up.ClusterHandle)      {}
func (c *routerCluster) Shutdown(_ up.ClusterHandle, done func()) {
	done()
}
func (c *routerCluster) Close() {}

type filterStateRouterLB struct {
	up.EmptyClusterLB
	hosts []up.HostSpec
	ptrs  []up.HostPtr
}

func (lb *filterStateRouterLB) ChooseHost(h up.ClusterLBHandle, ctx up.ClusterLBContext) (up.HostPtr, *up.ClusterLBCompletion) {
	if ctx != nil {
		if target, ok := ctx.GetFilterState("transit.target_host"); ok && target != "" {
			// Try to match by address in our known host list.
			for i, spec := range lb.hosts {
				if spec.Address == target && i < len(lb.ptrs) {
					return lb.ptrs[i], nil
				}
			}
			// Address not in our static list — try the handle's lookup.
			if ptr := h.FindHostByAddress(target); ptr != nil {
				return ptr, nil
			}
		}
	}
	// Fall back to the first healthy host.
	if h.HealthyHostCount(0) == 0 {
		return nil, nil
	}
	return h.HealthyHost(0, 0), nil
}
