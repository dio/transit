// Package hostpick is the cluster extension that picks an upstream host for
// each request.
//
// On Init it resolves every upstream listed in orange.yaml, registers each
// host with the cluster, and remembers the HostPtr by upstream name.
//
// ChooseHost reads the *pending.Pending classify stored in the per-stream
// object bag (via ClusterLBContext.GetStreamObject with key
// classify.StreamObjectKey), and returns an async ClusterLBCompletion. It
// registers a callback via pending.Pending.OnResolve; when classify resolves
// the Pending (from bodyHandler after parsing the OpenAI `model` field, or
// from onStreamComplete on stream teardown), the callback hops back to the
// cluster's main thread via handle.Schedule and calls completion.Complete.
// No per-request goroutine is parked.
//
// See .agents/skills/transit-body-driven-cluster-routing/SKILL.md for the
// rationale (header iteration finishes across all filters before the body
// arrives, so synchronous filter-state writes from a body callback are too
// late to influence ChooseHost).
package hostpick

import (
	"context"
	"fmt"
	"net"
	"net/url"
	"sync"
	"sync/atomic"
	"time"

	"github.com/dio/transit/examples/orange/classify"
	orangecfg "github.com/dio/transit/examples/orange/config"
	"github.com/dio/transit/up"
)


const (
	ClusterName = "orange-hostpick"

	defaultResolveTimeout = 1 * time.Second
)

func init() {
	up.RegisterCluster(ClusterName, &factory{})
}

type factory struct{}

func (factory) Create(_ []byte) (up.ClusterConfigFactory, error) { return &cfgFactory{}, nil }

type cfgFactory struct{}

func (cfgFactory) NewCluster(h up.ClusterHandle) up.Cluster { return &cluster{handle: h} }
func (cfgFactory) Close()                                   {}

type cluster struct {
	handle up.ClusterHandle

	// hosts is the source of truth ChooseHost reads from after Init. Published
	// once as a whole map via atomic to keep ChooseHost lock-free.
	hosts atomic.Pointer[map[string]up.HostPtr]
}

func (c *cluster) Init(h up.ClusterHandle) {
	c.handle = h
	cfg := orangecfg.Get()

	out := make(map[string]up.HostPtr, len(cfg.Providers))
	ctx, cancel := context.WithTimeout(context.Background(), defaultResolveTimeout)
	defer cancel()
	for name, p := range cfg.Providers {
		addr, err := resolveUpstream(ctx, p.Endpoint)
		if err != nil {
			continue
		}
		// sni metadata lets the cluster's transport_socket_matches pick a per-host
		// UpstreamTlsContext (one per provider hostname). Without this, every host
		// in the cluster would share a single TLS context and we'd be back to the
		// hardcoded-sni demo limitation.
		spec := up.HostSpec{Address: addr}
		if host := p.Host(); host != "" {
			spec.Metadata = map[string]string{"sni": host}
		}
		ptrs := h.AddHosts([]up.HostSpec{spec})
		if len(ptrs) == 0 {
			continue
		}
		h.UpdateHostHealth(ptrs[0], up.HostHealthy)
		out[name] = ptrs[0]
	}
	c.hosts.Store(&out)
	h.PreInitComplete()
}

func (c *cluster) ServerInitialized(_ up.ClusterHandle) {}
func (c *cluster) NewClusterLB() up.ClusterLB {
	return &lb{owner: c, cancelled: make(map[*up.ClusterLBCompletion]struct{})}
}
func (c *cluster) DrainStarted(_ up.ClusterHandle)          {}
func (c *cluster) Shutdown(_ up.ClusterHandle, done func()) { done() }
func (c *cluster) Close()                                   {}

type lb struct {
	up.EmptyClusterLB
	owner *cluster

	// cancelled tracks ChooseHost completions Envoy cancelled before they
	// completed. The scheduled completion callback consults this map on the
	// cluster main thread and skips Complete when the entry is present. Empty
	// in the happy path: entries only appear on the cancel path.
	mu        sync.Mutex
	cancelled map[*up.ClusterLBCompletion]struct{}
}

func (l *lb) ChooseHost(_ up.ClusterLBHandle, ctx up.ClusterLBContext) (up.HostPtr, *up.ClusterLBCompletion) {
	promise, ok := classify.DecisionKey.GetFromCtx(ctx)
	if !ok {
		return nil, nil
	}

	completion := ctx.NewCompletion()
	promise.OnResolve(func(d classify.Decision) {
		// May be invoked inline on the body/onStreamComplete thread (worker)
		// or inline here if the promise is already resolved. Either way we
		// must run completion.Complete on the cluster main thread.
		l.owner.handle.Schedule(func() { l.complete(completion, d) })
	})
	return nil, completion
}

// complete finalises a host selection on the cluster main thread. It is the
// single place that calls completion.Complete: kept here so the
// cancelled-check, the host lookup and the Complete call cannot race with
// CancelHostSelection.
func (l *lb) complete(completion *up.ClusterLBCompletion, d classify.Decision) {
	l.mu.Lock()
	_, cancelled := l.cancelled[completion]
	if cancelled {
		delete(l.cancelled, completion)
	}
	l.mu.Unlock()
	if cancelled {
		return
	}

	if d.Err != "" {
		completion.Complete(nil, d.Err)
		return
	}
	var host up.HostPtr
	if m := l.owner.hosts.Load(); m != nil {
		host = (*m)[d.Provider]
	}
	if host == nil {
		completion.Complete(nil, "orange.unknown_upstream")
		return
	}
	completion.Complete(host, "")
}

func (l *lb) CancelHostSelection(completion *up.ClusterLBCompletion) {
	l.mu.Lock()
	l.cancelled[completion] = struct{}{}
	l.mu.Unlock()
}

// resolveUpstream parses a URL like "https://api.openai.com" into a resolved
// "ip:port" address suitable for HostSpec.
func resolveUpstream(ctx context.Context, endpoint string) (string, error) {
	host, port, err := splitEndpoint(endpoint)
	if err != nil {
		return "", err
	}
	addrs, err := net.DefaultResolver.LookupIPAddr(ctx, host)
	if err != nil {
		return "", fmt.Errorf("resolve %q: %w", host, err)
	}
	for _, a := range addrs {
		if v4 := a.IP.To4(); v4 != nil {
			return net.JoinHostPort(v4.String(), port), nil
		}
	}
	if len(addrs) > 0 {
		return net.JoinHostPort(addrs[0].IP.String(), port), nil
	}
	return "", fmt.Errorf("resolve %q: no addresses", host)
}

// splitEndpoint extracts host + port from an endpoint URL or "host:port" form.
// "https://api.openai.com" → ("api.openai.com", "443"). Exposed for tests.
func splitEndpoint(endpoint string) (string, string, error) {
	if endpoint == "" {
		return "", "", fmt.Errorf("empty endpoint")
	}
	u, err := url.Parse(endpoint)
	if err != nil {
		return "", "", fmt.Errorf("parse %q: %w", endpoint, err)
	}
	host := u.Hostname()
	port := u.Port()
	if port == "" {
		switch u.Scheme {
		case "https", "":
			port = "443"
		case "http":
			port = "80"
		default:
			return "", "", fmt.Errorf("endpoint %q: unsupported scheme %q", endpoint, u.Scheme)
		}
	}
	if host == "" {
		return "", "", fmt.Errorf("endpoint %q: missing host", endpoint)
	}
	return host, port, nil
}
