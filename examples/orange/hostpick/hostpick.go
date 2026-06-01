// Package hostpick is the cluster extension that picks an upstream host for
// each request.
//
// On Init it resolves every upstream listed in orange.yaml, registers each
// host with the cluster, and remembers the HostPtr by upstream name.
//
// ChooseHost reads the per-request token classify wrote to filter state
// (classify.StateToken), looks up the matching pending.Pending in the
// process-wide registry, and returns an async ClusterLBCompletion. A
// goroutine waits on the Pending; once classify resolves it (from
// classify.bodyHandler after parsing the OpenAI `model` field), the
// goroutine hops back to the cluster's main thread via handle.Schedule and
// calls completion.Complete(host, "").
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
	"github.com/dio/transit/examples/orange/pending"
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

	out := make(map[string]up.HostPtr, len(cfg.Upstreams))
	ctx, cancel := context.WithTimeout(context.Background(), defaultResolveTimeout)
	defer cancel()
	for name, ups := range cfg.Upstreams {
		addr, err := resolveUpstream(ctx, ups.Endpoint)
		if err != nil {
			continue
		}
		// sni metadata lets the cluster's transport_socket_matches pick a per-host
		// UpstreamTlsContext (one per provider hostname). Without this, every host
		// in the cluster would share a single TLS context and we'd be back to the
		// hardcoded-sni demo limitation.
		spec := up.HostSpec{Address: addr}
		if host := ups.Host(); host != "" {
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
	return &lb{owner: c, waiters: make(map[*up.ClusterLBCompletion]struct{})}
}
func (c *cluster) DrainStarted(_ up.ClusterHandle)          {}
func (c *cluster) Shutdown(_ up.ClusterHandle, done func()) { done() }
func (c *cluster) Close()                                   {}

type lb struct {
	up.EmptyClusterLB
	owner *cluster

	// waiters tracks in-flight async ChooseHost completions so
	// CancelHostSelection can mark them dead before the resolver goroutine
	// tries to Complete. Map presence == "still active".
	mu      sync.Mutex
	waiters map[*up.ClusterLBCompletion]struct{}
}

func (l *lb) ChooseHost(_ up.ClusterLBHandle, ctx up.ClusterLBContext) (up.HostPtr, *up.ClusterLBCompletion) {
	token, ok := ctx.GetFilterState(classify.StateToken)
	if !ok || token == "" {
		return nil, nil
	}
	p := pending.Lookup(token)
	if p == nil {
		return nil, nil
	}

	completion := ctx.NewCompletion()
	l.mu.Lock()
	l.waiters[completion] = struct{}{}
	l.mu.Unlock()

	go l.waitAndComplete(p, completion)
	return nil, completion
}

func (l *lb) waitAndComplete(p *pending.Pending, completion *up.ClusterLBCompletion) {
	<-p.Done()

	l.mu.Lock()
	_, alive := l.waiters[completion]
	delete(l.waiters, completion)
	l.mu.Unlock()
	if !alive {
		return
	}

	res, _ := p.Result()
	if res.Err != "" {
		l.owner.handle.Schedule(func() {
			completion.Complete(nil, res.Err)
		})
		return
	}

	var host up.HostPtr
	if m := l.owner.hosts.Load(); m != nil {
		host = (*m)[res.Upstream]
	}
	l.owner.handle.Schedule(func() {
		if host == nil {
			completion.Complete(nil, "orange.unknown_upstream")
			return
		}
		completion.Complete(host, "")
	})
}

func (l *lb) CancelHostSelection(completion *up.ClusterLBCompletion) {
	l.mu.Lock()
	delete(l.waiters, completion)
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
