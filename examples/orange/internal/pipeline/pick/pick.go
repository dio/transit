// Package pick is the dynamic-modules cluster extension that selects an
// upstream host for each request based on the match.Decision stored in the
// per-stream object bag by the orange-match downstream filter.
//
// See README.md for design details: lifecycle, STRICT_DNS-style DNS refresh,
// multi-IP round-robin, cluster main-thread contract, and the async
// body-driven host-selection pattern.
//
// All hosts on the orange-pick cluster are added at runtime via
// ClusterHandle.AddHosts from applyResolved — never via xDS. The custom
// Envoy build enables auto_host_sni and a bounded SNI-scoped TLS session
// cache on the orange-pick upstream (see
// https://gist.github.com/dio/965d1e555909c02013ca882a2b3caa78), so
// runtime-added hosts handshake with their own SNI without xDS supplying
// it. New providers, bindings, or fallback targets are introduced by
// loading them into the config snapshot; do not reach for xDS for this.
package pick

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"math/rand"
	"net"
	"net/url"
	"os"
	"sort"
	"sync/atomic"
	"time"

	"github.com/miekg/dns"

	"github.com/dio/transit/examples/orange/internal/config"
	"github.com/dio/transit/examples/orange/internal/observability"
	"github.com/dio/transit/examples/orange/internal/pipeline/match"
	"github.com/dio/transit/up"
)

const (
	ClusterName = "orange-pick"

	// 5s accommodates Kubernetes ndots search-domain chaining, which can add
	// multiple failing lookups before the real hostname is tried.
	defaultResolveTimeout = 5 * time.Second

	// defaultDNSRefreshInterval is the fallback wake interval used by the
	// refresh loop when no hosts are registered yet (e.g. all Init resolves
	// failed). In the normal path the loop wakes at the earliest TTL expiry.
	defaultDNSRefreshInterval = 30 * time.Second

	// minTTLFloor clamps pathologically short TTLs and sets the retry delay
	// after a transient DNS failure so we don't hammer the resolver.
	minTTLFloor = 10 * time.Second
)

func init() {
	Register(observability.Logger("orange/pick"))
}

// Register registers the pick cluster factory. Pass a non-nil logger to
// override the default (Orange component logger with component="orange/pick").
// Call before Envoy initializes clusters; init() registers with nil as default.
func Register(logger *slog.Logger) {
	up.RegisterCluster(ClusterName, &factory{logger: logger})
}

type factory struct{ logger *slog.Logger }

func (f factory) Create(_ []byte) (up.ClusterConfigFactory, error) {
	return &cfgFactory{logger: f.logger}, nil
}

type cfgFactory struct{ logger *slog.Logger }

func (f cfgFactory) NewCluster(h up.ClusterHandle) up.Cluster {
	return &cluster{handle: h, logger: f.logger}
}
func (cfgFactory) Close() {}

// resolvedUpstream holds all resolved IPs for one provider plus their HostPtrs
// and the time at which the DNS TTL expires. The refresh loop wakes at the
// earliest nextRefresh across all upstreams rather than on a fixed interval.
//
// rr is an atomic round-robin counter; lookupHost increments it on every call
// and selects ptrs[rr % len(ptrs)], distributing requests across all IPs.
// resolvedUpstream is always heap-allocated (stored as *resolvedUpstream) so
// rr can be mutated without copying the struct (atomic.Uint64 is noCopy).
type resolvedUpstream struct {
	addrs       []string
	ptrs        []up.HostPtr
	nextRefresh time.Time
	rr          atomic.Uint64
}

type cluster struct {
	handle        up.ClusterHandle
	stopConfig    func()
	stopFileWatch func()
	stopRefresh   context.CancelFunc
	logger        *slog.Logger

	// resolveFunc is called by resolveAll for each provider endpoint. Nil means
	// use the package-level resolveUpstream (real DNS). Set in tests to avoid
	// network calls.
	resolveFunc func(ctx context.Context, endpoint string) (addrs []string, ttl time.Duration, err error)

	// hosts is the source of truth ChooseHost reads from. Published atomically
	// after each resolveAll call to keep ChooseHost lock-free. Values are
	// pointers so the per-entry rr counter survives map republishes.
	hosts atomic.Pointer[map[string]*resolvedUpstream]
}

