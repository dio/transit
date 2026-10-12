package llmdecider

import (
	"encoding/json"
	"fmt"

	"github.com/dio/transit/up"
)

// modelClusterFactory is the Cluster Extension surface. The module owns the host
// set, so the model→host mapping lives here rather than in xDS.
type modelClusterFactory struct{}

type clusterConfig struct {
	Hosts []clusterHost `json:"hosts"`
}

type clusterHost struct {
	Address  string `json:"address"`
	Model    string `json:"model"`
	Hostname string `json:"hostname,omitempty"`
}

func (f *modelClusterFactory) Create(config []byte) (up.ClusterConfigFactory, error) {
	hosts, err := parseClusterHosts(config)
	if err != nil {
		return nil, err
	}
	return &modelClusterConfigFactory{hosts: hosts}, nil
}

func parseClusterHosts(config []byte) ([]clusterHost, error) {
	if len(config) == 0 {
		return nil, fmt.Errorf("llm-decider cluster: config must include at least one host")
	}
	var parsed clusterConfig
	if err := json.Unmarshal(config, &parsed); err != nil {
		return nil, fmt.Errorf("llm-decider cluster: parse config: %w", err)
	}
	if len(parsed.Hosts) == 0 {
		return nil, fmt.Errorf("llm-decider cluster: config must include at least one host")
	}
	for i, host := range parsed.Hosts {
		if host.Address == "" {
			return nil, fmt.Errorf("llm-decider cluster: host %d address is required", i)
		}
		if host.Model == "" {
			return nil, fmt.Errorf("llm-decider cluster: host %d model is required", i)
		}
	}
	return parsed.Hosts, nil
}

type modelClusterConfigFactory struct {
	hosts []clusterHost
}

func (cf *modelClusterConfigFactory) NewCluster(_ up.ClusterHandle) up.Cluster {
	return &modelCluster{hosts: cf.hosts}
}

func (cf *modelClusterConfigFactory) Close() {}

type modelCluster struct {
	hosts   []clusterHost
	byModel map[string]up.HostPtr
}

func (c *modelCluster) Init(h up.ClusterHandle) {
	specs := make([]up.HostSpec, 0, len(c.hosts))
	for _, host := range c.hosts {
		specs = append(specs, up.HostSpec{
			Address:  host.Address,
			Hostname: host.Hostname,
			Metadata: map[string]string{"model": host.Model},
		})
	}

	ptrs := h.AddHosts(specs)
	c.byModel = make(map[string]up.HostPtr, len(ptrs))
	for i, ptr := range ptrs {
		h.UpdateHostHealth(ptr, up.HostHealthy)
		c.byModel[c.hosts[i].Model] = ptr
	}

	// Bootstrap synchronously. In the Envoy Gateway path a patched cluster can
	// arrive via CDS after Envoy is already running, so ServerInitialized alone
	// would leave the cluster pending indefinitely.
	h.PreInitComplete()
}

func (c *modelCluster) NewClusterLB() up.ClusterLB           { return &modelClusterLB{parent: c} }
func (c *modelCluster) ServerInitialized(_ up.ClusterHandle) {}
func (c *modelCluster) DrainStarted(_ up.ClusterHandle)      {}
func (c *modelCluster) Shutdown(_ up.ClusterHandle, done func()) {
	done()
}
func (c *modelCluster) Close() {}

// modelClusterLB selects the host serving the decided model, which the downstream
// filter wrote onto the request. It falls back to the first healthy host when the
// decision is absent, or names no host this cluster owns.
type modelClusterLB struct {
	up.EmptyClusterLB
	parent *modelCluster
}

func (lb *modelClusterLB) ChooseHost(
	h up.ClusterLBHandle,
	ctx up.ClusterLBContext,
) (up.HostPtr, *up.ClusterLBCompletion) {
	if h.HealthyHostCount(0) == 0 {
		return nil, nil
	}
	if model := headerValue(ctx, HeaderDecidedModel); model != "" {
		if ptr, known := lb.parent.byModel[model]; known {
			return ptr, nil
		}
	}
	return h.HealthyHost(0, 0), nil
}

// headerValue reads the first value of a downstream request header.
func headerValue(ctx up.ClusterLBContext, name string) string {
	for _, header := range ctx.GetAllHeaders() {
		if header[0] == name {
			return header[1]
		}
	}
	return ""
}
