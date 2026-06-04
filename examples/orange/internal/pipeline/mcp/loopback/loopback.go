// Package loopback registers the orange-mcp-loopback dynamic-modules cluster.
// The cluster owns the orange-mcp sidecar lifecycle: it binds the listener
// synchronously in Init, publishes the address as its single host, and starts
// the Serve goroutine in ServerInitialized. Shutdown stops the sidecar and
// waits for the serve goroutine to exit.
package loopback

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"

	"github.com/dio/transit/examples/orange/internal/observability"
	"github.com/dio/transit/examples/orange/internal/pipeline/mcp"
	"github.com/dio/transit/up"
)

const ClusterName = "orange-mcp-loopback"

func init() {
	up.RegisterCluster(ClusterName, &factory{
		logger: observability.Logger("orange/mcp/loopback"),
	})
}

// clusterConfig is unmarshalled from the cluster_config JSON in envoy.tmpl.yaml.
// All fields are optional; omit to use env-var / compiled-in defaults.
//
//	cluster_config:
//	  "@type": type.googleapis.com/google.protobuf.StringValue
//	  value: '{"listen_addr":"unix:///tmp/orange-mcp.sock"}'
type clusterConfig struct {
	// ListenAddr overrides ORANGE_MCP_LISTEN_ADDR and the compiled-in default.
	// Supports ephemeral TCP ("127.0.0.1:0"), fixed TCP ("127.0.0.1:10004"),
	// and Unix domain sockets ("unix:///tmp/orange-mcp.sock").
	ListenAddr string `json:"listen_addr,omitempty"`
}

type factory struct{ logger *slog.Logger }

func (f *factory) Create(raw []byte) (up.ClusterConfigFactory, error) {
	var cfg clusterConfig
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &cfg); err != nil {
			return nil, fmt.Errorf("orange-mcp-loopback: parse cluster_config: %w", err)
		}
	}
	return &cfgFactory{logger: f.logger, cfg: cfg}, nil
}

type cfgFactory struct {
	logger *slog.Logger
	cfg    clusterConfig
}

func (f *cfgFactory) NewCluster(_ up.ClusterHandle) up.Cluster {
	return &cluster{logger: f.logger, cfg: f.cfg}
}

func (*cfgFactory) Close() {}

type cluster struct {
	up.BaseCluster
	logger *slog.Logger
	cfg    clusterConfig
	sc     *mcp.Sidecar
	host   up.HostPtr
	bg     up.ClusterGroup
}

func (c *cluster) Init(h up.ClusterHandle) {
	sc, err := mcp.NewSidecar(c.cfg.ListenAddr)
	if err != nil {
		c.logger.Error("orange-mcp-loopback: failed to create sidecar", "err", err)
		h.PreInitComplete()
		return
	}
	if err := sc.Listen(); err != nil {
		c.logger.Error("orange-mcp-loopback: failed to bind sidecar listener", "err", err)
		h.PreInitComplete()
		return
	}
	ptrs := h.AddHosts([]up.HostSpec{{Address: sc.ListenAddr()}})
	if len(ptrs) == 0 {
		c.logger.Error("orange-mcp-loopback: AddHosts returned no ptrs", "addr", sc.ListenAddr())
		h.PreInitComplete()
		return
	}
	h.UpdateHostHealth(ptrs[0], up.HostHealthy)
	c.sc = sc
	c.host = ptrs[0]
	h.PreInitComplete()
}

func (c *cluster) ServerInitialized(_ up.ClusterHandle) {
	if c.sc == nil {
		return
	}
	c.bg.Go(func(_ context.Context) {
		if err := c.sc.Serve(); err != nil && err != http.ErrServerClosed {
			c.logger.Error("orange-mcp-loopback: sidecar serve error", "err", err)
		}
	})
	c.bg.Start()
}

func (c *cluster) Shutdown(_ up.ClusterHandle, done func()) {
	if c.sc != nil {
		c.sc.Stop()
	}
	c.bg.Stop()
	done()
}

func (c *cluster) NewClusterLB() up.ClusterLB {
	return &lb{host: c.host}
}

type lb struct {
	up.EmptyClusterLB
	host up.HostPtr
}

func (l *lb) ChooseHost(_ up.ClusterLBHandle, _ up.ClusterLBContext) (up.HostPtr, *up.ClusterLBCompletion) {
	return l.host, nil
}