func (c *cluster) Init(h up.ClusterHandle) {
	c.handle = h
	if c.logger == nil {
		c.logger = observability.Logger("orange/pick")
	}
	config.EnsureLogger()

	ctx, cancel := context.WithTimeout(context.Background(), defaultResolveTimeout)
	defer cancel()
	// Init is called on the main thread, so resolveAddrs (DNS) + applyResolved
	// (host mutations) can run in sequence here without scheduling.
	c.applyResolved(h, c.resolveAddrs(ctx))
	h.PreInitComplete()
}

func (c *cluster) ServerInitialized(h up.ClusterHandle) {
	c.stopConfig = config.Start(context.Background())

	// Enable file watching for the orange config file to trigger immediate refreshes.
	c.stopFileWatch = config.EnableFileWatch(os.Getenv(config.EnvVar))

	ctx, cancel := context.WithCancel(context.Background())
	c.stopRefresh = cancel
	go c.refreshLoop(ctx, h)
}

// refreshLoop wakes at the earliest nextRefresh across all registered upstreams,
// resolves DNS on the goroutine, then schedules host reconciliation on the main
// thread (AddHosts/RemoveHosts/UpdateHostHealth must not be called off-thread).
func (c *cluster) refreshLoop(ctx context.Context, h up.ClusterHandle) {
	for {
		delay := time.Until(c.earliestNextRefresh())
		if delay < minTTLFloor {
			delay = minTTLFloor
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(delay):
			rctx, cancel := context.WithTimeout(ctx, defaultResolveTimeout)
			addrs := c.resolveAddrs(rctx)
			cancel()
			done := make(chan struct{})
			c.handle.Schedule(func() {
				c.applyResolved(h, addrs)
				close(done)
			})
			select {
			case <-ctx.Done():
				return
			case <-done:
			}
		}
	}
}

// earliestNextRefresh returns the soonest nextRefresh across all registered
// upstreams, or now+defaultDNSRefreshInterval when the host map is empty.
func (c *cluster) earliestNextRefresh() time.Time {
	m := c.hosts.Load()
	if m == nil || len(*m) == 0 {
		return time.Now().Add(defaultDNSRefreshInterval)
	}
	var earliest time.Time
	for _, r := range *m {
		if r == nil {
			continue
		}
		if earliest.IsZero() || r.nextRefresh.Before(earliest) {
			earliest = r.nextRefresh
		}
	}
	return earliest
}

// resolve delegates to c.resolveFunc when set, otherwise calls resolveUpstream.
func (c *cluster) resolve(ctx context.Context, endpoint string) ([]string, time.Duration, error) {
	if c.resolveFunc != nil {
		return c.resolveFunc(ctx, endpoint)
	}
	return resolveUpstream(ctx, endpoint)
}

// lookupHost maps a match.Decision to a HostPtr for the selected provider,
// distributing requests across all registered IPs via round-robin.
// It is the only orange-specific logic in the cluster LB hot path; all
// completion lifecycle concerns are owned by up.AsyncHostSelector.
func (c *cluster) lookupHost(d match.Decision) up.HostResult {
	if d.Err != "" {
		return up.HostResult{ErrDetail: d.Err}
	}
	if m := c.hosts.Load(); m != nil {
		if r := (*m)[d.ProviderBackend]; r != nil && len(r.ptrs) > 0 {
			idx := r.rr.Add(1) % uint64(len(r.ptrs))
			return up.HostResult{Host: r.ptrs[idx]}
		}
	}
	return up.HostResult{ErrDetail: "orange.unknown_upstream"}
}

// dnsResult holds the outcome of one DNS resolution attempt, plus the
// provider hostname needed to build a HostSpec if the host set changes.
type dnsResult struct {
	addrs    []string
	hostname string // p.Host(), captured at resolve time
	ttl      time.Duration
	err      error
}

