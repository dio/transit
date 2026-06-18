package filters

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"sync/atomic"
	"time"

	"github.com/dio/transit/up"
)

func init() {
	up.RegisterCluster("static-hosts", &clusterStaticHostsFactory{})
	up.RegisterCluster("scheduler-probe", &clusterSchedulerProbeFactory{})
	up.Register("e2e-cluster-scheduler-state", clusterSchedulerState)
}

type clusterStaticHostsFactory struct{}

func (f *clusterStaticHostsFactory) Create(config []byte) (up.ClusterConfigFactory, error) {
	var parsed struct {
		Hosts []struct {
			Address  string `json:"address"`
			Hostname string `json:"hostname,omitempty"`
			Weight   uint32 `json:"weight,omitempty"`
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
		hosts = append(hosts, up.HostSpec{
			Address:  resolveClusterHostAddr(host.Address),
			Hostname: host.Hostname,
			Weight:   host.Weight,
		})
	}
	return &clusterStaticHostsConfigFactory{hosts: hosts}, nil
}

func resolveClusterHostAddr(addr string) string {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return addr
	}
	if net.ParseIP(host) != nil {
		return addr
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	ips, err := net.DefaultResolver.LookupHost(ctx, host)
	if err != nil || len(ips) == 0 {
		return addr
	}
	return net.JoinHostPort(ips[0], port)
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

func (c *clusterStaticHostsCluster) ServerInitialized(h up.ClusterHandle) {
	scheduleClusterProbe(h)
}
func (c *clusterStaticHostsCluster) DrainStarted(_ up.ClusterHandle) {}
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

var (
	clusterSchedulerCommitted atomic.Int64
	clusterSchedulerRan       atomic.Int64
)

type clusterSchedulerProbeFactory struct{}

func (f *clusterSchedulerProbeFactory) Create(_ []byte) (up.ClusterConfigFactory, error) {
	return &clusterSchedulerProbeConfigFactory{}, nil
}

type clusterSchedulerProbeConfigFactory struct{}

func (cf *clusterSchedulerProbeConfigFactory) NewCluster(_ up.ClusterHandle) up.Cluster {
	return &clusterSchedulerProbeCluster{}
}

func (cf *clusterSchedulerProbeConfigFactory) Close() {}

type clusterSchedulerProbeCluster struct{}

func (c *clusterSchedulerProbeCluster) Init(h up.ClusterHandle) {
	h.PreInitComplete()
}

func (c *clusterSchedulerProbeCluster) NewClusterLB() up.ClusterLB {
	return &clusterSchedulerProbeLB{}
}

func (c *clusterSchedulerProbeCluster) ServerInitialized(h up.ClusterHandle) {
	scheduleClusterProbe(h)
}

func scheduleClusterProbe(h up.ClusterHandle) {
	// Schedule from a goroutine so the test covers the real background-worker
	// path used by config refresh code, not only same-thread scheduling.
	go func() {
		clusterSchedulerCommitted.Add(1)
		h.Schedule(func() {
			clusterSchedulerRan.Add(1)
		})
	}()
}

func (c *clusterSchedulerProbeCluster) DrainStarted(_ up.ClusterHandle) {}
func (c *clusterSchedulerProbeCluster) Shutdown(_ up.ClusterHandle, done func()) {
	done()
}
func (c *clusterSchedulerProbeCluster) Close() {}

type clusterSchedulerProbeLB struct{ up.EmptyClusterLB }

func (lb *clusterSchedulerProbeLB) ChooseHost(_ up.ClusterLBHandle, _ up.ClusterLBContext) (up.HostPtr, *up.ClusterLBCompletion) {
	return nil, nil
}

func clusterSchedulerState(w *up.Writer, _ *up.Request) {
	body := fmt.Appendf(nil, "committed=%d ran=%d", clusterSchedulerCommitted.Load(), clusterSchedulerRan.Load())
	w.SendLocalResponse(200, body, [2]string{"content-type", "text/plain"})
}
