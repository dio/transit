package metadata

import (
	"encoding/json"
	"fmt"

	"github.com/dio/transit/up"
)

func init() {
	up.RegisterCluster("metadata-hosts", &metadataHostsFactory{})
}

// ── config ────────────────────────────────────────────────────────────────────

type clusterConfig struct {
	Hosts []hostConfig `json:"hosts"`
}

type hostConfig struct {
	Address string `json:"address"`
	Weight  uint32 `json:"weight,omitempty"`
}

func parseHosts(config []byte) ([]up.HostSpec, error) {
	if len(config) == 0 {
		return nil, fmt.Errorf("metadata-hosts: config must include at least one host")
	}
	var parsed clusterConfig
	if err := json.Unmarshal(config, &parsed); err != nil {
		return nil, fmt.Errorf("metadata-hosts: parse config: %w", err)
	}
	if len(parsed.Hosts) < 2 {
		return nil, fmt.Errorf("metadata-hosts: config must include at least two hosts (standard, premium)")
	}
	hosts := make([]up.HostSpec, 0, len(parsed.Hosts))
	for i, h := range parsed.Hosts {
		if h.Address == "" {
			return nil, fmt.Errorf("metadata-hosts: host %d address is required", i)
		}
		hosts = append(hosts, up.HostSpec{Address: h.Address, Weight: h.Weight})
	}
	return hosts, nil
}

// ── factory ───────────────────────────────────────────────────────────────────

type metadataHostsFactory struct{}

func (f *metadataHostsFactory) Create(config []byte) (up.ClusterConfigFactory, error) {
	hosts, err := parseHosts(config)
	if err != nil {
		return nil, err
	}
	return &metadataHostsConfigFactory{hosts: hosts}, nil
}

type metadataHostsConfigFactory struct {
	hosts []up.HostSpec
}

func (cf *metadataHostsConfigFactory) NewCluster(_ up.ClusterHandle) up.Cluster {
	return &metadataHostsCluster{hosts: cf.hosts}
}

func (cf *metadataHostsConfigFactory) Close() {}

// ── cluster ───────────────────────────────────────────────────────────────────

type metadataHostsCluster struct {
	hosts []up.HostSpec
	ptrs  []up.HostPtr
}

func (c *metadataHostsCluster) Init(h up.ClusterHandle) {
	c.ptrs = h.AddHosts(c.hosts)
	for _, ptr := range c.ptrs {
		h.UpdateHostHealth(ptr, up.HostHealthy)
	}
	h.PreInitComplete()
}

func (c *metadataHostsCluster) NewClusterLB() up.ClusterLB {
	return &metadataLB{}
}

func (c *metadataHostsCluster) ServerInitialized(_ up.ClusterHandle) {}
func (c *metadataHostsCluster) DrainStarted(_ up.ClusterHandle)      {}
func (c *metadataHostsCluster) Shutdown(_ up.ClusterHandle, done func()) {
	done()
}
func (c *metadataHostsCluster) Close() {}

// ── LB ────────────────────────────────────────────────────────────────────────

type metadataLB struct{ up.EmptyClusterLB }

func (lb *metadataLB) ChooseHost(h up.ClusterLBHandle, ctx up.ClusterLBContext) (up.HostPtr, *up.ClusterLBCompletion) {
	idx := resolveTierIndex(tierFromCtx(ctx))
	count := h.HostCount(0)
	if count == 0 {
		return nil, nil
	}
	if idx >= count {
		idx = 0
	}
	return h.Host(0, idx), nil
}

func tierFromCtx(ctx up.ClusterLBContext) string {
	if tier, ok := ctx.GetFilterState(fsKey); ok {
		return tier
	}
	return ""
}

// resolveTierIndex maps a tier string to a host index.
// Index 0 = standard host, index 1 = premium host.
func resolveTierIndex(tier string) int {
	if tier == "premium" {
		return 1
	}
	return 0
}