// resolveAddrs performs DNS resolution for every LLM provider and MCP server in
// the current config. It is safe to call from any goroutine; it never touches
// the cluster handle.
func (c *cluster) resolveAddrs(ctx context.Context) map[string]dnsResult {
	cfg := config.Get()
	out := make(map[string]dnsResult, len(cfg.Providers))
	for name, p := range cfg.Providers {
		addrs, ttl, err := c.resolve(ctx, p.Endpoint)
		out[name] = dnsResult{addrs: addrs, hostname: p.Host(), ttl: ttl, err: err}
	}
	if cfg.MCP != nil {
		for name, s := range cfg.MCP.Servers {
			addrs, ttl, err := c.resolve(ctx, s.Endpoint)
			out[name] = dnsResult{addrs: addrs, hostname: s.Host(), ttl: ttl, err: err}
		}
	}
	return out
}

// applyResolved reconciles the cluster host set from pre-resolved DNS results.
// Must be called on the cluster main thread (AddHosts/RemoveHosts/UpdateHostHealth
// are main-thread-only per the ClusterHandle contract).
//
// Each provider may resolve to multiple A-record IPs; all are registered so the
// round-robin in lookupHost can distribute across them. Reconciliation rules:
//   - resolve fails    → keep existing entry, reset nextRefresh to now+minTTLFloor
//   - IP set unchanged → keep existing HostPtrs and rr counter, update nextRefresh
//   - IP added         → AddHosts for new addr, retain ptrs for unchanged addrs
//   - IP removed       → RemoveHosts for gone addr
//   - new provider     → AddHosts for all resolved addrs
//   - provider deleted → RemoveHosts for all its ptrs
func (c *cluster) applyResolved(h up.ClusterHandle, resolved map[string]dnsResult) {
	current := c.hosts.Load()
	out := make(map[string]*resolvedUpstream, len(resolved))

	for name, r := range resolved {
		if r.err != nil {
			c.logger.Warn("skipping upstream: resolve failed", "upstream", name, "err", r.err)
			// Preserve the existing entry so a DNS hiccup doesn't pull a healthy
			// host, but schedule a retry soon.
			if current != nil {
				if old, ok := (*current)[name]; ok {
					entry := &resolvedUpstream{addrs: old.addrs, ptrs: old.ptrs, nextRefresh: time.Now().Add(retryDelay())}
					entry.rr.Store(old.rr.Load())
					out[name] = entry
				}
			}
			continue
		}

		ttl := max(r.ttl, minTTLFloor)
		nextRefresh := time.Now().Add(ttl + jitter(minTTLFloor))

		// Build a map of existing addr -> HostPtr for this provider.
		existing := map[string]up.HostPtr{}
		var oldRR uint64
		var old *resolvedUpstream
		if current != nil {
			if o, ok := (*current)[name]; ok {
				old = o
				for i, addr := range old.addrs {
					existing[addr] = old.ptrs[i]
				}
				oldRR = old.rr.Load()
			}
		}

		// Remove IPs that are no longer in the DNS result.
		newAddrSet := make(map[string]struct{}, len(r.addrs))
		for _, addr := range r.addrs {
			newAddrSet[addr] = struct{}{}
		}

		// IP set unchanged (order-independent) — just extend the refresh deadline.
		if old != nil && len(existing) == len(newAddrSet) {
			unchanged := true
			for addr := range existing {
				if _, ok := newAddrSet[addr]; !ok {
					unchanged = false
					break
				}
			}
			if unchanged {
				entry := &resolvedUpstream{addrs: old.addrs, ptrs: old.ptrs, nextRefresh: nextRefresh}
				entry.rr.Store(oldRR)
				out[name] = entry
				continue
			}
		}

		for addr, ptr := range existing {
			if _, keep := newAddrSet[addr]; !keep {
				h.RemoveHosts([]up.HostPtr{ptr})
				c.logger.Debug("upstream addr removed", "upstream", name, "addr", addr)
			}
		}

		// Build the new ptrs slice, registering any IPs not yet in the cluster.
		ptrs := make([]up.HostPtr, 0, len(r.addrs))
		for _, addr := range r.addrs {
			if ptr, ok := existing[addr]; ok {
				ptrs = append(ptrs, ptr)
			} else {
				added := h.AddHosts([]up.HostSpec{{
					Address:  addr,
					Hostname: r.hostname,
					Metadata: map[string]string{"sni": r.hostname},
				}})
				if len(added) == 0 {
					c.logger.Warn("skipping addr: AddHosts returned no ptrs", "upstream", name, "addr", addr)
					continue
				}
				h.UpdateHostHealth(added[0], up.HostHealthy)
				ptrs = append(ptrs, added[0])
				c.logger.Debug("upstream addr added", "upstream", name, "addr", addr)
			}
		}

		if len(ptrs) == 0 {
			c.logger.Warn("skipping upstream: no valid addrs after reconcile", "upstream", name)
			continue
		}

		entry := &resolvedUpstream{addrs: r.addrs, ptrs: ptrs, nextRefresh: nextRefresh}
		entry.rr.Store(oldRR)
		out[name] = entry
	}

	// Remove providers deleted from config.
	if current != nil {
		for name, old := range *current {
			if _, kept := out[name]; !kept {
				h.RemoveHosts(old.ptrs)
				c.logger.Info("upstream removed from config", "upstream", name, "addrs", old.addrs)
			}
		}
	}

	c.hosts.Store(&out)
}

func (c *cluster) NewClusterLB() up.ClusterLB {
	log := c.logger
	sel := up.NewAsyncHostSelector(
		c.handle,
		match.DecisionKey,
		c.lookupHost,
		up.SelectorObserver{
			OnSelected:       func(host up.HostPtr) { log.Debug("host selected", "host", host) },
			OnFailed:         func(errDetail string) { log.Warn("host selection failed", "err", errDetail) },
			OnCancelled:      func() { log.Debug("host selection cancelled") },
			OnMissingPromise: func() { log.Warn("no match promise in stream bag") },
		},
	)
	return &lb{
		sel:    sel,
		lookup: c.lookupHost,
		log:    log,
	}
}

func (c *cluster) DrainStarted(_ up.ClusterHandle) {} // no in-flight requests to drain
func (c *cluster) Shutdown(_ up.ClusterHandle, done func()) {
	if c.stopRefresh != nil {
		c.stopRefresh()
	}
	if c.stopFileWatch != nil {
		c.stopFileWatch()
	}
	if c.stopConfig != nil {
		c.stopConfig()
	}
	done()
}
func (c *cluster) Close() {} // required by up.Cluster; Shutdown handles teardown

type lb struct {
	up.EmptyClusterLB
	sel    *up.AsyncHostSelector[match.Decision]
	lookup func(match.Decision) up.HostResult
	log    *slog.Logger
}

func (l *lb) ChooseHost(h up.ClusterLBHandle, ctx up.ClusterLBContext) (up.HostPtr, *up.ClusterLBCompletion) {
	if ctx != nil {
		if provider, ok := ctx.GetFilterState(match.StateUpstream); ok && provider != "" {
			res := l.lookup(match.Decision{ProviderBackend: provider})
			if res.ErrDetail != "" {
				if l.log != nil {
					l.log.Warn("host selection failed", "err", res.ErrDetail, "provider", provider)
				}
				completion := ctx.NewCompletion()
				if completion != nil {
					completion.Complete(nil, res.ErrDetail)
					return nil, completion
				}
				return nil, nil
			}
			if l.log != nil {
				l.log.Debug("host selected", "host", res.Host, "provider", provider)
			}
			return res.Host, nil
		}
	}
	return l.sel.ChooseHost(h, ctx)
}

func (l *lb) CancelHostSelection(completion *up.ClusterLBCompletion) {
	l.sel.Cancel(completion)
}

// resolveUpstream parses an endpoint URL, resolves the hostname to IP addresses
// via DNS, and returns all "ip:port" strings plus the minimum TTL observed in
// the A-record answer. Multiple IPs are returned when DNS returns multiple A
// records; all are registered as hosts so lookupHost can round-robin among them.
func resolveUpstream(ctx context.Context, endpoint string) (addrs []string, ttl time.Duration, err error) {
	host, port, err := splitEndpoint(endpoint)
	if err != nil {
		return nil, 0, err
	}
	ipAddrs, ttl, err := lookupWithTTL(ctx, host)
	if err != nil {
		return nil, 0, fmt.Errorf("resolve %q: %w", host, err)
	}
	result := pickAddrs(ipAddrs, port)
	if len(result) == 0 {
		return nil, 0, fmt.Errorf("resolve %q: no addresses", host)
	}
	return result, ttl, nil
}

// pickAddrs sorts addrs by byte value so DNS round-robin rotation yields a
// stable result, then returns all addresses as "ip:port" strings. Sorting
// ensures applyResolved detects IP-set changes by slice equality rather than
// permutation. Since lookupWithTTL only queries TypeA, all addresses are IPv4.
func pickAddrs(addrs []net.IPAddr, port string) []string {
	sort.Slice(addrs, func(i, j int) bool {
		return bytes.Compare(addrs[i].IP, addrs[j].IP) < 0
	})
	out := make([]string, len(addrs))
	for i, a := range addrs {
		out[i] = net.JoinHostPort(a.IP.String(), port)
	}
	return out
}

// fallbackDNSConfig is used when /etc/resolv.conf is missing or unreadable
// (e.g. scratch/distroless containers). It points at well-known public resolvers
// that are reachable from any internet-connected host.
var fallbackDNSConfig = &dns.ClientConfig{
	Servers: []string{"8.8.8.8", "1.1.1.1"},
	Port:    "53",
}

// lookupWithTTL queries A records for host using the system resolver config
// and returns the resolved addresses together with the minimum TTL from the
// answer section. The TTL is used to schedule the next re-resolve.
func lookupWithTTL(ctx context.Context, host string) ([]net.IPAddr, time.Duration, error) {
	conf, err := dns.ClientConfigFromFile("/etc/resolv.conf")
	if err != nil {
		slog.Warn("orange/pick: /etc/resolv.conf unavailable, falling back to public resolvers", "err", err)
		conf = fallbackDNSConfig
	}

	c := &dns.Client{}
	m := new(dns.Msg)
	m.SetQuestion(dns.Fqdn(host), dns.TypeA)
	m.RecursionDesired = true

	var lastErr error
	for _, server := range conf.Servers {
		r, _, err := c.ExchangeContext(ctx, m, net.JoinHostPort(server, conf.Port))
		if err != nil {
			lastErr = err
			continue
		}
		if r.Rcode != dns.RcodeSuccess {
			lastErr = fmt.Errorf("DNS rcode %s for %q", dns.RcodeToString[r.Rcode], host)
			continue
		}

		var addrs []net.IPAddr
		minTTL := ^uint32(0)
		for _, rr := range r.Answer {
			if a, ok := rr.(*dns.A); ok {
				addrs = append(addrs, net.IPAddr{IP: a.A})
				if a.Hdr.Ttl < minTTL {
					minTTL = a.Hdr.Ttl
				}
			}
		}
		if len(addrs) == 0 {
			lastErr = fmt.Errorf("no A records for %q", host)
			continue
		}
		return addrs, time.Duration(minTTL) * time.Second, nil
	}

	if lastErr != nil {
		return nil, 0, lastErr
	}
	return nil, 0, fmt.Errorf("all DNS servers exhausted for %q", host)
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
		// Accepted no-port forms: "https://host" (443), "http://host" (80).
		// A bare "host" (no scheme, no port) also reaches here with scheme ""
		// and defaults to 443; "host:port" without a scheme parses as Opaque
		// and will fail the host == "" check below.
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

// retryDelay returns a jittered retry interval for a failed DNS resolve:
// minTTLFloor + rand[0, minTTLFloor). Spreading retries over a [10s, 20s)
// window prevents instances that synchronised on a failure from storming the
// in-cluster DNS server when it recovers.
func retryDelay() time.Duration {
	// #nosec G404 — jitter is not security-sensitive
	return minTTLFloor + time.Duration(rand.Int63n(int64(minTTLFloor)))
}

// jitter returns a random duration in [0, spread) to add to a TTL-based
// nextRefresh, desynchronising instances that resolved the same hostname at
// the same time.
func jitter(spread time.Duration) time.Duration {
	// #nosec G404 — jitter is not security-sensitive
	return time.Duration(rand.Int63n(int64(spread)))
}
